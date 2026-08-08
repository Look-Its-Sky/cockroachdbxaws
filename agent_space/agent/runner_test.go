package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/schema"
	"github.com/tmc/langchaingo/vectorstores"

	"agent_space/utils/mcp"
	"agent_space/utils/mcp/mcptest"
)

// scriptedModel returns pre-written responses in order, recording the message
// history it was given. This exercises the loop's control flow — history
// construction, tool dispatch, termination — without spending tokens or
// depending on a model's mood.
type scriptedModel struct {
	mu        sync.Mutex
	responses []*llms.ContentResponse
	err       error

	calls    int
	lastMsgs []llms.MessageContent
	lastOpts []llms.CallOption
}

func (m *scriptedModel) GenerateContent(_ context.Context, messages []llms.MessageContent, opts ...llms.CallOption) (*llms.ContentResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.lastMsgs = messages
	m.lastOpts = opts
	m.calls++

	if m.err != nil {
		return nil, m.err
	}
	if m.calls > len(m.responses) {
		// Standing in for a model that stops calling tools: return the last
		// response again rather than failing the test with an index panic.
		return m.responses[len(m.responses)-1], nil
	}
	return m.responses[m.calls-1], nil
}

func (m *scriptedModel) Call(context.Context, string, ...llms.CallOption) (string, error) {
	return "", errors.New("not used")
}

func textResponse(s string) *llms.ContentResponse {
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: s, StopReason: "stop"}}}
}

func toolResponse(id, name, args string) *llms.ContentResponse {
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{
		StopReason: "tool_calls",
		ToolCalls: []llms.ToolCall{{
			ID:           id,
			Type:         "function",
			FunctionCall: &llms.FunctionCall{Name: name, Arguments: args},
		}},
	}}}
}

// stubStore returns fixed incident text for recall.
type stubStore struct {
	docs []string
	err  error
}

func (s stubStore) AddDocuments(context.Context, []schema.Document, ...vectorstores.Option) ([]string, error) {
	return nil, nil
}

func (s stubStore) SimilaritySearch(_ context.Context, _ string, _ int, _ ...vectorstores.Option) ([]schema.Document, error) {
	if s.err != nil {
		return nil, s.err
	}
	docs := make([]schema.Document, 0, len(s.docs))
	for _, d := range s.docs {
		docs = append(docs, schema.Document{PageContent: d})
	}
	return docs, nil
}

