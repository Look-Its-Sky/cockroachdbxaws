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

// pre-written responses in order, exercising the loop's control flow without extra tokens
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
		// the runner may ask for more turns than a test scripts, up to MaxIterations plus summarise; repeating the last response is also how a model that never stops calling tools is expressed
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

// a scripted model over real MCP tools, so the tool path runs end to end rather than mocked
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

	// the recalled incidents must reach the prompt or the vector store is decorative
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
	if step.Tool != "select_query" || step.Failed || step.Cause != "" {
		t.Errorf("step = %+v, want a successful select_query with no cause", step)
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

	// the result must come back with its tool_call_id or the provider rejects the follow-up turn
	if !hasToolResponse(model.lastMsgs, "call-1", "ran: SELECT count(*) FROM incidents") {
		t.Errorf("tool result was not fed back to the model:\n%s", renderMessages(model.lastMsgs))
	}
}

func TestRunFeedsBadArgumentsBackToModel(t *testing.T) {
	// get_table_schema needs two fields, so this is genuinely unparseable
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
	if got := res.Trace[0].Cause; got != CauseInvalidArguments {
		t.Errorf("Cause = %q, want %q", got, CauseInvalidArguments)
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
	// a refused query is the server's answer, not a transport failure; the run carries on but the step is still a failure and the trace has to say so
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
	// tool_error, not transport: the server answered, so the run carries on
	if got := res.Trace[0].Cause; got != CauseToolError {
		t.Errorf("Cause = %q, want %q", got, CauseToolError)
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
	if got := res.Trace[0].Cause; got != CauseUnknownTool {
		t.Errorf("Cause = %q, want %q", got, CauseUnknownTool)
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
	// a model that never stops must be cut off with its findings intact
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
	// the point of the native loop: the model gets each tool's real schema, not a flattened stand-in
	model := &scriptedModel{responses: []*llms.ContentResponse{textResponse("done")}}

	runner, _ := newRunner(t, model, stubStore{})
	if _, err := runner.Run(t.Context(), "status?", 1); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var opts llms.CallOptions
	for _, o := range model.lastOpts {
		o(&opts)
	}

	// derived from the session so adding a fixture tool does not fail a test about schema fidelity
	if len(opts.Tools) != len(runner.Tools) {
		t.Fatalf("model was given %d tools, want all %d", len(opts.Tools), len(runner.Tools))
	}
	for _, tool := range opts.Tools {
		params, ok := tool.Function.Parameters.(map[string]any)
		if !ok {
			t.Fatalf("%s parameters = %T, want the MCP schema", tool.Function.Name, tool.Function.Parameters)
		}
		// every function needs a properties object, even the no-argument one, or LM Studio 400s the request
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

func TestRunAbortsOnTransportFailure(t *testing.T) {
	// a dropped session cannot be fixed by retrying and each retry costs a full generate; before this was split out, a dead session ran to the cap and blamed the model's step budget
	fake := mcptest.Start(t)
	session, err := mcp.Connect(t.Context(), mcp.Config{URL: fake.URL, APIKey: "test-key"})
	if err != nil {
		t.Fatalf("mcp.Connect: %v", err)
	}

	model := &scriptedModel{responses: []*llms.ContentResponse{
		toolResponse("call-1", "select_query", `{"query": "SELECT 1"}`),
	}}
	runner := New(model, stubStore{}, session.Tools())
	runner.MaxIterations = 6

	if err := session.Close(); err != nil {
		t.Fatalf("session.Close: %v", err)
	}

	res, err := runner.Run(t.Context(), "Why is the service down?", 1)
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("Run error = %v, want it to wrap ErrTransport", err)
	}

	// One attempt, not six: the run stops instead of re-asking a dead session.
	if model.calls != 1 {
		t.Errorf("model was called %d times, want 1: a dropped session must not be retried", model.calls)
	}
	if len(res.Trace) != 1 {
		t.Fatalf("Trace has %d steps, want 1", len(res.Trace))
	}
	// the partial trace still comes back, as evidence of how far the run got
	if step := res.Trace[0]; !step.Failed || step.Cause != CauseTransport {
		t.Errorf("step = %+v, want a failed step caused by %q", step, CauseTransport)
	}
	if res.Truncated {
		t.Error("Truncated = true, want false: the run was aborted, not cut off at the cap")
	}
}

func TestRunContinuesWhenTheServerRejectsOneCall(t *testing.T) {
	// a blocked schema comes back as a protocol error, identical to a dead session at the call site; treating it as one abandoned a live run one query from an answer
	model := &scriptedModel{responses: []*llms.ContentResponse{
		toolResponse("call-1", "select_query",
			`{"query": "SELECT table_name FROM information_schema.tables"}`),
		textResponse("ROLLBACK. The implicated commit is a91f3c2."),
	}}

	runner, fake := newRunner(t, model, stubStore{})
	res, err := runner.Run(t.Context(), "What broke?", 1)
	if err != nil {
		t.Fatalf("Run aborted on a recoverable rejection: %v", err)
	}

	if len(res.Trace) != 1 {
		t.Fatalf("Trace has %d steps, want 1", len(res.Trace))
	}
	step := res.Trace[0]
	if !step.Failed || step.Cause != CauseRejected {
		t.Errorf("step = %+v, want a failed step caused by %q", step, CauseRejected)
	}
	// The reason has to reach the model, or it cannot pick a different query.
	if !strings.Contains(step.Output, "restricted schema") {
		t.Errorf("step output = %q, want the server's reason", step.Output)
	}
	if res.Answer != "ROLLBACK. The implicated commit is a91f3c2." {
		t.Errorf("Answer = %q, want the run to continue past the rejection", res.Answer)
	}
	// it really reached the server, so this is a rejection and not a client-side argument failure
	if calls := fake.Calls(); len(calls) != 1 {
		t.Errorf("server received %+v, want the one rejected call", calls)
	}
}

func TestSummariseWithholdsTools(t *testing.T) {
	// summarise forces a looping model to commit, so the final call must carry no tools
	model := &scriptedModel{responses: []*llms.ContentResponse{
		toolResponse("call-1", "select_query", `{"query": "SELECT 1"}`),
	}}

	runner, _ := newRunner(t, model, stubStore{})
	runner.MaxIterations = 2

	res, err := runner.Run(t.Context(), "Why is the service down?", 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Truncated {
		t.Fatal("Truncated = false, want the run to have reached summarise")
	}

	// the scripted model never stops calling tools, so the last call it saw is the summarise one
	var opts llms.CallOptions
	for _, o := range model.lastOpts {
		o(&opts)
	}
	if len(opts.Tools) != 0 {
		t.Errorf("summarise offered %d tools, want 0", len(opts.Tools))
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

func TestInstructionsCarrySchemaAndTellTheModelToSkipDiscovery(t *testing.T) {
	runner, _ := newRunner(t, &scriptedModel{responses: []*llms.ContentResponse{textResponse("x")}}, stubStore{})
	runner.Schema = "database defaultdb\n  CREATE TABLE deploys (commit_sha STRING, deployed_at TIMESTAMPTZ)"

	got := runner.instructions()
	if !strings.Contains(got, "CREATE TABLE deploys") {
		t.Errorf("the schema never reached the prompt:\n%s", got)
	}
	// handing over the schema without saying so leaves the model free to keep calling the discovery tools
	for _, want := range []string{"list_tables", "get_table_schema", "do NOT need"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt does not tell the model to skip discovery (missing %q)", want)
		}
	}
}

func TestInstructionsAreUnchangedWithoutASchema(t *testing.T) {
	// a cluster that could not be described must leave behaviour exactly as it was
	runner, _ := newRunner(t, &scriptedModel{responses: []*llms.ContentResponse{textResponse("x")}}, stubStore{})
	runner.Schema = "   "

	// the verdict block is asked for on every path, schema or not; what must
	// not appear is any description of tables that could not be read
	if got := runner.instructions(); got != systemPrompt+verdictInstruction {
		t.Errorf("instructions changed with an empty schema:\n%s", got)
	}
	if strings.Contains(runner.instructions(), "The live cluster's application tables") {
		t.Error("a schema block leaked into the prompt with no schema loaded")
	}
}

func TestRunReportsNotTruncatedWhenTheModelAnswers(t *testing.T) {
	// the counterpart to TestRunStopsAtIterationCap; the false case has to be pinned or the flag only proves one of the two things it distinguishes
	model := &scriptedModel{responses: []*llms.ContentResponse{
		toolResponse("call-1", "select_query", `{"query": "SELECT 1"}`),
		textResponse("ROLLBACK. The implicated commit is a91f3c2."),
	}}

	runner, _ := newRunner(t, model, stubStore{})
	runner.MaxIterations = 6

	res, err := runner.Run(t.Context(), "What broke?", 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Truncated {
		t.Error("Truncated = true although the model answered on its own")
	}
	// Stopped early rather than running to the cap.
	if res.Iterations != 2 {
		t.Errorf("Iterations = %d, want 2: the loop must stop when the answer arrives", res.Iterations)
	}
	if model.calls != 2 {
		t.Errorf("model was called %d times, want 2: no summarise call belongs here", model.calls)
	}
	if res.Answer != "ROLLBACK. The implicated commit is a91f3c2." {
		t.Errorf("Answer = %q, want the model's own words rather than the fallback", res.Answer)
	}
}
