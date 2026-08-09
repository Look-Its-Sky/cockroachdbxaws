package agent

import (
	"errors"
	"time"
)

// Cause classifies a failed step. Failed alone cannot distinguish a model that
// guessed at a table name from an MCP server that has fallen over, and those
// call for opposite responses: the first is the agent working as intended and
// correcting itself, the second is an outage that no amount of retrying fixes.
// A trace that shows six failures without saying which kind hides exactly the
// thing the trace exists to reveal.
type Cause string

const (
	// CauseMalformedCall is a tool call carrying no function at all.
	CauseMalformedCall Cause = "malformed_call"
	// CauseUnknownTool is a call naming a tool that was never offered.
	CauseUnknownTool Cause = "unknown_tool"
	// CauseInvalidArguments is arguments that could not be parsed against the
	// tool's schema, so nothing was sent to the server.
	CauseInvalidArguments Cause = "invalid_arguments"
	// CauseToolError is the server running the tool and rejecting the call —
	// a refused query, a missing table. Recoverable: the model reads why.
	CauseToolError Cause = "tool_error"
	// CauseRejected is the server refusing to run the tool at all: a blocked
	// schema, an argument it will not accept. It arrives as a protocol error
	// rather than a result, but it is a verdict on this one call and nothing
	// more, so it is recoverable in exactly the way CauseToolError is — the
	// model reads the reason and asks a different question.
	CauseRejected Cause = "rejected"
	// CauseTransport is the call failing before the tool ever ran: a dropped
	// session, a protocol-level rejection, a cancelled context. In practice
	// the most common one is configuration — an API key whose service account
	// has no access to the cluster is refused at the protocol layer, not by
	// the tool. None of these are recoverable by the model.
	CauseTransport Cause = "transport"
)

// ErrTransport reports that an MCP call failed before the tool ran, as opposed
// to a tool running and declining. Callers use it to answer "can we reach the
// cluster at all" rather than surfacing an outage as a generic run failure.
//
// The wording avoids "transport failed": the identical code path covers a
// dropped connection and a credential that is merely unauthorised, and calling
// the latter a transport failure sends the reader looking for a network
// problem that is not there.
var ErrTransport = errors.New("agent: MCP call failed before the tool ran")

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
	// Cause is set only on failed steps, and names which kind of failure.
	Cause Cause `json:"cause,omitempty"`
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
