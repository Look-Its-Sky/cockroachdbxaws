package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/vectorstores"

	"agent_space/utils/mcp"
)

const (
	// recall -> inspect schema -> query -> decide + recovery
	defaultMaxIterations = 6

	// defaultSources is how many past incidents to recall, matching /ask.
	defaultSources = 4

	// how many past decisions to quote. Deliberately far fewer than incidents:
	// these are opinions, and the more of them are in the prompt the more the
	// live evidence has to argue against.
	maxPrecedents = 2

	// maxToolOutputChars caps what one tool result contributes to the next prompt
	maxToolOutputChars = 4000
	completionBudget   = 4000 // includes thinking tokens
)

const systemPrompt = `You are an SRE incident-response agent for a production service backed by CockroachDB.

Your job, for each incident:
1. Identify the commit that most likely caused the failure, and name it explicitly.
2. Decide ROLLBACK or HOTFIX, and say which:
   - ROLLBACK when a correct fix cannot land in time to stop the bleeding.
   - HOTFIX when a fix can land the same day.
3. Justify the decision using the past incidents provided as context, citing the
   ones you relied on.

You have read-only tools that query the live CockroachDB cluster. Use them to
check the current state of the data before deciding — a decision grounded only
in past incidents is a guess about the present.

Spend tool calls on application data: the tables the failing service reads and
writes. Start with list_tables and get_table_schema to find them, then a narrow
SELECT. Do not inspect cluster infrastructure — node lists, running queries, job
and migration history — unless the incident is specifically about cluster health.
Those return large results that crowd out the incident context and rarely bear on
whether to roll a commit back.

Two or three well-chosen queries are enough. When you have what you need, answer
in prose. State the decision and the commit in the first sentence. Do not call a
tool once you can answer.`

// PrecedentSource recalls decisions this team made on similar incidents.
//
// An interface rather than the concrete type so this package does not depend on
// the remediation half: the investigation runs perfectly well without one, and
// nil means exactly the behaviour that existed before decisions were recorded.
type PrecedentSource interface {
	Recall(ctx context.Context, query string, limit int) ([]string, error)
}

type Runner struct {
	Model         llms.Model
	Store         vectorstores.VectorStore
	Tools         []*mcp.Tool
	MaxIterations int

	// Precedents is what engineers decided last time. Nil disables it.
	Precedents PrecedentSource

	// tables read at boot, empty means the model discovers them itself
	Schema string

	specs  []llms.Tool
	byName map[string]*mcp.Tool
	once   sync.Once
}

// New builds a Runner over the discovered MCP tools.
func New(model llms.Model, store vectorstores.VectorStore, tools []*mcp.Tool) *Runner {
	return &Runner{
		Model:         model,
		Store:         store,
		Tools:         tools,
		MaxIterations: defaultMaxIterations,
	}
}

// build tool specs once, passing each tool's real JSON Schema to the model
func (r *Runner) init() {
	r.once.Do(func() {
		r.byName = make(map[string]*mcp.Tool, len(r.Tools))
		r.specs = make([]llms.Tool, 0, len(r.Tools))

		for _, t := range r.Tools {
			r.byName[t.Name()] = t
			r.specs = append(r.specs, llms.Tool{
				Type: "function",
				Function: &llms.FunctionDefinition{
					Name:        t.Name(),
					Description: t.Description(),
					Parameters:  t.Schema(),
				},
			})
		}

		if r.MaxIterations <= 0 {
			r.MaxIterations = defaultMaxIterations
		}
	})
}

