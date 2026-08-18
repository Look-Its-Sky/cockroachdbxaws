package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/vectorstores"

	"agent_space/agent"
	"agent_space/incident"
	"agent_space/remediation"
	"agent_space/utils"
	"agent_space/utils/crdbvector"
	"agent_space/utils/mcp"
	"agent_space/utils/queue"
	"agent_space/worker"
)

var (
	store    vectorstores.VectorStore
	model    llms.Model
	mcpSess  *mcp.Session
	sreAgent *agent.Runner

	pool         *pgxpool.Pool
	analysisPool *pgxpool.Pool
	sqsWorker    *worker.Worker
	sqsResults   *worker.Store
	repos        *remediation.Repositories
	remediator   *remediation.Runner
	solutions    *remediation.Solutions
	publisher    *remediation.Publisher
	precedents   *remediation.Precedents
)

const schemaLoadTimeout = 60 * time.Second

// slack past a run's own ceiling before a "running" row is treated as
// abandoned, so one finishing at its deadline is not reclaimed under itself
const staleSlack = 5 * time.Minute

func initStore() {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		log.Fatal("DATABASE_URL is not set in the environment or .env file! Refusing to start.")
	}

	embedder, err := utils.GetEmbedder()
	if err != nil {
		log.Fatalf("Failed to initialize embedder: %v", err)
	}

	log.Printf("Vector store: connecting to %s", utils.RedactURL(connStr))

	ctx := context.Background()

	// connect to the database
	pool, err = pgxpool.New(ctx, connStr)
	if err != nil {
		log.Fatalf("Failed to build the connection pool for %s: %v", utils.RedactURL(connStr), err)
	}

	// init vector store with pool and embedder
	store, err = crdbvector.New(
		ctx,
		crdbvector.WithConn(pool),
		crdbvector.WithEmbedder(embedder),
		crdbvector.WithVectorDimensions(utils.VectorDimensions()),
	)

	if err != nil {
		log.Fatalf("Failed to initialize vector store at %s: %v\n"+
			"  Running the local stack? Start it with\n"+
			"    docker compose -f docker-compose.local.yml up -d\n"+
			"  and point DATABASE_URL at the published port:\n"+
			"    CockroachDB: postgresql://root@localhost:26257/defaultdb?sslmode=disable\n"+
			"    PostgreSQL:  postgres://postgres:postgres@localhost:5432/agent_space?sslmode=disable",
			utils.RedactURL(connStr), err)
	}
}

