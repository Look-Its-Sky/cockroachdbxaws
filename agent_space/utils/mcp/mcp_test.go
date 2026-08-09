package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"agent_space/utils/mcp/mcptest"
)

// Aliases keep the table-driven cases below readable.
var (
	selectQuerySchema    = mcptest.SelectQuerySchema
	listTablesSchema     = mcptest.ListTablesSchema
	getTableSchemaSchema = mcptest.GetTableSchemaSchema
)

func connectFake(t *testing.T) (*Session, *mcptest.Server) {
	t.Helper()

	fake := mcptest.Start(t)
	session, err := Connect(t.Context(), Config{
		URL:       fake.URL,
		APIKey:    "test-key",
		ClusterID: "cluster-123",
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	return session, fake
}

func TestConnectDiscoversTools(t *testing.T) {
	session, _ := connectFake(t)

	got := session.Names()
	want := []string{"get_cluster", "get_table_schema", "list_tables", "select_query"} // sorted by name
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v", got, want)
		}
	}

	tool, ok := session.Tool("select_query")
	if !ok {
		t.Fatal("select_query not found")
	}

	// The raw schema must survive discovery intact: the native tool loop hands
	// it straight to the model, so any lossy conversion here stays invisible
	// until the model starts guessing at argument shapes.
	schema, ok := tool.Schema().(map[string]any)
	if !ok {
		t.Fatalf("Schema() = %T, want map[string]any", tool.Schema())
	}
	if schema["type"] != "object" {
		t.Errorf("schema type = %v, want object", schema["type"])
	}
	if props, ok := schema["properties"].(map[string]any); !ok {
		t.Errorf("schema has no properties: %v", schema)
	} else if _, ok := props["query"]; !ok {
		t.Errorf("schema properties missing query: %v", props)
	}

	if desc := tool.Description(); !strings.Contains(desc, "query") {
		t.Errorf("Description() does not surface the schema: %q", desc)
	}
}

func TestConnectSendsAuthHeaders(t *testing.T) {
	_, fake := connectFake(t)

	if got := fake.Header("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer test-key")
	}
	if got := fake.Header(clusterIDHeader); got != "cluster-123" {
		t.Errorf("%s = %q, want %q", clusterIDHeader, got, "cluster-123")
	}
}