// Run answers a question, consulting past incidents and the live cluster.
func (r *Runner) Run(ctx context.Context, question string, limit int) (Result, error) {
	r.init()

	if strings.TrimSpace(question) == "" {
		return Result{}, errors.New("agent: question is empty")
	}
	if limit <= 0 {
		limit = defaultSources
	}

	grounding, err := r.recall(ctx, question, limit)
	if err != nil {
		return Result{}, err
	}

	// past decisions are a bonus, never a prerequisite: an index that is missing
	// or unreachable must not stop an incident being investigated
	precedents := r.recallPrecedents(ctx, question)

	result := Result{Sources: len(grounding), Grounding: grounding, Precedents: precedents}
	messages := []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, r.instructions()),
		llms.TextParts(llms.ChatMessageTypeHuman, buildPrompt(question, grounding, precedents)),
	}

	for i := 1; i <= r.MaxIterations; i++ {
		result.Iterations = i

		resp, err := r.Model.GenerateContent(ctx, messages,
			llms.WithTools(r.specs),
			llms.WithMaxTokens(completionBudget),
		)
		if err != nil {
			return result, fmt.Errorf("agent: generate: %w", err)
		}
		if len(resp.Choices) == 0 {
			return result, errors.New("agent: model returned no choices")
		}

		choice := resp.Choices[0]
		calls := toolCalls(choice)
		if len(calls) == 0 {
			answer := strings.TrimSpace(choice.Content)
			if answer == "" {
				// reasoning models can spend the whole budget thinking and return nothing
				answer = "The model returned no answer (stop reason: " + choice.StopReason + ")."
			}
			result.setAnswer(answer)
			return result, nil
		}

		// echo the tool-call turn back or the results have no tool_call_id to attach to
		messages = append(messages, assistantTurn(choice, calls))

		steps, err := r.execute(ctx, i, calls)
		result.Trace = append(result.Trace, steps...)
		if err != nil {
			// cluster is gone, return the trace with the error rather than a partial answer
			return result, err
		}
		for j, step := range steps {
			messages = append(messages, llms.MessageContent{
				Role: llms.ChatMessageTypeTool,
				Parts: []llms.ContentPart{llms.ToolCallResponse{
					ToolCallID: calls[j].ID,
					Name:       step.Tool,
					Content:    step.Output,
				}},
			})
		}
	}

	// out of iterations, a half-finished investigation is still evidence
	result.Truncated = true
	result.setAnswer(r.summarise(ctx, messages))
	return result, nil
}

// system prompt plus the schema read at boot, so queries are not spent finding tables
func (r *Runner) instructions() string {
	if strings.TrimSpace(r.Schema) == "" {
		return systemPrompt + verdictInstruction
	}

	return systemPrompt + `

The live cluster's application tables are below. They were read at startup, so
they are current and you do NOT need to call list_databases, list_tables or
get_table_schema to find them. Go straight to the SELECT that answers the
question. Only fall back to those tools if something you need is genuinely
missing here.

` + r.Schema + verdictInstruction
}

// recall pulls the most similar past incidents out of the vector index.
func (r *Runner) recall(ctx context.Context, question string, limit int) ([]string, error) {
	if r.Store == nil {
		return nil, nil
	}

	docs, err := r.Store.SimilaritySearch(ctx, question, limit)
	if err != nil {
		return nil, fmt.Errorf("agent: recall past incidents: %w", err)
	}

	grounding := make([]string, 0, len(docs))
	for _, d := range docs {
		grounding = append(grounding, d.PageContent)
	}
	return grounding, nil
}

// recallPrecedents pulls what engineers decided on similar incidents.
//
// Errors are logged and dropped rather than returned. Past decisions improve an
// investigation; they are not required for one, and an index that is down must
// not cost a verdict.
func (r *Runner) recallPrecedents(ctx context.Context, question string) []string {
	if r.Precedents == nil {
		return nil
	}

	found, err := r.Precedents.Recall(ctx, question, maxPrecedents)
	if err != nil {
		log.Printf("agent: could not recall past decisions, continuing without them: %v", err)
		return nil
	}
	return found
}

// run the calls concurrently, keeping order so tool_call_ids line up; the error is the first transport failure
func (r *Runner) execute(ctx context.Context, iteration int, calls []llms.ToolCall) ([]Step, error) {
	steps := make([]Step, len(calls))
	errs := make([]error, len(calls))

	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func(i int, call llms.ToolCall) {
			defer wg.Done()
			steps[i], errs[i] = r.invoke(ctx, iteration, call)
		}(i, call)
	}
	wg.Wait()

	// ordered rather than first-to-fail so a run aborts on the same call every time
	for _, err := range errs {
		if err != nil {
			return steps, err
		}
	}
	return steps, nil
}