// the index of decisions engineers have made, in its own collection on the same
// tables: the store's filters are equality-only, so decisions living beside the
// incidents would quietly take recall slots from the incident history
func initPrecedents() {
	embedder, err := utils.GetEmbedder()
	if err != nil {
		log.Printf("Decisions: no embedder, so decisions will be recorded but never recalled: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), schemaLoadTimeout)
	defer cancel()

	decisionStore, err := crdbvector.New(ctx,
		crdbvector.WithConn(pool),
		crdbvector.WithEmbedder(embedder),
		crdbvector.WithVectorDimensions(utils.VectorDimensions()),
		crdbvector.WithCollectionName(remediation.DecisionCollection),
	)
	if err != nil {
		log.Printf("Decisions: index unavailable, so decisions will be recorded but never recalled: %v", err)
		return
	}

	precedents = remediation.NewPrecedents(decisionStore, pool, crdbvector.DefaultEmbeddingStoreTableName)
	log.Printf("Decisions: past picks are recalled from the %q collection, %d at a time",
		remediation.DecisionCollection, remediation.MaxPrecedents)
}

func initLLM() {
	var err error
	if model, err = utils.GetLLM(); err != nil {
		log.Fatalf("Failed to initialize LLM: %v", err)
	}

	log.Printf("LLM: %s", utils.DescribeLLM())
}

func initMCP() {
	cfg := mcp.ConfigFromEnv()
	if !cfg.Configured() {
		log.Fatal("COCKROACH_API_KEY is not set.")
	}

	session, err := mcp.Connect(context.Background(), cfg)
	if err != nil {
		log.Printf("MCP: connection failed, /agent will be unavailable: %v", err)
		return
	}

	mcpSess = session
	agentTools := session.Tools()
	defer func() {
		// nil is a working value: it means no decision has ever informed a
		// verdict, which is exactly how this ran before decisions existed
		if sreAgent != nil && precedents != nil {
			sreAgent.Precedents = precedents
		}
	}()

	if utils.EnvBool("AGENT_ALLOW_WRITE_TOOLS") {
		log.Println("MCP: AGENT_ALLOW_WRITE_TOOLS is set; the agent can modify the cluster.")
	} else {
		agentTools = mcp.ReadOnly(agentTools)
	}

	// drop the cluster-introspection tools the prompt already tells the model to avoid.
	excluded := mcp.DefaultExcluded
	if raw, set := os.LookupEnv("AGENT_EXCLUDE_TOOLS"); set {
		excluded = strings.Split(raw, ",")
	}

	agentTools = mcp.Exclude(agentTools, excluded)
	sreAgent = agent.New(model, store, agentTools)

	// cache schem for prompting
	schemaCtx, cancelSchema := context.WithTimeout(context.Background(), schemaLoadTimeout)
	defer cancelSchema()

	if schema, err := agent.LoadClusterSchema(schemaCtx, agentTools, nil); err != nil {
		log.Printf("MCP: could not read the cluster schema, the agent will discover it per run: %v", err)
	} else {
		sreAgent.Schema = schema
		log.Printf("MCP: cluster schema loaded (%d chars); the agent starts knowing the tables", len(schema))
	}

	scope := session.ClusterID()
	if scope == "" {
		scope = "organization-wide"
	}

	offered := make([]string, 0, len(agentTools))
	for _, t := range agentTools {
		offered = append(offered, t.Name())
	}

	log.Printf("MCP: connected to %s (scope: %s); discovered %d tools, offering %d to the agent: %s",
		session.Endpoint(), scope, len(session.Tools()), len(offered), strings.Join(offered, ", "))
}

// the service-to-repository mapping. Created here rather than in a SQL script
// so that resetting the demo data cannot take the configuration with it.
func initRepositories() {
	r, err := remediation.NewRepositories(context.Background(), pool)
	if err != nil {
		log.Printf("Remediation: repository mapping unavailable, fixes cannot be proposed: %v", err)
		return
	}
	repos = r

	mapped, err := r.List(context.Background())
	if err != nil {
		log.Printf("Remediation: could not list repositories: %v", err)
		return
	}
	if len(mapped) == 0 {
		log.Println("Remediation: no services are mapped to a repository. Apply one with " +
			"`go run ./cmd/seed -file scripts/repos.sql`.")
		return
	}

	for _, repo := range mapped {
		verified := "build only"
		if repo.HasTests() {
			verified = "build and tests"
		}
		log.Printf("Remediation: %s -> %s/%s (%s, %s)",
			repo.ServiceID, repo.Redacted(), repo.Subdirectory, repo.RuntimeImage, verified)
	}
}

// the SQS consumer. Like MCP, this is optional: an unset SQS_QUEUE_URL leaves
// the HTTP API exactly as it was rather than refusing to boot.
func initWorker() {
	cfg := queue.ConfigFromEnv()
	if !cfg.Configured() {
		log.Println("Worker: SQS_QUEUE_URL is not set; investigations can only be started over HTTP.")
		return
	}
	if !cfg.BoundaryConfigured() {
		log.Println("Worker: SQS_QUEUE_URL is set but AGENT_TENANT_ID or AGENT_CLASSIFICATION is missing or invalid; not polling.")
		return
	}
	if sreAgent == nil {
		log.Println("Worker: SQS_QUEUE_URL is set but the agent is unavailable (no MCP); not polling.")
		return
	}
	analysisURL := strings.TrimSpace(os.Getenv("ANALYSIS_DATABASE_URL"))
	if analysisURL == "" {
		log.Println("Worker: SQS_QUEUE_URL is set but ANALYSIS_DATABASE_URL is missing; not polling.")
		return
	}
	if analysisURL == strings.TrimSpace(os.Getenv("DATABASE_URL")) {
		log.Println("Worker: ANALYSIS_DATABASE_URL must be a separate read-only analysis database connection; not polling.")
		return
	}
	var err error
	analysisPool, err = pgxpool.New(context.Background(), analysisURL)
	if err != nil {
		log.Printf("Worker: could not build the analysis context pool for %s; not polling: %v", utils.RedactURL(analysisURL), err)
		return
	}
	pingCtx, cancelPing := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelPing()
	if err := analysisPool.Ping(pingCtx); err != nil {
		analysisPool.Close()
		analysisPool = nil
		log.Printf("Worker: analysis context database %s is unavailable; not polling: %v", utils.RedactURL(analysisURL), err)
		return
	}

	client, err := queue.New(context.Background(), cfg)
	if err != nil {
		log.Printf("Worker: could not build the SQS client, not polling: %v", err)
		return
	}

	// without a journal the worker still runs; it loses durability, not the
	// ability to investigate
	sqsResults = worker.NewStore()
	if journal, err := worker.NewPGJournal(context.Background(), pool); err != nil {
		log.Printf("Worker: verdicts will be in-memory only, lost on restart: %v", err)
	} else {
		sqsResults = worker.NewDurableStore(journal)
		log.Println("Worker: verdicts persist to the investigations table and survive a restart.")
		reclaimInvestigations(journal, cfg.RunTimeout)
	}

	sqsWorker = &worker.Worker{
		Queue:    client,
		Resolver: incident.NewAnalysisResolver(analysisPool, cfg.Boundary()),
		Agent:    sreAgent,
		Results:  sqsResults,
		Config:   cfg,
	}

	// nil when there is no container runtime, which leaves the worker doing
	// exactly what it did before: investigate, record a verdict, acknowledge
	if remediator != nil {
		sqsWorker.Remediation = remediator
	}

	log.Printf("Worker: assignments from %s for region=%s tenant=%s classification=%s",
		client.Endpoint(), cfg.Region, cfg.TenantID, cfg.Classification)
}

// the remediation pipeline: triage, candidate fixes, verification, draft PRs.
// Every prerequisite is optional, so a laptop with no container runtime still
// boots with the investigation half working.
func initRemediation() {
	// Read models remain available even when execution is explicitly disabled
	// or a sandbox prerequisite is missing. This lets an operator distinguish
	// "nothing ran" from "the API is unavailable" without granting write or
	// container authority.
	if s, err := remediation.NewSolutions(context.Background(), pool); err != nil {
		log.Printf("Remediation: candidates will not be stored, so the UI cannot read them back: %v", err)
	} else {
		solutions = s
		reclaimRemediations(s)
	}

	// unset means on: the prerequisites below already degrade, so this exists
	// only to turn it off deliberately on a machine that could run it
	if raw, set := os.LookupEnv("REMEDIATION_ENABLED"); set && !utils.EnvBool("REMEDIATION_ENABLED") {
		log.Printf("Remediation: REMEDIATION_ENABLED=%q; verdicts will not be turned into fixes.", raw)
		return
	}
	if repos == nil {
		log.Println("Remediation: no repository mapping, so fixes cannot be proposed.")
		return
	}

	sandbox := &remediation.ContainerSandbox{
		Binary:     utils.EnvOr("CONTAINER_BINARY", "docker"),
		NamePrefix: "sre-agent",
	}

	// checked at boot on purpose: finding out that docker is missing twenty
	// minutes into an incident is the expensive way to learn it
	if err := sandbox.Available(context.Background()); err != nil {
		log.Printf("Remediation: %v", err)
		log.Println("Remediation: no container runtime, so no fixes will be proposed. " +
			"Investigations are unaffected. Set CONTAINER_BINARY=podman if that is what you run.")
		return
	}

	publisher = remediation.NewPublisher(os.Getenv("GITHUB_TOKEN"))
	if !publisher.Configured() {
		log.Println("Remediation: GITHUB_TOKEN is not set; fixes will be proposed and verified, " +
			"but picking one cannot open a draft pull request.")
	}

	remediator = &remediation.Runner{
		Repos:   repos,
		Sandbox: sandbox,
		Model:   model,
		Store:   solutions,
		// only used to clone. The demo repository is public, so this is
		// normally empty and no credential enters the container at all.
		GitHubToken:          os.Getenv("GITHUB_TOKEN"),
		Precedents:           precedents,
		JobConcurrency:       envInt("REMEDIATION_JOBS", 0),
		CandidateConcurrency: envInt("REMEDIATION_CANDIDATE_CONCURRENCY", 0),
		// -1 turns repair off, which envInt cannot express: it reads anything
		// not positive as "use the default"
		MaxRepairs: repairRounds(),
	}
}

// mark investigations a previous process left running. Age-gated on the run's
// own ceiling, so a second process polling the same queue is unaffected;
// recovering the work is Seen's job, this only stops a dead run reading as live.
func reclaimInvestigations(journal *worker.PGJournal, runTimeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), schemaLoadTimeout)
	defer cancel()

	n, err := journal.AbandonStale(ctx, runTimeout+staleSlack)
	if err != nil {
		log.Printf("Worker: could not reconcile investigations left running: %v", err)
		return
	}
	if n > 0 {
		log.Printf("Worker: %d investigation(s) were left running by a previous process, and are now failed.", n)
	}
}

