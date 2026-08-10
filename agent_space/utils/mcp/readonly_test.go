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

// copied from cockroachdb-mcp-server 0.1.0: most reads declare readOnlyHint, two writers declare false, three declare nothing
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

// a tool declaring nothing must be withheld; backwards would hand the agent create_table and insert_rows
func TestReadOnlyFailsClosedOnMissingAnnotations(t *testing.T) {
	got := ReadOnly([]*Tool{
		toolWith("select_query", &sdk.ToolAnnotations{ReadOnlyHint: true}),
		toolWith("harmless_looking_tool", nil),
	})

	if names(got) != "select_query" {
		t.Errorf("ReadOnly kept %q, want an undeclared tool to be withheld", names(got))
	}
}

// with no annotations at all, failing closed leaves nothing, so the name fallback keeps it working
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

// the real server end to end: the filter must leave a usable set, not an empty one
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

func TestExcludeDropsNamedToolsOnly(t *testing.T) {
	tools := []*Tool{
		{remote: &sdk.Tool{Name: "select_query"}},
		{remote: &sdk.Tool{Name: "show_statement"}},
		{remote: &sdk.Tool{Name: "show_running_queries"}},
		{remote: &sdk.Tool{Name: "list_tables"}},
	}

	kept := Exclude(tools, DefaultExcluded)

	var names []string
	for _, tool := range kept {
		names = append(names, tool.Name())
	}
	want := []string{"select_query", "list_tables"}
	if len(names) != len(want) {
		t.Fatalf("kept %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("kept %v, want %v", names, want)
		}
	}
}

func TestExcludeMatchesWholeNamesNotPrefixes(t *testing.T) {
	// "show_" as a prefix would take its neighbours with it, so matching is exact
	tools := []*Tool{
		{remote: &sdk.Tool{Name: "show_statement"}},
		{remote: &sdk.Tool{Name: "show_statement_details"}},
	}

	kept := Exclude(tools, []string{"show_statement"})
	if len(kept) != 1 || kept[0].Name() != "show_statement_details" {
		t.Errorf("Exclude removed a tool by prefix: %d kept", len(kept))
	}
}

func TestExcludeWithNoNamesIsAPassThrough(t *testing.T) {
	tools := []*Tool{{remote: &sdk.Tool{Name: "select_query"}}}
	if got := Exclude(tools, nil); len(got) != 1 {
		t.Errorf("Exclude(nil) dropped tools: %d kept", len(got))
	}
	// An explicitly empty AGENT_EXCLUDE_TOOLS means "offer everything".
	if got := Exclude(tools, []string{""}); len(got) != 1 {
		t.Errorf("Exclude with a blank name dropped tools: %d kept", len(got))
	}
}
