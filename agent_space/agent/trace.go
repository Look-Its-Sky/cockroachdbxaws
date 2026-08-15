package agent

import (
	"errors"
	"time"
)

// why a step failed; failed alone cannot tell a model's bad guess from an outage
type Cause string

const (
	// a tool call carrying no function at all
	CauseMalformedCall Cause = "malformed_call"
	// a call naming a tool that was never offered
	CauseUnknownTool Cause = "unknown_tool"
	// arguments that did not parse against the schema, so nothing was sent
	CauseInvalidArguments Cause = "invalid_arguments"
	// the server ran the tool and rejected the call, recoverable
	CauseToolError Cause = "tool_error"
	// the server refused to run the tool at all, a verdict on this call only, recoverable
	CauseRejected Cause = "rejected"
	// the call never reached the tool: dropped session, protocol rejection, cancelled context
	CauseTransport Cause = "transport"
)

// an MCP call failed before the tool ran, as opposed to a tool running and declining
var ErrTransport = errors.New("agent: MCP call failed before the tool ran")

// one tool invocation, so a decision can be audited rather than taken on faith
type Step struct {
	Iteration  int            `json:"iteration"`
	Tool       string         `json:"tool"`
	Arguments  map[string]any `json:"arguments,omitempty"`
	RawInput   string         `json:"raw_input,omitempty"`
	Output     string         `json:"output"`
	DurationMS int64          `json:"duration_ms"`
	Failed     bool           `json:"failed"`
	// set only on failed steps, naming which kind of failure
	Cause Cause `json:"cause,omitempty"`
}

// a completed agent run
type Result struct {
	Answer string `json:"answer"`
	// the same decision as Answer, in a form the remediation pipeline can
	// branch on without reading English
	Verdict    Verdict `json:"verdict"`
	Sources    int     `json:"sources"`
	Iterations int     `json:"iterations"`
	Trace      []Step  `json:"trace"`
	// past incidents recalled from the index
	Grounding []string `json:"grounding,omitempty"`
	// past decisions this team recorded on similar incidents, kept separate
	// from Grounding because they are opinion rather than history
	Precedents []string `json:"precedents,omitempty"`
	Truncated  bool     `json:"truncated"`
}

// record the model's answer, splitting the machine-readable verdict off the
// prose so a human never reads a JSON block and automation never parses English
func (r *Result) setAnswer(raw string) {
	r.Verdict, r.Answer = parseVerdict(raw)
}

func elapsedMS(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}