// bad arguments and rejections come back as text the model reads; only a dead session errors
func (r *Runner) invoke(ctx context.Context, iteration int, call llms.ToolCall) (Step, error) {
	start := time.Now()
	step := Step{Iteration: iteration}

	fail := func(cause Cause, output string) (Step, error) {
		step.Failed = true
		step.Cause = cause
		step.Output = output
		step.DurationMS = elapsedMS(start)
		return step, nil
	}

	if call.FunctionCall == nil {
		return fail(CauseMalformedCall, "Malformed tool call: no function specified.")
	}

	step.Tool = call.FunctionCall.Name
	step.RawInput = call.FunctionCall.Arguments

	tool, ok := r.byName[step.Tool]
	if !ok {
		return fail(CauseUnknownTool,
			fmt.Sprintf("No tool named %q. Available tools: %s.", step.Tool, strings.Join(r.names(), ", ")))
	}

	args, err := mcp.ParseArgs(call.FunctionCall.Arguments, tool.Schema())
	if err != nil {
		return fail(CauseInvalidArguments, err.Error())
	}
	step.Arguments = args

	res, err := tool.InvokeResult(ctx, args)
	if err != nil {
		// a rejected call is a verdict on that call, not the session, so keep going
		if !mcp.IsSessionFailure(err) {
			return fail(CauseRejected, "Tool call rejected: "+err.Error())
		}
		// record the step so the trace shows how far the run got, then stop
		s, _ := fail(CauseTransport, "Tool call failed: "+err.Error())
		return s, fmt.Errorf("%w: %w", ErrTransport, err)
	}

	// a tool that ran and said no is still a failed step, or the trace hides the pattern
	if res.IsError {
		return fail(CauseToolError, clip(res.Text, maxToolOutputChars))
	}

	step.Output = clip(res.Text, maxToolOutputChars)
	step.DurationMS = elapsedMS(start)
	return step, nil
}

func (r *Runner) names() []string {
	names := make([]string, 0, len(r.Tools))
	for _, t := range r.Tools {
		names = append(names, t.Name())
	}
	return names
}

// final answer with tools withheld, so a model stuck in a loop has to decide
func (r *Runner) summarise(ctx context.Context, messages []llms.MessageContent) string {
	messages = append(messages, llms.TextParts(llms.ChatMessageTypeHuman,
		"You have run out of investigation steps. Using only what you have gathered, state your ROLLBACK or HOTFIX decision and the implicated commit now. Do not call any more tools."))

	resp, err := r.Model.GenerateContent(ctx, messages, llms.WithMaxTokens(completionBudget))
	if err != nil || len(resp.Choices) == 0 {
		return "The investigation exceeded its step budget before reaching a decision. See the trace for what was gathered."
	}

	answer := strings.TrimSpace(resp.Choices[0].Content)
	if answer == "" {
		return "The investigation exceeded its step budget before reaching a decision. See the trace for what was gathered."
	}
	return answer
}

// normalise the two shapes providers use: tool_calls and the legacy function_call
func toolCalls(choice *llms.ContentChoice) []llms.ToolCall {
	if len(choice.ToolCalls) > 0 {
		return choice.ToolCalls
	}
	if choice.FuncCall != nil {
		return []llms.ToolCall{{Type: "function", FunctionCall: choice.FuncCall}}
	}
	return nil
}

// assistantTurn reconstructs the model's tool-calling message for the history.
func assistantTurn(choice *llms.ContentChoice, calls []llms.ToolCall) llms.MessageContent {
	parts := make([]llms.ContentPart, 0, len(calls)+1)
	if text := strings.TrimSpace(choice.Content); text != "" {
		parts = append(parts, llms.TextContent{Text: text})
	}
	for _, call := range calls {
		parts = append(parts, call)
	}

	return llms.MessageContent{Role: llms.ChatMessageTypeAI, Parts: parts}
}

// buildPrompt frames the question with the recalled incidents, and with what
// this team decided the last time something like it happened.
//
// The two are kept in separate sections and labelled differently on purpose.
// Incidents are history; decisions are opinion, and a model given both without
// being told which is which will treat a colleague's judgement as a fact about
// the world.
func buildPrompt(question string, grounding, precedents []string) string {
	var b strings.Builder

	if len(grounding) == 0 {
		b.WriteString("No similar past incidents were found in the incident store.\n\n")
	} else {
		b.WriteString("Similar past incidents recalled from the CockroachDB incident store:\n")
		for i, g := range grounding {
			fmt.Fprintf(&b, "%d. %s\n", i+1, g)
		}
		b.WriteString("\n")
	}

	if len(precedents) > 0 {
		b.WriteString("How this team decided similar cases before. These are judgements " +
			"made by the engineers you work for, recorded after they reviewed proposed " +
			"fixes. Weigh them — they tell you what this team values and what it has " +
			"rejected before. Do not follow one against the evidence in front of you: " +
			"the current incident is what is being decided, and a past decision that " +
			"does not fit it is not a precedent.\n")
		for i, p := range precedents {
			fmt.Fprintf(&b, "\n--- past decision %d ---\n%s\n", i+1, p)
		}
		b.WriteString("\n")
	}

	b.WriteString("Current incident: ")
	b.WriteString(question)
	return b.String()
}

func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (output truncated)"
}

var _ = func() bool {
	if _, err := json.Marshal(Step{}); err != nil {
		panic(err)
	}
	return true
}()
