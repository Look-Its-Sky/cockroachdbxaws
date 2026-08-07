// Command toolcheck measures whether the configured OpenRouter chat model emits
// well-formed tool calls.
//
// The whole agent rests on this: if the model cannot reliably request a tool
// with valid arguments, the MCP integration produces nothing no matter how
// correct the client is. OpenRouter advertising `tools` in supported_parameters
// is a claim about the API surface, not about the model's behaviour, so this
// measures the behaviour directly before anything is built on top of it.
//
// Usage:
//
//	go run ./cmd/toolcheck              # 20 iterations against OPENROUTER_MODEL
//	go run ./cmd/toolcheck -n 40        # more samples
//	go run ./cmd/toolcheck -model qwen/qwen3-coder-next
//
// Exits non-zero if the pass rate falls below -threshold, so it can gate CI.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tmc/langchaingo/llms"

	"agent_space/utils"
	"agent_space/utils/mcp"
)

// The fixtures mirror two real CockroachDB Cloud MCP tools, schemas included,
// so the measurement reflects the shapes the agent actually sends.
var fixtureTools = []llms.Tool{
	{
		Type: "function",
		Function: &llms.FunctionDefinition{
			Name:        "select_query",
			Description: "Execute a read-only SELECT statement against the CockroachDB cluster.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"statement": map[string]any{
						"type":        "string",
						"description": "A single SELECT statement.",
					},
				},
				"required": []any{"statement"},
			},
		},
	},
	{
		Type: "function",
		Function: &llms.FunctionDefinition{
			Name:        "list_tables",
			Description: "List the tables in a database schema.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"database": map[string]any{"type": "string"},
					"schema":   map[string]any{"type": "string"},
				},
				"required": []any{"database", "schema"},
			},
		},
	},
}

// requiredArgs mirrors the fixture schemas, for checking what came back.
var requiredArgs = map[string][]string{
	"select_query": {"statement"},
	"list_tables":  {"database", "schema"},
}

// prompts alternate so a single lucky phrasing cannot carry the score. Each one
// has an unambiguous correct tool.
var prompts = []struct {
	text     string
	wantTool string
}{
	{"How many rows are in the incidents table? Use the tools available to find out.", "select_query"},
	{"What tables exist in the public schema of the defaultdb database?", "list_tables"},
}

// result is one iteration's outcome.
type result struct {
	index      int
	prompt     string
	wantTool   string
	gotTool    string
	rawArgs    string
	stopReason string
	content    string
	err        error

	emitted  bool // a tool call came back at all
	named    bool // the tool name is one we published
	parsed   bool // arguments are a JSON object
	complete bool // every required field is present and non-empty
	correct  bool // it picked the tool the prompt called for
}

// ok reports whether this iteration produced a usable tool call. Picking a
// different valid tool is a judgement call, not a malformation, so `correct` is
// reported separately and does not gate the pass rate.
func (r result) ok() bool { return r.emitted && r.named && r.parsed && r.complete }

func main() {
	var (
		iterations  = flag.Int("n", 20, "number of iterations")
		modelName   = flag.String("model", "", "override OPENROUTER_MODEL")
		threshold   = flag.Float64("threshold", 0.8, "minimum pass rate before exiting non-zero")
		concurrency = flag.Int("c", 4, "concurrent requests")
		maxTokens   = flag.Int("max-tokens", 2000, "completion budget; reasoning models need headroom")
		verbose     = flag.Bool("v", false, "print every iteration, not just failures")
	)
	flag.Parse()

	utils.LoadConfig()
	if *modelName != "" {
		// Set both names so the override lands whichever provider is active.
		os.Setenv("OPENROUTER_MODEL", *modelName)
		os.Setenv("LLM_MODEL", *modelName)
	}

	active := utils.ChatModel()
	model, err := utils.GetLLM()
	if err != nil {
		fmt.Fprintf(os.Stderr, "toolcheck: %v\n", err)
		os.Exit(2)
	}

	fmt.Printf("%s\niterations: %d\nbudget:     %d completion tokens\n\n", utils.DescribeLLM(), *iterations, *maxTokens)

	results := run(model, *iterations, *concurrency, *maxTokens)
	report(results, *verbose)

	rate := passRate(results)
	fmt.Printf("\npass rate: %.0f%% (threshold %.0f%%)\n", rate*100, *threshold*100)
	if rate < *threshold {
		fmt.Printf("\n%s is not reliable enough for the agent loop.\n", active)
		fmt.Println("Try: go run ./cmd/toolcheck -model qwen/qwen3-coder-next")
		os.Exit(1)
	}
}

// run executes the iterations with bounded concurrency.
func run(model llms.Model, iterations, concurrency, maxTokens int) []result {
	results := make([]result, iterations)
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for i := range iterations {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			results[i] = probe(model, i, maxTokens)
			fmt.Fprint(os.Stderr, ".")
		}(i)
	}

	wg.Wait()
	fmt.Fprintln(os.Stderr)
	return results
}