func TestConnectOmitsClusterHeaderWhenUnset(t *testing.T) {
	fake := mcptest.Start(t)

	session, err := Connect(t.Context(), Config{URL: fake.URL, APIKey: "test-key"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	if got := fake.Header(clusterIDHeader); got != "" {
		t.Errorf("%s = %q, want it absent so the key's full RBAC scope applies", clusterIDHeader, got)
	}
}

func TestConnectWithoutAPIKey(t *testing.T) {
	if _, err := Connect(t.Context(), Config{URL: "https://example.invalid/mcp"}); err != ErrNotConfigured {
		t.Errorf("Connect without key = %v, want ErrNotConfigured", err)
	}
}

func TestConnectTransportFailure(t *testing.T) {
	// A dead endpoint must return an error rather than hanging or panicking:
	// boot treats this as "run without MCP".
	_, err := Connect(t.Context(), Config{URL: "http://127.0.0.1:1/mcp", APIKey: "secret-key"})
	if err == nil {
		t.Fatal("Connect to a dead endpoint succeeded, want error")
	}
	if strings.Contains(err.Error(), "secret-key") {
		t.Errorf("error leaks the API key: %v", err)
	}
}

func TestInvokeReturnsToolText(t *testing.T) {
	session, fake := connectFake(t)
	tool, _ := session.Tool("select_query")

	out, err := tool.Invoke(t.Context(), map[string]any{"query": "SELECT 1"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out != "ran: SELECT 1" {
		t.Errorf("Invoke = %q, want %q", out, "ran: SELECT 1")
	}

	calls := fake.Calls()
	if len(calls) != 1 || calls[0].Tool != "select_query" || calls[0].Args["query"] != "SELECT 1" {
		t.Errorf("server received %+v, want one select_query with query SELECT 1", calls)
	}
}

func TestInvokeSurfacesToolErrorAsText(t *testing.T) {
	session, _ := connectFake(t)
	tool, _ := session.Tool("select_query")

	// A tool-level rejection must come back as readable text, not a Go error,
	// so the model can correct the statement and retry.
	out, err := tool.Invoke(t.Context(), map[string]any{"query": "DROP TABLE incidents"})
	if err != nil {
		t.Fatalf("Invoke returned a Go error for a tool-level failure: %v", err)
	}
	if !strings.Contains(out, "only SELECT statements are permitted") {
		t.Errorf("Invoke = %q, want the tool's own error message", out)
	}
}

func TestCallParsesModelStrings(t *testing.T) {
	session, _ := connectFake(t)
	tool, _ := session.Tool("select_query")

	tests := []struct {
		name  string
		input string
	}{
		{"plain json", `{"query": "SELECT 1"}`},
		{"fenced json", "```json\n{\"query\": \"SELECT 1\"}\n```"},
		{"bare fence", "```\n{\"query\": \"SELECT 1\"}\n```"},
		{"double encoded", `"{\"query\": \"SELECT 1\"}"`},
		{"bare text for single required field", "SELECT 1"},
		{"quoted bare text", `"SELECT 1"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := tool.Call(t.Context(), tt.input)
			if err != nil {
				t.Fatalf("Call(%q): %v", tt.input, err)
			}
			if out != "ran: SELECT 1" {
				t.Errorf("Call(%q) = %q, want %q", tt.input, out, "ran: SELECT 1")
			}
		})
	}
}

func TestCallRejectsUnparseableInputWithSchema(t *testing.T) {
	session, _ := connectFake(t)
	// get_table_schema needs two fields, so bare text is genuinely ambiguous
	// and must fail rather than guess. list_tables requires only "database",
	// so it would legitimately wrap bare text instead.
	tool, _ := session.Tool("get_table_schema")

	_, err := tool.Call(t.Context(), "just tell me the columns")
	if err == nil {
		t.Fatal("Call with ambiguous input succeeded, want an error")
	}
	// The error is the model's only feedback channel, so it has to name the
	// fields the tool wants.
	for _, want := range []string{"database", "table"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q, so the model cannot retry: %v", want, err)
		}
	}
}

// The single-required-field wrap is what lets a model that emits bare text
// still drive a tool. list_tables is the real example: "defaultdb" is
// unambiguous because "database" is the only field that must be supplied.
func TestCallWrapsBareTextForSingleRequiredField(t *testing.T) {
	session, fake := connectFake(t)
	tool, _ := session.Tool("list_tables")

	out, err := tool.Call(t.Context(), "defaultdb")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "tables in defaultdb") {
		t.Errorf("Call = %q, want the bare text wrapped into database", out)
	}

	calls := fake.Calls()
	if len(calls) != 1 || calls[0].Args["database"] != "defaultdb" {
		t.Errorf("server received %+v, want database=defaultdb", calls)
	}
}

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		schema  any
		want    map[string]any
		wantErr bool
	}{
		{
			name:   "object passes through",
			input:  `{"database": "defaultdb", "table": "incidents"}`,
			schema: getTableSchemaSchema,
			want:   map[string]any{"database": "defaultdb", "table": "incidents"},
		},
		{
			name:  "prose before json is not treated as an object",
			input: "Here you go: {\"query\": \"SELECT 1\"}",
			// Falls back to the single-required-field wrap, which keeps the
			// call alive; the server rejects it if the SQL is wrong.
			schema: selectQuerySchema,
			want:   map[string]any{"query": "Here you go: {\"query\": \"SELECT 1\"}"},
		},
		{
			name:   "empty input with no required fields",
			input:  "",
			schema: map[string]any{"type": "object"},
			want:   map[string]any{},
		},
		{
			name:    "empty input with required fields",
			input:   "   ",
			schema:  selectQuerySchema,
			wantErr: true,
		},
		{
			name:   "empty object",
			input:  "{}",
			schema: selectQuerySchema,
			want:   map[string]any{},
		},
		{
			name:    "json array is not arguments",
			input:   `["SELECT 1"]`,
			schema:  getTableSchemaSchema,
			wantErr: true,
		},
		{
			name:   "nil schema still accepts an object",
			input:  `{"a": 1}`,
			schema: nil,
			want:   map[string]any{"a": float64(1)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseArgs(tt.input, tt.schema)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseArgs(%q) = %v, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseArgs(%q): %v", tt.input, err)
			}

			encodedGot, _ := json.Marshal(got)
			encodedWant, _ := json.Marshal(tt.want)
			if string(encodedGot) != string(encodedWant) {
				t.Errorf("ParseArgs(%q) = %s, want %s", tt.input, encodedGot, encodedWant)
			}
		})
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("COCKROACH_API_KEY", "")
	t.Setenv("COCKROACH_MCP_URL", "")
	t.Setenv("COCKROACH_CLUSTER_ID", "")

	cfg := ConfigFromEnv()
	if cfg.URL != DefaultURL {
		t.Errorf("URL = %q, want the managed endpoint %q", cfg.URL, DefaultURL)
	}
	if cfg.Configured() {
		t.Error("Configured() = true without an API key")
	}

	t.Setenv("COCKROACH_API_KEY", "abc")
	t.Setenv("COCKROACH_CLUSTER_ID", "cid")
	cfg = ConfigFromEnv()
	if !cfg.Configured() {
		t.Error("Configured() = false with URL and key set")
	}
	if cfg.ClusterID != "cid" {
		t.Errorf("ClusterID = %q, want %q", cfg.ClusterID, "cid")
	}
}

// A no-argument tool publishes {"type":"object"} with no "properties". That is
// legal MCP, but LM Studio rejects the entire function list with a 400 when the
// key is missing — so one such tool breaks every call in the batch, not just
// its own.
func TestSchemaFillsInPropertiesForNoArgTools(t *testing.T) {
	session, _ := connectFake(t)

	tool, ok := session.Tool("get_cluster")
	if !ok {
		t.Fatal("get_cluster not found")
	}

	// The server really did omit it, so the test exercises the fix.
	raw, _ := tool.remote.InputSchema.(map[string]any)
	if _, present := raw["properties"]; present {
		t.Fatal("fixture no longer reproduces the case: raw schema already has properties")
	}

	schema, ok := tool.Schema().(map[string]any)
	if !ok {
		t.Fatalf("Schema() = %T, want map[string]any", tool.Schema())
	}
	if schema["type"] != "object" {
		t.Errorf("type = %v, want object", schema["type"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %v, want an empty object", schema["properties"])
	}
	if len(props) != 0 {
		t.Errorf("properties = %v, want it empty", props)
	}

	// Normalising must not write through to the SDK's shared map.
	if _, mutated := raw["properties"]; mutated {
		t.Error("normalizeSchema mutated the server's schema")
	}
}

func TestSchemaLeavesRealSchemasIntact(t *testing.T) {
	session, _ := connectFake(t)
	tool, _ := session.Tool("select_query")

	schema, _ := tool.Schema().(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["query"]; !ok {
		t.Errorf("properties lost the real fields: %v", props)
	}
	if req, _ := schema["required"].([]any); len(req) != 1 {
		t.Errorf("required = %v, want it preserved", schema["required"])
	}
}

func TestNoArgToolIsCallable(t *testing.T) {
	session, _ := connectFake(t)
	tool, _ := session.Tool("get_cluster")

	// "{}" and "" are both what a model sends for a no-argument tool.
	for _, input := range []string{"{}", ""} {
		out, err := tool.Call(t.Context(), input)
		if err != nil {
			t.Fatalf("Call(%q): %v", input, err)
		}
		if !strings.Contains(out, "local-single-node") {
			t.Errorf("Call(%q) = %q", input, out)
		}
	}
}

// clusterScopedTool mirrors what the Cloud server publishes for a
// cluster-scoped tool: cluster_id sitting alongside the real arguments.
func clusterScopedTool(clusterID string, required []any) *Tool {
	return &Tool{
		remote: &sdk.Tool{
			Name:        "list_databases",
			Description: "List databases in the cluster",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cluster_id": map[string]any{"type": "string"},
					"database":   map[string]any{"type": "string"},
				},
				"required": required,
			},
		},
		session: &Session{config: Config{ClusterID: clusterID}},
	}
}

func TestSchemaHidesClusterIDWhenSessionIsScoped(t *testing.T) {
	// The server rejects the whole call when cluster_id arrives alongside the
	// mcp-cluster-id header, and a model shown the property will fill it in —
	// it did, reading the real UUID out of a prior list_clusters result. The
	// only durable fix is not to show it.
	tool := clusterScopedTool("8fdfa73f-06b5-4fbb-a47a-7888a1bda51f", []any{"cluster_id", "database"})

	schema, ok := tool.Schema().(map[string]any)
	if !ok {
		t.Fatalf("Schema() = %T, want map[string]any", tool.Schema())
	}
	props, _ := schema["properties"].(map[string]any)
	if _, present := props["cluster_id"]; present {
		t.Error("cluster_id is still offered to the model")
	}
	if _, present := props["database"]; !present {
		t.Errorf("the real arguments were lost: %v", props)
	}

	// Leaving it in "required" would be worse than leaving it in properties:
	// a strict server rejects the call for omitting a required field.
	req, _ := schema["required"].([]any)
	for _, r := range req {
		if r == "cluster_id" {
			t.Errorf("required still names cluster_id: %v", req)
		}
	}
	if len(req) != 1 || req[0] != "database" {
		t.Errorf("required = %v, want [database]", req)
	}

	// The SDK's map is shared across calls, so stripping must copy.
	raw, _ := tool.remote.InputSchema.(map[string]any)
	rawProps, _ := raw["properties"].(map[string]any)
	if _, present := rawProps["cluster_id"]; !present {
		t.Error("stripping mutated the server's own schema")
	}
}

func TestSchemaKeepsClusterIDWhenSessionIsNotScoped(t *testing.T) {
	// Organization-wide sessions send no header, so the argument is the only
	// way to name a cluster and must survive.
	tool := clusterScopedTool("", []any{"cluster_id"})

	schema, _ := tool.Schema().(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	if _, present := props["cluster_id"]; !present {
		t.Error("cluster_id was hidden from an unscoped session, leaving no way to target a cluster")
	}
}

func TestSchemaDropsRequiredWhenClusterIDWasItsOnlyEntry(t *testing.T) {
	// "required": [] is not valid JSON Schema, and strict OpenAI-compatible
	// servers reject the whole tool list over it.
	tool := clusterScopedTool("some-cluster", []any{"cluster_id"})

	schema, _ := tool.Schema().(map[string]any)
	if v, present := schema["required"]; present {
		t.Errorf("required = %v, want the key dropped entirely", v)
	}
}
