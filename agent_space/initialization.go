package main

import (
	"context"
	"log"
	"os"
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
	// shared by the vector store and the incident-context resolver
	pool       *pgxpool.Pool
	sqsWorker  *worker.Worker
	sqsResults *worker.Store
	repos      *remediation.Repositories
)

const schemaLoadTimeout = 60 * time.Second

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
	if sreAgent == nil {
		log.Println("Worker: SQS_QUEUE_URL is set but the agent is unavailable (no MCP); not polling.")
		return
	}

	client, err := queue.New(context.Background(), cfg)
	if err != nil {
		log.Printf("Worker: could not build the SQS client, not polling: %v", err)
		return
	}

	// verdicts outlive the process when the journal is available. When it is
	// not, the worker still runs: it loses durability, not the ability to
	// investigate, and saying so is better than refusing to start.
	sqsResults = worker.NewStore()
	if journal, err := worker.NewPGJournal(context.Background(), pool); err != nil {
		log.Printf("Worker: verdicts will be in-memory only, lost on restart: %v", err)
	} else {
		sqsResults = worker.NewDurableStore(journal)
		log.Println("Worker: verdicts persist to the investigations table and survive a restart.")
	}

	sqsWorker = &worker.Worker{
		Queue:    client,
		Resolver: incident.NewResolver(pool),
		Agent:    sreAgent,
		Results:  sqsResults,
		Config:   cfg,
	}

	log.Printf("Worker: assignments from %s", client.Endpoint())
}
