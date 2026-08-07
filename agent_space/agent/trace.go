package agent

import "time"

// Step records one tool invocation. The trace is what makes an agent run
// legible: it shows which CockroachDB tool was consulted, with what arguments,
// and what came back, so a decision can be audited rather than taken on faith.
type Step struct {
	Iteration  int            `json:"iteration"`
	Tool       string         `json:"tool"`
	Arguments  map[string]any `json:"arguments,omitempty"`
	RawInput   string         `json:"raw_input,omitempty"`
	Output     string         `json:"output"`
	DurationMS int64          `json:"duration_ms"`
	Failed     bool           `json:"failed"`
}

// Result is a completed agent run.
type Result struct {
	Answer     string   `json:"answer"`
	Sources    int      `json:"sources"`
	Iterations int      `json:"iterations"`
	Trace      []Step   `json:"trace"`
	Grounding  []string `json:"grounding,omitempty"`
	Truncated  bool     `json:"truncated"`
}

func elapsedMS(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}
