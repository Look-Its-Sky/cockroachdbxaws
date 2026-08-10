package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tmc/langchaingo/tools"
)

// bounds what one tool result contributes to the next prompt
const maxResultChars = 6000

// one remote MCP tool, usable as Schema+Invoke for the native loop or as langchaingo's tools.Tool
type Tool struct {
	remote  *sdk.Tool
	session *Session
}

var _ tools.Tool = (*Tool)(nil)

// Name returns the MCP tool name.
func (t *Tool) Name() string { return t.remote.Name }

// published on every cluster-scoped tool, for use only when the session is not already scoped
const clusterIDArg = "cluster_id"

// the tool's JSON Schema as OpenAI function parameters; cluster_id is stripped when the session is scoped
func (t *Tool) Schema() any {
	schema := normalizeSchema(t.remote.InputSchema)
	if t.scoped() {
		schema = withoutProperty(schema, clusterIDArg)
	}
	return schema
}

// whether this session pins a cluster via the header, which changes what arguments the server accepts
func (t *Tool) scoped() bool { return t.session != nil && t.session.ClusterID() != "" }

// drop a property and any mention of it in "required". The server rejects a scoped call that also passes cluster_id, and a model shown the property will fill it in, so hiding it makes the mistake unrepresentable.
func withoutProperty(schema any, name string) any {
	m, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	props, ok := m["properties"].(map[string]any)
	if !ok {
		return schema
	}
	if _, present := props[name]; !present {
		return schema
	}

	// Copy: the schema map is derived from the SDK's Tool and shared per call.
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}

	trimmed := make(map[string]any, len(props))
	for k, v := range props {
		if k != name {
			trimmed[k] = v
		}
	}
	out["properties"] = trimmed
	// a null "required" is not valid JSON Schema, so drop the key when nothing is left
	if req := withoutString(m["required"], name); req != nil {
		out["required"] = req
	} else {
		delete(out, "required")
	}
	return out
}

// drop a name from a "required" list, which decodes as []any or []string; nil when it empties
func withoutString(required any, name string) any {
	keep := func(s string) bool { return s != name }

	switch list := required.(type) {
	case []any:
		out := make([]any, 0, len(list))
		for _, v := range list {
			if s, ok := v.(string); !ok || keep(s) {
				out = append(out, v)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []string:
		out := make([]string, 0, len(list))
		for _, s := range list {
			if keep(s) {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	default:
		return required
	}
}

// fill in what MCP leaves optional but strict servers demand: LM Studio 400s the whole batch when a no-argument tool omits "properties"
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

// the server's own behavioural hints, nil when it declares none
func (t *Tool) Annotations() *sdk.ToolAnnotations { return t.remote.Annotations }

// the server's description with the schema appended, the only channel tools.Tool consumers have
func (t *Tool) Description() string {
	desc := strings.TrimSpace(t.remote.Description)
	if desc == "" {
		desc = "CockroachDB Cloud MCP tool " + t.remote.Name + "."
	}
	// Schema() rather than the raw InputSchema, so a scoped session does not advertise cluster_id here either
	return fmt.Sprintf("%s\nInput must be a JSON object matching this schema: %s", desc, renderSchema(t.Schema()))
}

// implements tools.Tool by parsing the model's string into structured arguments
func (t *Tool) Call(ctx context.Context, input string) (string, error) {
	args, err := ParseArgs(input, t.remote.InputSchema)
	if err != nil {
		return "", err
	}
	return t.Invoke(ctx, args)
}

// IsError distinguishes "the tool ran and said no" from "the tool ran and answered"; neither is a Go error
type Result struct {
	Text    string
	IsError bool
}

// text only; callers that need to count failures want InvokeResult, where a rejected query is distinguishable
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (string, error) {
	res, err := t.InvokeResult(ctx, args)
	return res.Text, err
}

// a tool reporting failure comes back as text so the model can correct itself; only transport and protocol failures error
func (t *Tool) InvokeResult(ctx context.Context, args map[string]any) (Result, error) {
	// belt and braces alongside hiding it from the schema: the tools.Tool path takes free-form JSON, and sending cluster_id beside the header rejects the whole call
	if t.scoped() {
		if _, present := args[clusterIDArg]; present {
			trimmed := make(map[string]any, len(args))
			for k, v := range args {
				if k != clusterIDArg {
					trimmed[k] = v
				}
			}
			args = trimmed
		}
	}

	res, err := t.session.session.CallTool(ctx, &sdk.CallToolParams{
		Name:      t.remote.Name,
		Arguments: args,
	})
	if err != nil {
		return Result{}, fmt.Errorf("mcp: call %s: %w", t.remote.Name, err)
	}

	out := flattenContent(res)
	if res.IsError {
		return Result{Text: "Tool reported an error: " + out, IsError: true}, nil
	}
	if out == "" {
		return Result{Text: "(tool returned no content)"}, nil
	}
	return Result{Text: truncate(out, maxResultChars)}, nil
}

// render a result as text, preferring content blocks and falling back to the structured payload
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
