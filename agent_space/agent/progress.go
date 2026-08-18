package agent

import (
	"context"
	"time"
)

// MaxProgressEvents bounds the durable in-flight timeline returned by the API.
// Events contain no model prose, tool arguments, or tool output.
const MaxProgressEvents = 50

type ProgressStage string

const (
	ProgressContext ProgressStage = "context"
	ProgressModel   ProgressStage = "model"
	ProgressTool    ProgressStage = "tool"
	ProgressVerdict ProgressStage = "verdict"
)

type ProgressStatus string

const (
	ProgressStarted   ProgressStatus = "started"
	ProgressCompleted ProgressStatus = "completed"
	ProgressFailed    ProgressStatus = "failed"
)

// Progress is deliberately categorical. It answers what stage is active
// without persisting chain-of-thought, prompts, arguments, results, or errors.
type Progress struct {
	Stage     ProgressStage  `json:"stage"`
	Status    ProgressStatus `json:"status"`
	Iteration int            `json:"iteration,omitempty"`
	Tool      string         `json:"tool,omitempty"`
	Cause     Cause          `json:"cause,omitempty"`
	At        time.Time      `json:"at"`
}

type progressReporter func(Progress)
type progressContextKey struct{}

// WithProgress attaches a synchronous observer to one run. The worker uses it
// to mirror safe progress into the same durable record as the final verdict.
func WithProgress(ctx context.Context, report func(Progress)) context.Context {
	if report == nil {
		return ctx
	}
	return context.WithValue(ctx, progressContextKey{}, progressReporter(report))
}

// ReportProgress emits a safe event when a reporter is attached.
func ReportProgress(ctx context.Context, event Progress) {
	report, _ := ctx.Value(progressContextKey{}).(progressReporter)
	if report == nil {
		return
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	report(event)
}
