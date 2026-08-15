package remediation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tmc/langchaingo/llms"
)

// a model that returns what it is told to, so triage can be driven without tokens
type scriptedModel struct {
	reply  string
	err    error
	calls  int
	prompt string
}

func (m *scriptedModel) GenerateContent(_ context.Context, messages []llms.MessageContent, _ ...llms.CallOption) (*llms.ContentResponse, error) {
	m.calls++
	for _, msg := range messages {
		for _, part := range msg.Parts {
			if text, ok := part.(llms.TextContent); ok {
				m.prompt += text.Text + "\n"
			}
		}
	}
	if m.err != nil {
		return nil, m.err
	}
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: m.reply}}}, nil
}

func (m *scriptedModel) Call(context.Context, string, ...llms.CallOption) (string, error) {
	return "", errors.New("not used")
}

func gatheredSections() map[string]Section {
	return map[string]Section{
		stageCommit: {Stage: stageCommit, Ran: true, ExitCode: 0,
			Output: "0c6f0ae1f2c3d4e5a6b7c8d9e0f1a2b3c4d5e6f7"},
		stageCommitLog: {Stage: stageCommitLog, Ran: true, ExitCode: 0,
			Output: "0c6f0ae1f2c3d4e5a6b7c8d9e0f1a2b3c4d5e6f7\nRyan Faircloth\n2026-07-20T07:55:57Z\nAdd KAFKA_TOPIC environment variable (#3665)"},
		stageFiles: {Stage: stageFiles, Ran: true, ExitCode: 0,
			Output: "+KAFKA_TOPIC=orders"},
		stageSource: {Stage: stageSource, Ran: true, ExitCode: 0,
			Output: "----- src/checkout/kafka/producer.go\npackage kafka"},
	}
}

func TestJudgeConfirmed(t *testing.T) {
	model := &scriptedModel{reply: "The topic is read from an unset variable at producer.go:41.\n\n" +
		"```json\n{\"status\": \"confirmed\", \"files\": [\"src/checkout/kafka/producer.go\"], \"confidence\": 0.8}\n```"}

	got, err := Judge(context.Background(), model, "orders stopped publishing", "ROLLBACK 0c6f0ae", gatheredSections())
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}

	if got.Status != TriageConfirmed {
		t.Errorf("status = %q, want confirmed", got.Status)
	}
	if !got.ShouldRemediate() {
		t.Error("a confirmed fault should proceed to remediation")
	}
	if len(got.Files) != 1 || got.Files[0] != "src/checkout/kafka/producer.go" {
		t.Errorf("files = %v", got.Files)
	}
	if got.Commit != "0c6f0ae1f2c3d4e5a6b7c8d9e0f1a2b3c4d5e6f7" {
		t.Errorf("commit = %q, want the resolved full SHA", got.Commit)
	}
	if got.CommitSubject != "Add KAFKA_TOPIC environment variable (#3665)" {
		t.Errorf("subject = %q", got.CommitSubject)
	}
	// the JSON must not be shown to a human as evidence
	if strings.Contains(got.Evidence, "```") {
		t.Errorf("evidence carries the raw block: %q", got.Evidence)
	}
}

func TestJudgeNotPresentStopsRemediation(t *testing.T) {
	// the case that prompted this gate: the commit is a fix, not a cause
	model := &scriptedModel{reply: "The divisor is already 10000000; this commit is a fix.\n\n" +
		"```json\n{\"status\": \"not_present\", \"confidence\": 0.9}\n```"}

	got, err := Judge(context.Background(), model, "amounts wrong", "ROLLBACK 92df8a2", gatheredSections())
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}

	if got.Status != TriageNotPresent {
		t.Errorf("status = %q, want not_present", got.Status)
	}
	if got.ShouldRemediate() {
		t.Error("a fault that is not present must not spend containers on fixes")
	}
}

func TestJudgeMissingCommitNeedsNoModelCall(t *testing.T) {
	sections := gatheredSections()
	sections[stageCommit] = Section{Stage: stageCommit, Ran: true, ExitCode: 128,
		Output: "fatal: Not a valid object name"}

	model := &scriptedModel{reply: "should not be called"}
	got, err := Judge(context.Background(), model, "anything", "ROLLBACK deadbee", sections)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}

	if got.Status != TriageCommitMissing {
		t.Errorf("status = %q, want commit_missing", got.Status)
	}
	if got.CommitExists {
		t.Error("CommitExists should be false")
	}
	if got.ShouldRemediate() {
		t.Error("a commit that does not exist must not proceed")
	}
	// a fact the sandbox already settled must not cost a model call
	if model.calls != 0 {
		t.Errorf("model was called %d times for a missing commit", model.calls)
	}
}

func TestJudgeInconclusiveStillProceeds(t *testing.T) {
	// the gate exists to stop obvious waste, not to overrule an incident
	// because a small model could not decide
	model := &scriptedModel{reply: "```json\n{\"status\": \"inconclusive\"}\n```"}

	got, err := Judge(context.Background(), model, "x", "y", gatheredSections())
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if !got.ShouldRemediate() {
		t.Error("inconclusive should still proceed")
	}
}

