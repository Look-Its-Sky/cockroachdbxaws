package main

// Boot-time wiring, kept out of main.go so that file is just routes.
//
// This lives in package main rather than in utils because utils/mcp imports
// utils, so a utils file importing utils/mcp is an import cycle:
//
//	imports agent_space/utils/mcp from initialization.go
//	imports agent_space/utils from client.go: import cycle not allowed
//
// The same applies via agent, which imports utils/mcp. Bootstrap code depends
// on everything, so it belongs at the top of the graph, not inside a leaf
// package that the rest of the tree depends on.

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
	"agent_space/utils"
	"agent_space/utils/crdbvector"
	"agent_space/utils/mcp"
)

var (
	store    vectorstores.VectorStore
	model    llms.Model
	mcpSess  *mcp.Session
	sreAgent *agent.Runner
)

// schemaLoadTimeout bounds the boot-time schema read. Generous because a Cloud
// Basic cluster can be cold, and cheap to lose: on timeout the agent just
// discovers the schema per run.
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

	// Log the target before dialling: "connection refused" is otherwise
	// indistinguishable from pointing at the wrong host entirely.
	log.Printf("Vector store: connecting to %s", utils.RedactURL(connStr))

	ctx := context.Background()

	// A pool, not crdbvector's WithConnectionURL — that calls pgx.Connect and
	// opens exactly one connection, and when it dies every later request
	// returns "conn closed" until the process restarts. A pool reconnects on
	// demand, which is what a long-lived container needs.
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		log.Fatalf("Failed to build the connection pool for %s: %v", utils.RedactURL(connStr), err)
	}

	store, err = crdbvector.New(
		ctx,
		crdbvector.WithConn(pool),
		crdbvector.WithEmbedder(embedder),
		// Size the vector column to the embedder so a model swap fails loudly
		// at insert time instead of silently corrupting similarity search.
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

// initLLM resolves the chat model at boot so a bad key fails here rather than
// on the first request.
func initLLM() {
	var err error
	if model, err = utils.GetLLM(); err != nil {
		log.Fatalf("Failed to initialize LLM: %v", err)
	}
	// Which provider actually won is the first thing you want to know when a
	// local endpoint is not being picked up.
	log.Printf("LLM: %s", utils.DescribeLLM())
}

// initMCP connects to the CockroachDB Cloud Managed MCP Server and builds the
// agent over the tools it advertises.
//
// Missing credentials are fatal. The Cloud Managed MCP Server is the second of
// the two CockroachDB integrations this service exists to demonstrate, so
// starting without it would serve an API that looks healthy on /ping while the
// headline route is permanently 503 — a failure discovered by whoever calls
// /agent first, rather than by whoever started the process.
//
// A connection that fails despite being configured is treated differently, and
// deliberately so: that is the endpoint being unreachable rather than the
// operator forgetting something, and /store, /retrieve and /ask do not depend
// on MCP. The service stays up, and a nil sreAgent makes /tools and /agent
// answer 503 with the reason.
func initMCP() {
	cfg := mcp.ConfigFromEnv()
	if !cfg.Configured() {
		log.Fatal("COCKROACH_API_KEY is not set. Refusing to start: the CockroachDB Cloud\n" +
			"  Managed MCP Server is what /agent runs on, and without it the route can only\n" +
			"  ever return 503.\n" +
			"    Cloud:       set COCKROACH_API_KEY to a service account key, and\n" +
			"                 COCKROACH_CLUSTER_ID to the cluster it may read.\n" +
			"    Local stack: COCKROACH_API_KEY=local-dev-token-insecure with\n" +
			"                 COCKROACH_MCP_URL=http://localhost:8443")
	}

	session, err := mcp.Connect(context.Background(), cfg)
	if err != nil {
		log.Printf("MCP: connection failed, /agent will be unavailable: %v", err)
		return
	}

	mcpSess = session

	// The agent diagnoses; it does not remediate. Withholding the write tools
	// costs it nothing it needs and removes the possibility that a misread
	// instruction turns into a DELETE.
	agentTools := session.Tools()
	if utils.EnvBool("AGENT_ALLOW_WRITE_TOOLS") {
		log.Println("MCP: AGENT_ALLOW_WRITE_TOOLS is set; the agent can modify the cluster.")
	} else {
		agentTools = mcp.ReadOnly(agentTools)
	}

	// Separately from safety: drop the cluster-introspection tools the prompt
	// already tells the model to avoid.
	excluded := mcp.DefaultExcluded
	if raw, set := os.LookupEnv("AGENT_EXCLUDE_TOOLS"); set {
		excluded = strings.Split(raw, ",")
	}

	agentTools = mcp.Exclude(agentTools, excluded)
	sreAgent = agent.New(model, store, agentTools)

	// Read and cache the schema once rather than letting every run rediscover
	// it. Best effort: on failure the model discovers it per run, as before.
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