// newRunner wires a scripted model to real MCP tools served by the fake, so the
// tool path is exercised end to end rather than mocked out.
func newRunner(t *testing.T, model llms.Model, store vectorstores.VectorStore) (*Runner, *mcptest.Server) {
	t.Helper()

	fake := mcptest.Start(t)
	session, err := mcp.Connect(t.Context(), mcp.Config{URL: fake.URL, APIKey: "test-key"})
	if err != nil {
		t.Fatalf("mcp.Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	return New(model, store, session.Tools()), fake
}

func TestRunAnswersWithoutTools(t *testing.T) {
	model := &scriptedModel{responses: []*llms.ContentResponse{
		textResponse("ROLLBACK. The implicated commit is a3f9c21."),
	}}
	store := stubStore{docs: []string{"INC-412: rolled back a3f9c21", "INC-388: hotfixed b1d0e44"}}

	runner, fake := newRunner(t, model, store)
	res, err := runner.Run(t.Context(), "Checkout is 500ing after this morning's deploy.", 2)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Answer != "ROLLBACK. The implicated commit is a3f9c21." {
		t.Errorf("Answer = %q", res.Answer)
	}
	if res.Sources != 2 {
		t.Errorf("Sources = %d, want 2", res.Sources)
	}
	if res.Iterations != 1 {
		t.Errorf("Iterations = %d, want 1", res.Iterations)
	}
	if len(res.Trace) != 0 {
		t.Errorf("Trace = %v, want empty", res.Trace)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("called MCP tools when none were requested: %v", fake.Calls())
	}

	// The recalled incidents must actually reach the prompt, or the vector
	// store is decorative.
	prompt := renderMessages(model.lastMsgs)
	for _, want := range []string{"INC-412", "INC-388", "Checkout is 500ing"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
}

func TestRunExecutesToolCallThenAnswers(t *testing.T) {
	model := &scriptedModel{responses: []*llms.ContentResponse{
		toolResponse("call-1", "select_query", `{"query": "SELECT count(*) FROM incidents"}`),
		textResponse("HOTFIX. The implicated commit is b1d0e44."),
	}}

	runner, fake := newRunner(t, model, stubStore{docs: []string{"INC-412: rolled back a3f9c21"}})
	res, err := runner.Run(t.Context(), "Latency spike on the orders service.", 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Iterations != 2 {
		t.Errorf("Iterations = %d, want 2", res.Iterations)
	}
	if res.Truncated {
		t.Error("Truncated = true, want false")
	}
	if len(res.Trace) != 1 {
		t.Fatalf("Trace has %d steps, want 1", len(res.Trace))
	}

	step := res.Trace[0]
	if step.Tool != "select_query" || step.Failed {
		t.Errorf("step = %+v, want a successful select_query", step)
	}
	if step.Arguments["query"] != "SELECT count(*) FROM incidents" {
		t.Errorf("step arguments = %v", step.Arguments)
	}
	if !strings.Contains(step.Output, "ran: SELECT count(*)") {
		t.Errorf("step output = %q", step.Output)
	}

	// The call must have reached the server, not just the trace.
	calls := fake.Calls()
	if len(calls) != 1 || calls[0].Tool != "select_query" {
		t.Errorf("server received %+v, want one select_query", calls)
	}

	// The tool result has to be fed back with its tool_call_id, or the
	// provider rejects the follow-up turn.
	if !hasToolResponse(model.lastMsgs, "call-1", "ran: SELECT count(*) FROM incidents") {
		t.Errorf("tool result was not fed back to the model:\n%s", renderMessages(model.lastMsgs))
	}
}

func TestRunFeedsBadArgumentsBackToModel(t *testing.T) {
	// get_table_schema needs two fields, so this is genuinely unparseable and the
	// model must be told why rather than the run aborting.
	model := &scriptedModel{responses: []*llms.ContentResponse{
		toolResponse("call-1", "get_table_schema", "tell me the columns"),
		textResponse("ROLLBACK. The implicated commit is c7e1a09."),
	}}

	runner, fake := newRunner(t, model, stubStore{})
	res, err := runner.Run(t.Context(), "Orders table looks wrong.", 1)
	if err != nil {
		t.Fatalf("Run should recover from bad arguments, got: %v", err)
	}

	if len(res.Trace) != 1 || !res.Trace[0].Failed {
		t.Fatalf("Trace = %+v, want one failed step", res.Trace)
	}
	for _, want := range []string{"database", "schema"} {
		if !strings.Contains(res.Trace[0].Output, want) {
			t.Errorf("failure output does not name %q, so the model cannot retry: %q", want, res.Trace[0].Output)
		}
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("unparseable arguments reached the server: %v", fake.Calls())
	}
	if res.Answer != "ROLLBACK. The implicated commit is c7e1a09." {
		t.Errorf("Answer = %q, want the run to continue after the bad call", res.Answer)
	}
}

func TestRunRecordsToolLevelErrorAsFailedButContinues(t *testing.T) {
	// A refused query is the server's answer, not a transport failure: the run
	// carries on and the model reads the rejection. But the step is a failure,
	// and the trace has to say so — a live run once made six calls against a
	// database that did not exist and reported every one as a success.
	model := &scriptedModel{responses: []*llms.ContentResponse{
		toolResponse("call-1", "select_query", `{"query": "DROP TABLE incidents"}`),
		textResponse("HOTFIX. The implicated commit is d4b8f31."),
	}}

	runner, _ := newRunner(t, model, stubStore{})
	res, err := runner.Run(t.Context(), "Can we drop the table?", 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.Trace) != 1 {
		t.Fatalf("Trace has %d steps, want 1", len(res.Trace))
	}
	if !res.Trace[0].Failed {
		t.Error("a tool-level rejection was recorded as a successful step")
	}
	// The rejection must still reach the model, or it cannot correct itself.
	if !strings.Contains(res.Trace[0].Output, "only SELECT statements are permitted") {
		t.Errorf("step output = %q, want the server's rejection", res.Trace[0].Output)
	}
	if res.Answer != "HOTFIX. The implicated commit is d4b8f31." {
		t.Errorf("Answer = %q, want the run to continue past the rejection", res.Answer)
	}
}

func TestRunRejectsUnknownTool(t *testing.T) {
	model := &scriptedModel{responses: []*llms.ContentResponse{
		toolResponse("call-1", "drop_everything", `{}`),
		textResponse("ROLLBACK. The implicated commit is e5c2b17."),
	}}

	runner, fake := newRunner(t, model, stubStore{})
	res, err := runner.Run(t.Context(), "What happened?", 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.Trace) != 1 || !res.Trace[0].Failed {
		t.Fatalf("Trace = %+v, want one failed step", res.Trace)
	}
	// The recovery message must list what is actually available.
	if !strings.Contains(res.Trace[0].Output, "select_query") {
		t.Errorf("unknown-tool message does not list available tools: %q", res.Trace[0].Output)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("an unknown tool reached the server: %v", fake.Calls())
	}
}

func TestRunStopsAtIterationCap(t *testing.T) {
	// A model that never stops calling tools must be cut off with its findings
	// intact rather than looping or returning nothing.
	model := &scriptedModel{responses: []*llms.ContentResponse{
		toolResponse("call-1", "select_query", `{"query": "SELECT 1"}`),
	}}

	runner, _ := newRunner(t, model, stubStore{})
	runner.MaxIterations = 3

	res, err := runner.Run(t.Context(), "Why is the service down?", 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !res.Truncated {
		t.Error("Truncated = false, want true after exhausting iterations")
	}
	if res.Iterations != 3 {
		t.Errorf("Iterations = %d, want 3", res.Iterations)
	}
	if len(res.Trace) != 3 {
		t.Errorf("Trace has %d steps, want 3", len(res.Trace))
	}
	if res.Answer == "" {
		t.Error("Answer is empty; a truncated run must still report something")
	}
}

func TestRunPassesRealSchemasToModel(t *testing.T) {
	// The point of the native loop: the model receives each tool's actual JSON
	// Schema, not langchaingo's flattened single-string stand-in.
	model := &scriptedModel{responses: []*llms.ContentResponse{textResponse("done")}}

	runner, _ := newRunner(t, model, stubStore{})
	if _, err := runner.Run(t.Context(), "status?", 1); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var opts llms.CallOptions
	for _, o := range model.lastOpts {
		o(&opts)
	}

	// Derived from the session rather than hard-coded, so adding a fixture
	// tool does not fail a test that is really about schema fidelity.
	if len(opts.Tools) != len(runner.Tools) {
		t.Fatalf("model was given %d tools, want all %d", len(opts.Tools), len(runner.Tools))
	}
	for _, tool := range opts.Tools {
		params, ok := tool.Function.Parameters.(map[string]any)
		if !ok {
			t.Fatalf("%s parameters = %T, want the MCP schema", tool.Function.Name, tool.Function.Parameters)
		}
		// Every function must carry a properties object, even the no-argument
		// one: LM Studio 400s the whole request if any of them omits it.
		props, ok := params["properties"].(map[string]any)
		if !ok {
			t.Errorf("%s has no properties object: %v", tool.Function.Name, params)
			continue
		}
		if tool.Function.Name != "get_cluster" && len(props) == 0 {
			t.Errorf("%s lost its schema properties: %v", tool.Function.Name, params)
		}
		if _, flattened := props["__arg1"]; flattened {
			t.Errorf("%s schema was flattened to __arg1", tool.Function.Name)
		}
	}
}

func TestRunRejectsEmptyQuestion(t *testing.T) {
	runner, _ := newRunner(t, &scriptedModel{responses: []*llms.ContentResponse{textResponse("x")}}, stubStore{})

	if _, err := runner.Run(t.Context(), "   ", 1); err == nil {
		t.Error("Run with an empty question succeeded, want an error")
	}
}

func TestRunPropagatesRecallFailure(t *testing.T) {
	runner, _ := newRunner(t,
		&scriptedModel{responses: []*llms.ContentResponse{textResponse("x")}},
		stubStore{err: errors.New("cluster unreachable")})

	_, err := runner.Run(t.Context(), "What broke?", 1)
	if err == nil || !strings.Contains(err.Error(), "cluster unreachable") {
		t.Errorf("Run error = %v, want the recall failure surfaced", err)
	}
}

// renderMessages flattens a message history to text for assertions.
func renderMessages(msgs []llms.MessageContent) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(string(m.Role))
		b.WriteString(": ")
		for _, p := range m.Parts {
			switch v := p.(type) {
			case llms.TextContent:
				b.WriteString(v.Text)
			case llms.ToolCall:
				if v.FunctionCall != nil {
					b.WriteString("[call " + v.FunctionCall.Name + " " + v.FunctionCall.Arguments + "]")
				}
			case llms.ToolCallResponse:
				b.WriteString("[result " + v.ToolCallID + " " + v.Content + "]")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func hasToolResponse(msgs []llms.MessageContent, id, contains string) bool {
	for _, m := range msgs {
		if m.Role != llms.ChatMessageTypeTool {
			continue
		}
		for _, p := range m.Parts {
			r, ok := p.(llms.ToolCallResponse)
			if ok && r.ToolCallID == id && strings.Contains(r.Content, contains) {
				return true
			}
		}
	}
	return false
}
