// Package agent runs the SRE decision loop: it grounds a question in past
// incidents recalled from CockroachDB's vector index, then lets the model query
// the live cluster through the CockroachDB Cloud MCP tools before committing to
// a rollback-or-hotfix call.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/vectorstores"

	"agent_space/utils/mcp"
)

const (
	// defaultMaxIterations bounds the tool loop. Six is enough for
	// recall -> inspect schema -> query -> decide with room to recover from a
	// bad call, without letting a confused model spend the budget.
	defaultMaxIterations = 6

	// defaultSources is how many past incidents to recall, matching /ask.
	defaultSources = 4

	// maxToolOutputChars caps what one tool result contributes to the next
	// prompt, so a wide result set cannot crowd out the incident context.
	maxToolOutputChars = 4000

	// completionBudget must leave room for reasoning models that spend
	// completion tokens before emitting any content. At 2000, one iteration in
	// twenty ended with finish_reason "length" mid-reasoning and emitted no
	// tool call at all — the budget ran out before the model got to the part
	// that matters. Failing that way is silent, so the headroom is worth more
	// than the tokens.
	completionBudget = 4000
)

// systemPrompt states the decision rule the project exists to automate. The
// rollback-vs-hotfix criterion is deliberately explicit: it is the judgement
// being encoded, so it belongs in the prompt rather than in the model's priors.
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

// Runner executes agent runs. It is safe for concurrent use.
type Runner struct {
	Model         llms.Model
	Store         vectorstores.VectorStore
	Tools         []*mcp.Tool
	MaxIterations int

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

// init builds the tool specs once. Each tool's real JSON Schema is passed
// straight through to the model — this is the whole reason for the native loop
// rather than langchaingo's agent, which flattens every tool to one string
// argument and leaves the model guessing at the shape.
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

	result := Result{Sources: len(grounding), Grounding: grounding}
	messages := []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, systemPrompt),
		llms.TextParts(llms.ChatMessageTypeHuman, buildPrompt(question, grounding)),
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
			result.Answer = strings.TrimSpace(choice.Content)
			if result.Answer == "" {
				// A reasoning model that spends its whole budget thinking
				// returns empty content; say so rather than returning "".
				result.Answer = "The model returned no answer (stop reason: " + choice.StopReason + ")."
			}
			return result, nil
		}

		// Echo the assistant's tool-call turn back, or the follow-up messages
		// have nothing to attach their tool_call_id to.
		messages = append(messages, assistantTurn(choice, calls))

		steps := r.execute(ctx, i, calls)
		result.Trace = append(result.Trace, steps...)
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

	// Out of iterations. Return the trace and a partial answer rather than an
	// opaque failure — a half-finished investigation is still evidence.
	result.Truncated = true
	result.Answer = r.summarise(ctx, messages)
	return result, nil
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

// execute runs the requested tool calls, concurrently when there is more than
// one, preserving the order the model asked for so tool_call_ids line up.
func (r *Runner) execute(ctx context.Context, iteration int, calls []llms.ToolCall) []Step {
	steps := make([]Step, len(calls))

	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func(i int, call llms.ToolCall) {
			defer wg.Done()
			steps[i] = r.invoke(ctx, iteration, call)
		}(i, call)
	}
	wg.Wait()

	return steps
}

// invoke runs one tool call. Bad arguments and tool-level failures come back as
// Step.Output text rather than errors, because the model reads that text and
// retries; only the run itself failing would justify aborting.
func (r *Runner) invoke(ctx context.Context, iteration int, call llms.ToolCall) Step {
	start := time.Now()
	step := Step{Iteration: iteration}

	if call.FunctionCall == nil {
		step.Failed = true
		step.Output = "Malformed tool call: no function specified."
		step.DurationMS = elapsedMS(start)
		return step
	}

	step.Tool = call.FunctionCall.Name
	step.RawInput = call.FunctionCall.Arguments

	tool, ok := r.byName[step.Tool]
	if !ok {
		step.Failed = true
		step.Output = fmt.Sprintf("No tool named %q. Available tools: %s.", step.Tool, strings.Join(r.names(), ", "))
		step.DurationMS = elapsedMS(start)
		return step
	}

	args, err := mcp.ParseArgs(call.FunctionCall.Arguments, tool.Schema())
	if err != nil {
		step.Failed = true
		step.Output = err.Error()
		step.DurationMS = elapsedMS(start)
		return step
	}
	step.Arguments = args

	res, err := tool.InvokeResult(ctx, args)
	switch {
	case err != nil:
		step.Failed = true
		step.Output = "Tool call failed: " + err.Error()
	default:
		// A tool that ran and rejected the call is still a failed step. The
		// model gets the text either way, but a trace that calls this a
		// success hides exactly the pattern worth seeing — six calls against a
		// database that does not exist read as a clean run otherwise.
		step.Failed = res.IsError
		step.Output = clip(res.Text, maxToolOutputChars)
	}

	step.DurationMS = elapsedMS(start)
	return step
}

func (r *Runner) names() []string {
	names := make([]string, 0, len(r.Tools))
	for _, t := range r.Tools {
		names = append(names, t.Name())
	}
	return names
}

// summarise asks for a final answer with tools withheld, so a model stuck in a
// tool loop is forced to commit to a decision.
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

// toolCalls normalises the two shapes providers use: the modern tool_calls
// array and the legacy single function_call.
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

// buildPrompt frames the question with the recalled incidents.
func buildPrompt(question string, grounding []string) string {
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

// compile-time guard: Step must stay JSON-encodable, since the API returns the
// trace verbatim.
var _ = func() bool {
	if _, err := json.Marshal(Step{}); err != nil {
		panic(err)
	}
	return true
}()
