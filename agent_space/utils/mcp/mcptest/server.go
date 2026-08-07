// Package mcptest provides an in-process stand-in for the CockroachDB Cloud
// MCP server, so the client adapter and the agent loop can be tested against a
// real MCP round trip without cloud credentials or network access.
package mcptest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The schemas are copied from a live cockroachdb-mcp-server 0.1.0, as raw JSON
// objects rather than Go types, so tests see exactly the shapes production
// does. Getting these wrong is not cosmetic: an earlier version of this file
// invented select_query{statement}, and because the real tool takes {query}
// with additionalProperties:false, every test passed while the real call was
// rejected outright.
//
// Keep additionalProperties:false — the SDK validates arguments against these,
// so a wrong field name fails here the same way it fails against the cluster.
var (
	SelectQuerySchema = map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "A single SELECT statement; non-SELECT statements are rejected. A default LIMIT is appended when none is supplied, capped at CRDB_MCP_MAX_ROWS_COUNT.",
			},
		},
		"required": []any{"query"},
	}

	// ListTablesSchema has one required field and two optional ones, so bare
	// text is unambiguous and gets wrapped into "database".
	ListTablesSchema = map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"database": map[string]any{"type": "string", "description": "Database whose tables are listed."},
			"limit":    map[string]any{"type": []any{"null", "integer"}, "description": "Max rows to return."},
			"offset":   map[string]any{"type": []any{"null", "integer"}, "description": "Rows to skip before returning results."},
		},
		"required": []any{"database"},
	}

	// GetTableSchemaSchema needs two fields, which is what makes bare text
	// genuinely ambiguous. This is the real multi-required tool; do not
	// substitute a single-required one or that distinction stops being tested.
	GetTableSchemaSchema = map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"database": map[string]any{"type": "string", "description": "Database containing the table."},
			"schema":   map[string]any{"type": "string", "description": "Schema name. Defaults to 'public' when omitted."},
			"table":    map[string]any{"type": "string", "description": "Table name."},
		},
		"required": []any{"database", "table"},
	}

	// NoArgsSchema mirrors what a no-argument tool publishes over the wire:
	// legal MCP, but missing the "properties" key that strict
	// OpenAI-compatible servers require. get_cluster and list_cluster_nodes
	// arrive like this, which is what broke LM Studio for the whole batch.
	NoArgsSchema = map[string]any{"type": "object"}
)

// Server is a running fake MCP server.
type Server struct {
	URL string

	mu      sync.Mutex
	headers http.Header
	calls   []Call
}

// Call records one tool invocation the server received.
type Call struct {
	Tool string
	Args map[string]any
}

// Start launches a fake server exposing select_query and list_tables, and
// registers its shutdown with t.
func Start(t *testing.T) *Server {
	t.Helper()

	fake := &Server{}
	srv := sdk.NewServer(&sdk.Implementation{Name: "fake-crdb", Version: "0.0.1"}, nil)

	srv.AddTool(&sdk.Tool{
		Name:        "select_query",
		Description: "Run a read-only SELECT against the cluster.",
		InputSchema: SelectQuerySchema,
	}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		args := fake.record("select_query", req)

		query, _ := args["query"].(string)
		// Mirror the real server refusing anything but a read: an error the
		// model is meant to read and recover from, not a transport failure.
		if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(query)), "SELECT") {
			return &sdk.CallToolResult{
				IsError: true,
				Content: []sdk.Content{&sdk.TextContent{Text: "only SELECT statements are permitted"}},
			}, nil
		}

		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: "ran: " + query}},
		}, nil
	})

	srv.AddTool(&sdk.Tool{
		Name:        "list_tables",
		Description: "List the tables in a database.",
		InputSchema: ListTablesSchema,
	}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		args := fake.record("list_tables", req)
		database, _ := args["database"].(string)

		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{
				Text: "tables in " + database + ": incidents",
			}},
		}, nil
	})

	srv.AddTool(&sdk.Tool{
		Name:        "get_table_schema",
		Description: "Describe the columns of a table.",
		InputSchema: GetTableSchemaSchema,
	}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		args := fake.record("get_table_schema", req)
		database, _ := args["database"].(string)
		table, _ := args["table"].(string)

		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{
				Text: "columns of " + database + "." + table + ": id, commit_sha, summary",
			}},
		}, nil
	})

	srv.AddTool(&sdk.Tool{
		Name:        "get_cluster",
		Description: "Describe the cluster.",
		InputSchema: NoArgsSchema,
	}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		fake.record("get_cluster", req)
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: "cluster: local-single-node"}},
		}, nil
	})

	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.captureHeaders(r.Header)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)

	fake.URL = ts.URL
	return fake
}

func (s *Server) record(tool string, req *sdk.CallToolRequest) map[string]any {
	var args map[string]any
	_ = json.Unmarshal(req.Params.Arguments, &args)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, Call{Tool: tool, Args: args})

	return args
}

func (s *Server) captureHeaders(h http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.headers == nil {
		s.headers = h.Clone()
	}
}

// Header returns a header from the first request the server received. This is
// the only way to prove the auth headers survive the client's RoundTripper.
func (s *Server) Header(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headers.Get(key)
}

// Calls returns every tool invocation received, in order.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}
