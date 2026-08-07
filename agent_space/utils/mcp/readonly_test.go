package mcp

import (
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolWith builds a Tool carrying just the fields ReadOnly looks at.
func toolWith(name string, ann *sdk.ToolAnnotations) *Tool {
	return &Tool{remote: &sdk.Tool{Name: name, Annotations: ann}}
}

func names(tools []*Tool) string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name())
	}
	return strings.Join(out, ",")
}

// The mix here is copied from cockroachdb-mcp-server 0.1.0: most read tools
// declare readOnlyHint, the two obvious writers declare it false, and three
// writers declare nothing at all. That last group is the whole point.
func TestReadOnlyUsesAnnotations(t *testing.T) {
	ro := &sdk.ToolAnnotations{ReadOnlyHint: true}
	rw := &sdk.ToolAnnotations{ReadOnlyHint: false}

	got := ReadOnly([]*Tool{
		toolWith("select_query", ro),
		toolWith("list_tables", ro),
		toolWith("delete_rows", rw),
		toolWith("update_rows", rw),
		toolWith("create_table", nil),    // declares nothing
		toolWith("create_database", nil), // declares nothing
		toolWith("insert_rows", nil),     // declares nothing
	})

	if want := "select_query,list_tables"; names(got) != want {
		t.Errorf("ReadOnly kept %q, want %q", names(got), want)
	}
}

// A tool that declares nothing must be withheld, not admitted. Getting this
// backwards would hand the agent create_table and insert_rows on the real
// server, which is exactly the failure this filter exists to prevent.
func TestReadOnlyFailsClosedOnMissingAnnotations(t *testing.T) {
	got := ReadOnly([]*Tool{
		toolWith("select_query", &sdk.ToolAnnotations{ReadOnlyHint: true}),
		toolWith("harmless_looking_tool", nil),
	})

	if names(got) != "select_query" {
		t.Errorf("ReadOnly kept %q, want an undeclared tool to be withheld", names(got))
	}
}

// When no tool declares the hint the server publishes no annotations at all,
// and failing closed would leave the agent with nothing. The name fallback
// keeps it working — this is the likely shape of an unfamiliar server.
func TestReadOnlyFallsBackToNamesWhenNothingIsAnnotated(t *testing.T) {
	got := ReadOnly([]*Tool{
		toolWith("select_query", nil),
		toolWith("list_tables", nil),
		toolWith("explain_query", nil),
		toolWith("create_table", nil),
		toolWith("delete_rows", nil),
		toolWith("insert_rows", nil),
		toolWith("update_rows", nil),
		toolWith("drop_index", nil),
	})

	if want := "select_query,list_tables,explain_query"; names(got) != want {
		t.Errorf("ReadOnly kept %q, want %q", names(got), want)
	}
}

func TestReadOnlyEmptyInput(t *testing.T) {
	if got := ReadOnly(nil); len(got) != 0 {
		t.Errorf("ReadOnly(nil) = %v, want empty", names(got))
	}
}

// The real server, end to end: the filter must leave the agent a usable set,
// not an empty one.
func TestReadOnlyKeepsSessionToolsUsable(t *testing.T) {
	session, _ := connectFake(t)

	got := ReadOnly(session.Tools())
	if len(got) == 0 {
		t.Fatal("ReadOnly removed every tool; the agent would have nothing to call")
	}
	for _, tool := range got {
		if looksLikeWrite(tool.Name()) {
			t.Errorf("ReadOnly kept a write tool: %s", tool.Name())
		}
	}
}