// probe runs a single tool-calling request and grades the response.
func probe(model llms.Model, i, maxTokens int) result {
	p := prompts[i%len(prompts)]
	r := result{index: i, prompt: p.text, wantTool: p.wantTool}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	resp, err := model.GenerateContent(ctx,
		[]llms.MessageContent{
			llms.TextParts(llms.ChatMessageTypeSystem,
				"You are a database operations assistant. Use the provided tools to answer questions about the cluster. Call a tool rather than guessing."),
			llms.TextParts(llms.ChatMessageTypeHuman, p.text),
		},
		llms.WithTools(fixtureTools),
		llms.WithMaxTokens(maxTokens),
	)
	if err != nil {
		r.err = err
		return r
	}
	if len(resp.Choices) == 0 {
		r.err = fmt.Errorf("no choices returned")
		return r
	}

	choice := resp.Choices[0]
	r.stopReason = choice.StopReason
	r.content = choice.Content

	// Accept either shape: the modern tool_calls array, or the legacy single
	// function_call some providers still emit.
	var name, args string
	switch {
	case len(choice.ToolCalls) > 0 && choice.ToolCalls[0].FunctionCall != nil:
		name = choice.ToolCalls[0].FunctionCall.Name
		args = choice.ToolCalls[0].FunctionCall.Arguments
	case choice.FuncCall != nil:
		name = choice.FuncCall.Name
		args = choice.FuncCall.Arguments
	default:
		return r
	}

	r.emitted = true
	r.gotTool = name
	r.rawArgs = args

	required, known := requiredArgs[name]
	r.named = known
	r.correct = name == p.wantTool
	if !known {
		return r
	}

	// Grade against ParseArgs, the same normaliser the agent uses, so the score
	// reflects what the agent will actually tolerate rather than raw JSON purity.
	schema := schemaFor(name)
	parsed, err := mcp.ParseArgs(args, schema)
	if err != nil {
		return r
	}
	r.parsed = true

	r.complete = true
	for _, field := range required {
		v, ok := parsed[field]
		if !ok || strings.TrimSpace(fmt.Sprint(v)) == "" {
			r.complete = false
			break
		}
	}

	return r
}

func schemaFor(name string) any {
	for _, t := range fixtureTools {
		if t.Function.Name == name {
			return t.Function.Parameters
		}
	}
	return nil
}

func passRate(results []result) float64 {
	if len(results) == 0 {
		return 0
	}
	var passed int
	for _, r := range results {
		if r.ok() {
			passed++
		}
	}
	return float64(passed) / float64(len(results))
}

// report prints a per-stage breakdown, so a failure says which stage broke
// rather than just "it did not work".
func report(results []result, verbose bool) {
	stages := []struct {
		label string
		count func(result) bool
	}{
		{"emitted a tool call", func(r result) bool { return r.emitted }},
		{"used a published tool name", func(r result) bool { return r.named }},
		{"arguments parsed", func(r result) bool { return r.parsed }},
		{"required fields present", func(r result) bool { return r.complete }},
		{"picked the expected tool", func(r result) bool { return r.correct }},
	}

	total := len(results)
	fmt.Println("stage                          passed")
	fmt.Println("------------------------------------")
	for _, s := range stages {
		var n int
		for _, r := range results {
			if s.count(r) {
				n++
			}
		}
		fmt.Printf("%-28s %3d/%d\n", s.label, n, total)
	}

	stops := map[string]int{}
	for _, r := range results {
		if r.stopReason != "" {
			stops[r.stopReason]++
		}
	}
	if len(stops) > 0 {
		fmt.Print("\nstop reasons: ")
		var parts []string
		for reason, n := range stops {
			parts = append(parts, fmt.Sprintf("%s=%d", reason, n))
		}
		fmt.Println(strings.Join(parts, " "))
	}

	fmt.Println()
	for _, r := range results {
		if r.ok() && !verbose {
			continue
		}
		describe(r)
	}
}

func describe(r result) {
	status := "FAIL"
	if r.ok() {
		status = "ok  "
	}

	fmt.Printf("%s #%-2d want=%s got=%q stop=%q\n", status, r.index, r.wantTool, r.gotTool, r.stopReason)
	if r.err != nil {
		fmt.Printf("        error: %v\n", r.err)
	}
	if r.rawArgs != "" {
		fmt.Printf("        args: %s\n", clip(r.rawArgs, 160))
	}
	if !r.emitted && r.content != "" {
		fmt.Printf("        content instead of tool call: %s\n", clip(r.content, 160))
	}
}

func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// compile-time guard that the fixture schemas stay JSON-encodable, since they
// travel to the provider verbatim.
var _ = func() bool {
	for _, t := range fixtureTools {
		if _, err := json.Marshal(t.Function.Parameters); err != nil {
			panic(err)
		}
	}
	return true
}()
