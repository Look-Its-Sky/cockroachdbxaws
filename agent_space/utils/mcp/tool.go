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

// clusterIDArg is the argument the Cloud server publishes on every
// cluster-scoped tool, to be used only when the session is not already scoped.
const clusterIDArg = "cluster_id"

// Schema returns the tool's JSON Schema, ready to pass through as an OpenAI
// function's parameters.
//
// When the session carries a cluster ID, cluster_id is removed first: see
// withoutProperty for why the model must not be shown it.
func (t *Tool) Schema() any {
	schema := normalizeSchema(t.remote.InputSchema)
	if t.scoped() {
		schema = withoutProperty(schema, clusterIDArg)
	}
	return schema
}

// scoped reports whether this session pins a cluster via the mcp-cluster-id
// header, which changes what the server will accept as arguments.
func (t *Tool) scoped() bool { return t.session != nil && t.session.ClusterID() != "" }

// withoutProperty returns the schema with one property removed, along with any
// mention of it in "required".
//
// The Cloud server publishes cluster_id on every cluster-scoped tool and
// documents it as "Required when the MCP config has no cluster_id; otherwise
// must be omitted". We send the scope as a header, so the argument must never
// be sent — and the server rejects the entire call when it is, rather than
// ignoring it. A model shown the property will eventually fill it in: it did
// exactly that here, calling list_clusters, reading the real UUID out of the
// result, and passing it to list_databases in good faith.
//
// Hiding the property makes that mistake unrepresentable instead of relying on
// the model to infer a rule it was never told.
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
	// A null "required" is not valid JSON Schema, so drop the key entirely
	// when nothing is left in it.
	if req := withoutString(m["required"], name); req != nil {
		out["required"] = req
	} else {
		delete(out, "required")
	}
	return out
}

// withoutString drops a name from a JSON Schema "required" list, which decodes
// as []any or []string depending on how it reached us. Returns nil when the
// list becomes empty, so an empty "required" is omitted rather than sent.
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
	// Schema() rather than the raw InputSchema, so a scoped session does not
	// advertise cluster_id here either.
	return fmt.Sprintf("%s\nInput must be a JSON object matching this schema: %s", desc, renderSchema(t.Schema()))
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

// Result is the outcome of one tool call. IsError distinguishes "the tool ran
// and said no" from "the tool ran and answered" — both of which come back as
// text the model can read, and neither of which is a Go error.
type Result struct {
	Text    string
	IsError bool
}

// Invoke calls the remote tool with already-structured arguments, returning
// just the text. Callers that need to know whether the tool itself reported a
// failure should use InvokeResult: a rejected query and a successful one are
// indistinguishable here, which is fine for feeding the model and wrong for
// anything that counts failures.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (string, error) {
	res, err := t.InvokeResult(ctx, args)
	return res.Text, err
}

// InvokeResult calls the remote tool and reports whether the tool rejected the
// call.
//
// A tool that reports failure (a rejected query, a missing table) comes back as
// ordinary text rather than a Go error: the model needs to read it to correct
// itself, and aborting the run would deny it that chance. Only transport and
// protocol failures return an error.
func (t *Tool) InvokeResult(ctx context.Context, args map[string]any) (Result, error) {
	// Belt and braces alongside hiding it from the schema: the tools.Tool path
	// takes free-form JSON, so a model can still invent cluster_id even when it
	// was never offered. Sending it next to the header is a hard rejection of
	// the whole call, and silently dropping it is exactly right — the header
	// already carries the same scope.
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
