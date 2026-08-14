package routes

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/vectorstores"

	"agent_space/agent"
	"agent_space/incident"
	"agent_space/remediation"
	"agent_space/utils/mcp"
	"agent_space/worker"
)

// this is public and authless

var (
	VectorStore vectorstores.VectorStore
	Model       llms.Model
	MCPSession  *mcp.Session
	SREAgent    *agent.Runner
	// verdicts from the SQS worker; nil when no queue is configured
	Results *worker.Store

	// the remediation half. Each is nil when its prerequisite is missing — no
	// container runtime, no database, no GitHub token — and the routes that
	// need one say which is absent rather than failing obscurely.
	Remediation *remediation.Runner
	Solutions   *remediation.Solutions
	Repos       *remediation.Repositories
	Publisher   *remediation.Publisher
	// where recorded decisions are embedded so future incidents recall them.
	// Nil records decisions without indexing them, which the response says.
	PrecedentIndex *remediation.Precedents
	// reads incident prose back for a remediation started from the API
	Incidents *incident.Resolver
)

const mcpUnavailable = "CockroachDB MCP is not configured. Check if COCKROACH_API_KEY is set"

const workerUnavailable = "The SQS worker is not running. Check if SQS_QUEUE_URL is set"

// lists discovered tools via MCP
func Tools(c *gin.Context) {
	if MCPSession == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": mcpUnavailable})
		return
	}

	// map exposed tools
	exposed := make(map[string]bool)
	if SREAgent != nil {
		for _, t := range SREAgent.Tools {
			exposed[t.Name()] = true
		}
	}

	tools := make([]gin.H, 0, len(MCPSession.Tools()))
	withheld := 0
	for _, t := range MCPSession.Tools() {
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
		"endpoint":         MCPSession.Endpoint(),
		"cluster":          MCPSession.ClusterID(),
		"discovered":       len(tools),
		"offered_to_agent": len(tools) - withheld,
		"withheld_writes":  withheld,
		"tools":            tools,
	})
}
