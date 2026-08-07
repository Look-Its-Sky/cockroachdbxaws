package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/schema"
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

// mcpUnavailable is returned by the MCP-backed routes when the boot-time
// handshake did not succeed, so the failure is legible instead of a 404.
const mcpUnavailable = "CockroachDB MCP is not configured. Set COCKROACH_API_KEY (and optionally COCKROACH_CLUSTER_ID) and restart."

func initStore() {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		log.Fatal("CRITICAL ERROR: DATABASE_URL is not set in the environment or .env file! Refusing to start.")
	}

	embedder, err := utils.GetEmbedder()
	if err != nil {
		log.Fatalf("Failed to initialize embedder: %v", err)
	}

	// Log the target before dialling: a "connection refused" is otherwise
	// indistinguishable from pointing at the wrong host entirely.
	log.Printf("Vector store: connecting to %s", utils.RedactURL(connStr))

	ctx := context.Background()
	// Initialize the vector store using our custom crdbvector, compatible with CockroachDB
	store, err = crdbvector.New(
		ctx,
		crdbvector.WithConnectionURL(connStr),
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
// Unlike the store and the LLM, this is best-effort: a missing key or an
// unreachable endpoint must not take down /store, /retrieve and /ask, which are
// the demonstrable path. A nil sreAgent means those routes still serve and
// /agent reports why it cannot.
func initMCP() {
	cfg := mcp.ConfigFromEnv()
	if !cfg.Configured() {
		log.Println("MCP: COCKROACH_API_KEY is not set; /agent will be unavailable.")
		return
	}

	session, err := mcp.Connect(context.Background(), cfg)
	if err != nil {
		log.Printf("MCP: connection failed, /agent will be unavailable: %v", err)
		return
	}

	mcpSess = session

	// The agent diagnoses; it does not remediate. Withholding the write tools
	// costs it nothing it needs and removes the possibility that a misread
	// instruction turns into a DELETE. Set AGENT_ALLOW_WRITE_TOOLS=true to
	// hand them over anyway.
	agentTools := session.Tools()
	if utils.EnvBool("AGENT_ALLOW_WRITE_TOOLS") {
		log.Println("MCP: AGENT_ALLOW_WRITE_TOOLS is set; the agent can modify the cluster.")
	} else {
		agentTools = mcp.ReadOnly(agentTools)
	}
	sreAgent = agent.New(model, store, agentTools)

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

func main() {
	utils.LoadConfig()

	initStore()
	initLLM()
	initMCP()
	defer mcpSess.Close()

	router := gin.Default()

	router.GET("/ping", utils.Ping)

	// Endpoint to store text into the vector database
	router.POST("/store", func(c *gin.Context) {
		var req struct {
			Text string `json:"text" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		ctx := context.Background()
		doc := schema.Document{
			PageContent: req.Text,
			Metadata:    map[string]any{"source": "api"},
		}

		_, err := store.AddDocuments(ctx, []schema.Document{doc})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "Stored successfully"})
	})

	// Endpoint to retrieve similar text from the vector database
	router.POST("/retrieve", func(c *gin.Context) {
		var req struct {
			Query string `json:"query" binding:"required"`
			Limit int    `json:"limit"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		if req.Limit <= 0 {
			req.Limit = 2
		}

		ctx := context.Background()
		docs, err := store.SimilaritySearch(ctx, req.Query, req.Limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		var results []string
		for _, d := range docs {
			results = append(results, d.PageContent)
		}

		c.JSON(http.StatusOK, gin.H{"results": results})
	})

	// Endpoint to answer a question using the stored context as grounding.
	// This is the smallest end-to-end exercise of embed -> retrieve -> generate.
	router.POST("/ask", func(c *gin.Context) {
		var req struct {
			Question string `json:"question" binding:"required"`
			Limit    int    `json:"limit"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		if req.Limit <= 0 {
			req.Limit = 4
		}

		ctx := context.Background()
		docs, err := store.SimilaritySearch(ctx, req.Question, req.Limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		var grounding strings.Builder
		for _, d := range docs {
			grounding.WriteString("- " + d.PageContent + "\n")
		}

		prompt := fmt.Sprintf(
			"Answer the question using only the context below. If the context is insufficient, say so.\n\nContext:\n%s\nQuestion: %s",
			grounding.String(), req.Question,
		)

		// Reasoning models spend completion tokens thinking before they emit
		// any content, so the budget has to leave room for both.
		answer, err := llms.GenerateFromSinglePrompt(ctx, model, prompt, llms.WithMaxTokens(2000))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"answer": answer, "sources": len(docs)})
	})

	// Endpoint listing the CockroachDB Cloud MCP tools discovered at boot.
	// This is the visible proof that the second CockroachDB integration is
	// live, and it is the cheapest way to confirm the handshake worked.
	router.GET("/tools", func(c *gin.Context) {
		if mcpSess == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": mcpUnavailable})
			return
		}

		// Which tools the agent may actually call is a security property, so
		// report it rather than leaving it to be inferred from the boot log.
		exposed := make(map[string]bool)
		if sreAgent != nil {
			for _, t := range sreAgent.Tools {
				exposed[t.Name()] = true
			}
		}

		tools := make([]gin.H, 0, len(mcpSess.Tools()))
		withheld := 0
		for _, t := range mcpSess.Tools() {
			if !exposed[t.Name()] {
				withheld++
			}
			tools = append(tools, gin.H{
				"name":             t.Name(),
				"description":      t.Description(),
				"input_schema":     t.Schema(),
				"offered_to_agent": exposed[t.Name()],
			})
		}

		c.JSON(http.StatusOK, gin.H{
			"endpoint":         mcpSess.Endpoint(),
			"cluster":          mcpSess.ClusterID(),
			"discovered":       len(tools),
			"offered_to_agent": len(tools) - withheld,
			"withheld_writes":  withheld,
			"tools":            tools,
		})
	})

	// Endpoint running the full SRE loop: recall similar past incidents from
	// the vector index, query the live cluster over MCP, then decide rollback
	// or hotfix. /ask remains the one-shot path; this is the agentic one.
	router.POST("/agent", func(c *gin.Context) {
		if sreAgent == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": mcpUnavailable})
			return
		}

		var req struct {
			Question string `json:"question" binding:"required"`
			Limit    int    `json:"limit"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		result, err := sreAgent.Run(c.Request.Context(), req.Question, req.Limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, result)
	})

	// Container platforms inject the listen port, so honour PORT when set.
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Run returns an error rather than exiting, so discarding it turns "the port
	// is already taken" into a silent exit 0 — which reads as a clean shutdown
	// while a stale instance keeps serving the old configuration on that port.
	if err := router.Run(":" + port); err != nil {
		log.Fatalf("Server failed on port %s: %v\n"+
			"  If the port is already in use, another instance is still running:\n"+
			"    pkill -f agent_space   (or set PORT to something else)", port, err)
	}
}