// mark remediations a previous process left running. Unconditional: nothing
// else could have been running one, and no redelivery will recover it.
func reclaimRemediations(s *remediation.Solutions) {
	ctx, cancel := context.WithTimeout(context.Background(), schemaLoadTimeout)
	defer cancel()

	n, err := s.AbandonStale(ctx)
	if err != nil {
		log.Printf("Remediation: could not reconcile runs left running: %v", err)
		return
	}
	if n > 0 {
		log.Printf("Remediation: %d run(s) were left running by a previous process, and are now failed.", n)
	}
}

// how many repair rounds a candidate gets; an explicit 0 means none, which is
// why this cannot go through envInt
func repairRounds() int {
	raw := strings.TrimSpace(os.Getenv("REMEDIATION_MAX_REPAIRS"))
	if raw == "" {
		return 0
	}

	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		log.Printf("Remediation: REMEDIATION_MAX_REPAIRS=%q is not a non-negative integer; using the default", raw)
		return 0
	}
	if n == 0 {
		// the Runner's own "off" value, since 0 there means "use the default"
		return -1
	}
	return n
}

// a positive integer from the environment, or fallback. Zero is meaningful
// downstream: it means "use the package's own default".
func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}

	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		log.Printf("Remediation: %s=%q is not a positive integer; ignoring it", key, raw)
		return fallback
	}
	return n
}
