package routes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/schema"

	"agent_space/agent"
)

// the whole idea here is this requires auth

// stores text in the vector database
func Store(c *gin.Context) {
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

	_, err := VectorStore.AddDocuments(ctx, []schema.Document{doc})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Stored successfully"})
}

// similarity search the vector db
func Retrieve(c *gin.Context) {
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
	docs, err := VectorStore.SimilaritySearch(ctx, req.Query, req.Limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var results []string
	for _, d := range docs {
		results = append(results, d.PageContent)
	}

	c.JSON(http.StatusOK, gin.H{"results": results})
}

// embed -> retrieve -> generate.
func Ask(c *gin.Context) {
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
	docs, err := VectorStore.SimilaritySearch(ctx, req.Question, req.Limit)
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

	answer, err := llms.GenerateFromSinglePrompt(ctx, Model, prompt, llms.WithMaxTokens(2000))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"answer": answer, "sources": len(docs)})
}

// ask if one shot, agent is multi
func Agent(c *gin.Context) {
	if SREAgent == nil {
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

	result, err := SREAgent.Run(c.Request.Context(), req.Question, req.Limit)
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
}

// reads back an investigation the SQS worker started, by investigation_id.
// The queue path is asynchronous, so this is how a verdict is collected.
func Investigation(c *gin.Context) {
	if Results == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": workerUnavailable})
		return
	}

	record, ok := Results.Get(c.Param("id"))
	if !ok {
		// also what an assignment that was never delivered looks like, and
		// what a verdict from before a restart looks like: results are in-memory
		c.JSON(http.StatusNotFound, gin.H{"error": "no investigation with that id"})
		return
	}

	c.JSON(http.StatusOK, record)
}
