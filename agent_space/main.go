package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/schema"

	"agent_space/agent"
	"agent_space/utils"
)

// Boot-time wiring lives in initialization.go; this file is the routes.

const mcpUnavailable = "CockroachDB MCP is not configured. Check if COCKROACH_API_KEY is set" // in event of cooked mcp

func main() {
	utils.LoadConfig()

	initStore()
	initLLM()
	initMCP()
	defer mcpSess.Close()

	router := gin.Default()

	guarded := router.Group("", utils.RequireToken())

	router.GET("/ping", utils.Ping)

	// stores text in the vector database
	guarded.POST("/store", func(c *gin.Context) {
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
	guarded.POST("/retrieve", func(c *gin.Context) {
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
	guarded.POST("/ask", func(c *gin.Context) {
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
	guarded.POST("/agent", func(c *gin.Context) {
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
			if errors.Is(err, agent.ErrTransport) {
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"error": err.Error(),
					"trace": result.Trace,
				})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, result)
	})

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