func TestJudgePromptCarriesTheEvidence(t *testing.T) {
	model := &scriptedModel{reply: "```json\n{\"status\": \"confirmed\"}\n```"}

	if _, err := Judge(context.Background(), model, "orders stopped publishing", "ROLLBACK 0c6f0ae", gatheredSections()); err != nil {
		t.Fatalf("Judge: %v", err)
	}

	for _, want := range []string{
		"orders stopped publishing",        // the incident
		"ROLLBACK 0c6f0ae",                 // the verdict
		"KAFKA_TOPIC environment variable", // the commit
		"+KAFKA_TOPIC=orders",              // its diff
		"src/checkout/kafka/producer.go",   // current source
	} {
		if !strings.Contains(model.prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
}

func TestParseTriageFallsBackToProse(t *testing.T) {
	tests := map[string]TriageStatus{
		"The fault is present in the current code.":    TriageConfirmed,
		"This is confirmed by the source.":             TriageConfirmed,
		"The code does not contain it; already fixed.": TriageNotPresent,
		"That commit is a fix rather than a cause.":    TriageNotPresent,
		"The divisor is no longer present in main.":    TriageNotPresent,
		"I cannot tell from the evidence provided.":    TriageInconclusive,
	}

	for answer, want := range tests {
		if got := parseTriage(answer); got.Status != want {
			t.Errorf("parseTriage(%q).Status = %q, want %q", answer, got.Status, want)
		}
	}
}

func TestParseTriageNegativeBeatsPositiveSubstring(t *testing.T) {
	// "not present" contains "present"; the negative has to be checked first
	got := parseTriage("The described fault is not present in the current code.")
	if got.Status != TriageNotPresent {
		t.Errorf("status = %q, want not_present", got.Status)
	}
}

func TestBuildTriageScriptOnlyReads(t *testing.T) {
	script := BuildTriageScript(checkoutRepo(), "https://github.com/o/r.git", "0c6f0ae")

	// triage runs before anything else; it must not be able to modify the tree
	for _, forbidden := range []string{"git commit", "git checkout -b", "git apply", "git reset", "rm -rf"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("triage script mutates the checkout: %q", forbidden)
		}
	}
	for _, want := range []string{stageClone, stageCommit, stageCommitLog, stageFiles, stageSource} {
		if !strings.Contains(script, sectionBegin+want) {
			t.Errorf("stage %q is missing", want)
		}
	}
}

func TestBuildTriageScriptBoundsHistoryButFetchesTheCommit(t *testing.T) {
	script := BuildTriageScript(checkoutRepo(), "https://github.com/o/r.git", "0c6f0ae")

	// history is a context budget, not the whole repository
	if !strings.Contains(script, fmt.Sprintf("--depth %d", CloneDepth)) {
		t.Errorf("expected a bounded clone at depth %d", CloneDepth)
	}

	// the implicated commit is often outside the clone budget, and an
	// abbreviated SHA cannot be fetched by name, so deepening is the only
	// route; conditional, so the usual case pays nothing
	if !strings.Contains(script, "--deepen") {
		t.Errorf("nothing reaches a commit past the clone depth:\n%s", script)
	}
	if strings.Contains(script, "fetch --depth 1 origin '0c6f0ae'") {
		t.Errorf("an abbreviated SHA is fetched by name, which always fails:\n%s", script)
	}

	// a full object name, by contrast, is one extra object rather than 400
	full := BuildTriageScript(checkoutRepo(), "https://github.com/o/r.git",
		"0c6f0ae70920e87405ab44d1e3a160bce1d4e82c")
	if !strings.Contains(full, "fetch --depth 1 origin '0c6f0ae70920e87405ab44d1e3a160bce1d4e82c'") {
		t.Errorf("a full SHA is not fetched explicitly:\n%s", full)
	}
}

func TestFetchCommitToleratesFailure(t *testing.T) {
	// a commit that cannot be fetched is a finding for triage to report, not a
	// reason to abandon the run before it has looked at anything
	if !strings.Contains(fetchCommitCommand("deadbee"), "||") {
		t.Error("a failed fetch aborts the clone stage")
	}
}

func TestCloneIsSelfContained(t *testing.T) {
	// a blobless clone resolves contents lazily, so the container would need the
	// network for its whole life
	script := BuildTriageScript(checkoutRepo(), "https://github.com/o/r.git", "0c6f0ae")
	if strings.Contains(script, "--filter=blob:none") {
		t.Error("a blobless clone needs the network throughout the run")
	}
}

func TestBuildTriageScriptQuotesTheSHA(t *testing.T) {
	// the SHA reaches here from a model's answer, so it is untrusted input
	script := BuildTriageScript(checkoutRepo(), "https://github.com/o/r.git", "abc'; rm -rf /; echo '")

	if strings.Contains(script, "; rm -rf /;") && !strings.Contains(script, `'\''`) {
		t.Error("the SHA was interpolated without escaping")
	}
}
