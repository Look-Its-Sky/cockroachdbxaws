package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tmc/langchaingo/tools"
)

// maxResultChars bounds what a single tool result contributes to the next
// prompt. The server already caps responses at 10 KiB; this keeps a handful of
// tool calls from crowding out the incident context.
const maxResultChars = 6000

// Tool is one remote MCP tool, usable two ways:
//
//   - Schema plus Invoke, for the native tool loop, which passes the real JSON
//     Schema to the model and gets structured arguments back.
//   - Name, Description and Call, satisfying langchaingo's tools.Tool, whose
//     agent flattens every tool to a single string argument. That path can only
//     convey the schema as prose, so Description carries it.
type Tool struct {
	remote  *sdk.Tool
	session *Session
}

var _ tools.Tool = (*Tool)(nil)

// Name returns the MCP tool name.
func (t *Tool) Name() string { return t.remote.Name }

// Schema returns the tool's JSON Schema, ready to pass through as an OpenAI
// function's parameters.
func (t *Tool) Schema() any { return normalizeSchema(t.remote.InputSchema) }

// normalizeSchema fills in the parts of a JSON Schema that MCP leaves optional
// but strict OpenAI-compatible servers demand.
//
// A tool that takes no arguments publishes just {"type":"object"} — legal JSON
// Schema and legal MCP. LM Studio validates the function list and rejects the
// whole request with a 400 when "properties" is absent, so one no-argument tool
// (get_cluster, list_cluster_nodes) takes down every call in the batch. Adding
// the empty object costs nothing and describes exactly the same contract.
func normalizeSchema(schema any) any {
	m, ok := schema.(map[string]any)
	if !ok {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}

	// Copy: the map belongs to the SDK's Tool and is shared across calls.
	out := make(map[string]any, len(m)+2)
	for k, v := range m {
		out[k] = v
	}
	if _, ok := out["type"]; !ok {
		out["type"] = "object"
	}
	if _, ok := out["properties"]; !ok {
		out["properties"] = map[string]any{}
	}
	return out
}

// Annotations exposes the server's own behavioural hints for this tool.
// Nil when the server declares none.
func (t *Tool) Annotations() *sdk.ToolAnnotations { return t.remote.Annotations }

// Description returns the server's description with the input schema appended.
// The schema is redundant on the native path, where the model receives it
// properly, but it is the only channel available to tools.Tool consumers.
func (t *Tool) Description() string {
	desc := strings.TrimSpace(t.remote.Description)
	if desc == "" {
		desc = "CockroachDB Cloud MCP tool " + t.remote.Name + "."
	}
	return fmt.Sprintf("%s\nInput must be a JSON object matching this schema: %s", desc, renderSchema(t.remote.InputSchema))
}

// Call implements tools.Tool by parsing the model's string into structured
// arguments before invoking the tool.
func (t *Tool) Call(ctx context.Context, input string) (string, error) {
	args, err := ParseArgs(input, t.remote.InputSchema)
	if err != nil {
		return "", err
	}
	return t.Invoke(ctx, args)
}

// Invoke calls the remote tool with already-structured arguments.
//
// A tool that reports failure (a rejected query, a missing table) comes back as
// ordinary text, not a Go error: the model needs to read it to correct itself.
// Only transport and protocol failures return an error.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (string, error) {
	res, err := t.session.session.CallTool(ctx, &sdk.CallToolParams{
		Name:      t.remote.Name,
		Arguments: args,
	})
	if err != nil {
		return "", fmt.Errorf("mcp: call %s: %w", t.remote.Name, err)
	}

	out := flattenContent(res)
	if res.IsError {
		return "Tool reported an error: " + out, nil
	}
	if out == "" {
		return "(tool returned no content)", nil
	}
	return truncate(out, maxResultChars), nil
}

// flattenContent renders an MCP result as text, preferring the content blocks
// and falling back to the structured payload.
func flattenContent(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*sdk.TextContent); ok {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(text.Text)
		}
	}

	if b.Len() == 0 && res.StructuredContent != nil {
		if encoded, err := json.Marshal(res.StructuredContent); err == nil {
			return string(encoded)
		}
	}

	return strings.TrimSpace(b.String())
}
