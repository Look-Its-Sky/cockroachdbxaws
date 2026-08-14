package remediation

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/tmc/langchaingo/llms"
)

// a Sandbox that answers from canned output, keyed by the stage that identifies
// the script it was given. This is what makes the orchestration testable on a
// machine with no container runtime.
type fakeSandbox struct {
	mu     sync.Mutex
	specs  []Spec
	triage string
	// inspect and candidate are functions so a test can vary the answer per call
	inspect   func() string
	candidate func(spec Spec) Run
}

func (f *fakeSandbox) Run(_ context.Context, spec Spec) (Run, error) {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	f.mu.Unlock()

	switch {
	case strings.Contains(spec.Script, stageCommitLog):
		return Run{Output: f.triage}, nil
	case strings.Contains(spec.Script, sectionBegin+stageSources):
		return Run{Output: f.inspect()}, nil
	default:
		return f.candidate(spec), nil
	}
}

// answers each call from a list; the last answer repeats once the list runs
// out, since triage is one call and every candidate after it wants the same
// proposal back
type sequencedModel struct {
	mu        sync.Mutex
	answers   []string
	callCount int
}

func (m *sequencedModel) GenerateContent(context.Context, []llms.MessageContent, ...llms.CallOption) (*llms.ContentResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	answer := m.answers[min(m.callCount, len(m.answers)-1)]
	m.callCount++

	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: answer}}}, nil
}

func (m *sequencedModel) Call(context.Context, string, ...llms.CallOption) (string, error) {
	return "", nil
}

func testRepo() Repository {
	return Repository{
		ServiceID:     "checkout",
		Owner:         "Look-Its-Sky",
		Repo:          "opentelemetry-demo-auto-sre-test",
		DefaultBranch: "main",
		Subdirectory:  "src/checkout",
		RuntimeImage:  "golang:1.24",
		BuildCommand:  "go build ./...",
		TestCommand:   "go test ./...",
	}
}

// the shape a container prints: a set of delimited stages
func sections(stages ...[2]string) string {
	var b strings.Builder
	for _, s := range stages {
		b.WriteString(sectionBegin + s[0] + "\n")
		if s[1] != "" {
			b.WriteString(s[1] + "\n")
		}
		b.WriteString(sectionEnd + s[0] + " 0\n")
	}
	return b.String()
}

func triageOutput() string {
	return sections(
		[2]string{stageClone, "cloning"},
		[2]string{stageCommit, "0c6f0ae1234"},
		[2]string{stageCommitLog, "0c6f0ae1234\nsomeone\n2026-08-01\nchange the discount"},
		[2]string{stageFiles, "diff --git a/src/checkout/money.go"},
		[2]string{stageSource, "----- src/checkout/money.go\npackage money"},
	)
}

func inspectOutput() string {
	return sections(
		[2]string{stageClone, "cloning"},
		[2]string{stageTree, "src/checkout/money.go"},
		[2]string{stageSources, fileBegin + "src/checkout/money.go\npackage money\n" + fileEnd},
	)
}

// a candidate container that applied the change, built and passed
func passingCandidate(Spec) Run {
	return Run{Output: sections(
		[2]string{stageClone, "cloning"},
		[2]string{stageHarness, "wrote src/checkout/money.go"},
		[2]string{stageBuild, "ok"},
		[2]string{stageTest, "PASS"},
		[2]string{stageDiff, "diff --git a/src/checkout/money.go b/src/checkout/money.go"},
	)}
}

const goodProposal = "SUMMARY: clamp the discount\nRATIONALE: it was unbounded.\n" +
	fileBegin + "src/checkout/money.go\npackage money\n\nconst Fixed = true\n" + fileEnd

func TestRunProposesVerifiedCandidates(t *testing.T) {
	sandbox := &fakeSandbox{
		triage:    triageOutput(),
		inspect:   inspectOutput,
		candidate: passingCandidate,
	}

	runner := &Runner{
		Repos:   nil, // replaced below
		Sandbox: sandbox,
		Model: &sequencedModel{answers: []string{
			// the first call is triage's judgement, the rest are proposals
			"The fault is present in money.go.\n```json\n{\"status\":\"confirmed\",\"files\":[\"src/checkout/money.go\"],\"confidence\":0.8}\n```",
			goodProposal,
		}},
		Strategies: DefaultStrategies[:2],
	}

	outcome := runWithRepo(t, runner, testRepo())

	if outcome.Status != RemediationDone {
		t.Fatalf("status = %s, error = %q", outcome.Status, outcome.Error)
	}
	if outcome.Triage == nil || outcome.Triage.Status != TriageConfirmed {
		t.Fatalf("triage = %+v", outcome.Triage)
	}
	if len(outcome.Candidates) != 2 {
		t.Fatalf("got %d candidates, want 2", len(outcome.Candidates))
	}

	for _, c := range outcome.Candidates {
		if !c.Verification.Tested {
			t.Errorf("candidate %s (%s) is not marked as tested: %+v", c.ID, c.Strategy, c.Verification)
		}
		if c.Diff == "" {
			t.Errorf("candidate %s carries no diff", c.Strategy)
		}
		if len(c.Edits) == 0 {
			t.Errorf("candidate %s kept no file contents, so no PR could be built from it", c.Strategy)
		}
	}

	// one triage container, one inspect container, one per candidate
	if got := len(sandbox.specs); got != 4 {
		t.Errorf("ran %d containers, want 4", got)
	}
}

// the gate exists to stop the expensive path, so nothing beyond it may run
func TestRunStopsWhenTriageSaysTheFaultIsGone(t *testing.T) {
	sandbox := &fakeSandbox{
		triage:    triageOutput(),
		inspect:   func() string { t.Fatal("inspected the repository after triage said stop"); return "" },
		candidate: func(Spec) Run { t.Fatal("ran a candidate after triage said stop"); return Run{} },
	}

	runner := &Runner{
		Sandbox: sandbox,
		Model: &sequencedModel{answers: []string{
			"That commit is itself the fix; the fault is not present.\n```json\n{\"status\":\"not_present\"}\n```",
		}},
	}

	outcome := runWithRepo(t, runner, testRepo())

	if outcome.Status != RemediationStopped {
		t.Fatalf("status = %s, want stopped", outcome.Status)
	}
	if len(outcome.Candidates) != 0 {
		t.Fatalf("got %d candidates, want none", len(outcome.Candidates))
	}
	if len(sandbox.specs) != 1 {
		t.Errorf("ran %d containers, want 1 (triage only)", len(sandbox.specs))
	}
}

// a change that applies but alters nothing is not a fix, and showing an empty
// diff wastes the reviewer's scarcest resource
func TestCandidateWithAnEmptyDiffIsRecordedAsAFailure(t *testing.T) {
	sandbox := &fakeSandbox{
		triage:  triageOutput(),
		inspect: inspectOutput,
		candidate: func(Spec) Run {
			return Run{Output: sections(
				[2]string{stageClone, "cloning"},
				[2]string{stageHarness, "wrote src/checkout/money.go"},
				[2]string{stageBuild, "ok"},
				[2]string{stageTest, "PASS"},
				[2]string{stageDiff, ""},
			)}
		},
	}

	runner := &Runner{
		Sandbox: sandbox,
		Model: &sequencedModel{answers: []string{
			"Present.\n```json\n{\"status\":\"confirmed\",\"files\":[\"src/checkout/money.go\"]}\n```",
			goodProposal,
		}},
		Strategies: DefaultStrategies[:1],
	}

	outcome := runWithRepo(t, runner, testRepo())

	if len(outcome.Candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(outcome.Candidates))
	}
	c := outcome.Candidates[0]
	if c.Status != CandidateFailed || !strings.Contains(c.Error, "identical") {
		t.Errorf("an empty diff was not recorded as a failure: status=%s error=%q", c.Status, c.Error)
	}
}

// no commit means triage has nothing to read, and fanning out blind is exactly
// what the gate exists to prevent
func TestRunStopsWhenNoCommitWasImplicated(t *testing.T) {
	sandbox := &fakeSandbox{
		candidate: func(Spec) Run { t.Fatal("ran a container with no commit to triage"); return Run{} },
	}

	runner := &Runner{Sandbox: sandbox, Model: &sequencedModel{answers: []string{""}}}

	outcome := runWithRepoAndSHA(t, runner, testRepo(), "")

	if outcome.Status != RemediationStopped {
		t.Fatalf("status = %s, want stopped", outcome.Status)
	}
	if outcome.Triage.Status != TriageCommitMissing {
		t.Errorf("triage = %s, want commit_missing", outcome.Triage.Status)
	}
	if len(sandbox.specs) != 0 {
		t.Errorf("ran %d containers, want none", len(sandbox.specs))
	}
}

// a candidate container that applied and compiled but failed the build stage
func failingBuild(Spec) Run {
	return Run{Output: sections(
		[2]string{stageClone, "cloning"},
		[2]string{stageHarness, "wrote src/checkout/money.go"},
		[2]string{stageDiff, "diff --git a/src/checkout/money.go b/src/checkout/money.go"},
	) + sectionBegin + stageBuild + "\nmoney.go:92:3: invalid operation: mismatched types int64 and int32\n" +
		sectionEnd + stageBuild + " 1\n"}
}

const repairedProposal = "SUMMARY: clamp the discount, with the conversion\nRATIONALE: int32 to int64.\n" +
	fileBegin + "src/checkout/money.go\npackage money\n\nconst Fixed = 2\n" + fileEnd

// The failures that come back are mechanical — a missing type conversion —
// while the reasoning about what to change was right. Showing the model its own
// compiler output is the cheapest way to recover that.
func TestCandidateIsRepairedFromItsOwnBuildFailure(t *testing.T) {
	var runs int
	sandbox := &fakeSandbox{
		triage:  triageOutput(),
		inspect: inspectOutput,
		candidate: func(Spec) Run {
			runs++
			if runs == 1 {
				return failingBuild(Spec{})
			}
			return passingCandidate(Spec{})
		},
	}

	model := &sequencedModel{answers: []string{
		"Present.\n```json\n{\"status\":\"confirmed\",\"files\":[\"src/checkout/money.go\"]}\n```",
		goodProposal,
		repairedProposal,
	}}

	runner := &Runner{Sandbox: sandbox, Model: model, Strategies: DefaultStrategies[:1]}
	outcome := runWithRepo(t, runner, testRepo())

	if len(outcome.Candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(outcome.Candidates))
	}

	c := outcome.Candidates[0]
	if c.Repairs != 1 {
		t.Errorf("repairs = %d, want 1", c.Repairs)
	}
	if !c.Verification.Tested {
		t.Errorf("the repaired candidate is not verified: %+v", c.Verification)
	}
	// the repaired summary replaces the one that did not compile
	if !strings.Contains(c.Summary, "with the conversion") {
		t.Errorf("summary is from the failed attempt: %q", c.Summary)
	}
	if runs != 2 {
		t.Errorf("ran %d candidate containers, want 2 (one attempt, one repair)", runs)
	}
}

// a model that cannot fix its own compiler error when shown it once will not
// fix it on the fifth go, and every round is a container
func TestRepairIsBounded(t *testing.T) {
	var runs int
	sandbox := &fakeSandbox{
		triage:  triageOutput(),
		inspect: inspectOutput,
		candidate: func(Spec) Run {
			runs++
			return failingBuild(Spec{})
		},
	}

	model := &sequencedModel{answers: []string{
		"Present.\n```json\n{\"status\":\"confirmed\",\"files\":[\"src/checkout/money.go\"]}\n```",
		goodProposal,
	}}

	runner := &Runner{Sandbox: sandbox, Model: model, Strategies: DefaultStrategies[:1]}
	outcome := runWithRepo(t, runner, testRepo())

	if runs != 2 {
		t.Errorf("ran %d containers, want 2: one attempt plus one repair", runs)
	}
	// and the unrepairable candidate is still reported honestly rather than lost
	c := outcome.Candidates[0]
	if c.Verification.Built {
		t.Error("a candidate that never compiled is marked as building")
	}
	if !strings.Contains(c.Verification.Summary(), "does not build") {
		t.Errorf("summary = %q", c.Verification.Summary())
	}
}

func TestNoRepairWhenTheCandidateAlreadyPasses(t *testing.T) {
	var runs int
	sandbox := &fakeSandbox{
		triage:  triageOutput(),
		inspect: inspectOutput,
		candidate: func(Spec) Run {
			runs++
			return passingCandidate(Spec{})
		},
	}

	runner := &Runner{
		Sandbox: sandbox,
		Model: &sequencedModel{answers: []string{
			"Present.\n```json\n{\"status\":\"confirmed\",\"files\":[\"src/checkout/money.go\"]}\n```",
			goodProposal,
		}},
		Strategies: DefaultStrategies[:1],
	}
	runWithRepo(t, runner, testRepo())

	if runs != 1 {
		t.Errorf("ran %d containers, want 1: nothing needed repairing", runs)
	}
}

func TestMaxRepairsNegativeTurnsRepairOff(t *testing.T) {
	var runs int
	sandbox := &fakeSandbox{
		triage:    triageOutput(),
		inspect:   inspectOutput,
		candidate: func(Spec) Run { runs++; return failingBuild(Spec{}) },
	}

	runner := &Runner{
		Sandbox:    sandbox,
		MaxRepairs: -1,
		Model: &sequencedModel{answers: []string{
			"Present.\n```json\n{\"status\":\"confirmed\",\"files\":[\"src/checkout/money.go\"]}\n```",
			goodProposal,
		}},
		Strategies: DefaultStrategies[:1],
	}
	runWithRepo(t, runner, testRepo())

	if runs != 1 {
		t.Errorf("ran %d containers, want 1: repair is off", runs)
	}
}

func TestNeedsRepairOnlyForFailuresAModelCanAct(t *testing.T) {
	cases := []struct {
		name string
		v    Verification
		want bool
	}{
		{"build failed", Verification{Applied: true, BuildRan: true}, true},
		{"tests failed", Verification{Applied: true, BuildRan: true, Built: true, TestRan: true}, true},
		{"never applied", Verification{}, false},
		{"timed out", Verification{Applied: true, TimedOut: true}, false},
		{"all good", Verification{Applied: true, BuildRan: true, Built: true, TestRan: true, Tested: true}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.NeedsRepair(); got != tc.want {
				t.Errorf("NeedsRepair() = %v, want %v", got, tc.want)
			}
		})
	}
}

// a service with no tests is satisfied by a build; one with tests is not
func TestSatisfiedFollowsWhatTheServiceDeclares(t *testing.T) {
	builds := Verification{Applied: true, BuildRan: true, Built: true}

	if builds.Satisfied(testRepo()) {
		t.Error("checkout has tests, so a build alone must not satisfy it")
	}

	noTests := testRepo()
	noTests.TestCommand = ""
	if !builds.Satisfied(noTests) {
		t.Error("a service with no tests is not satisfied by a passing build")
	}
}

func TestFilesToReadFallsBackToWhatTheCommitTouched(t *testing.T) {
	parsed := ParseSections(triageOutput())

	got := filesToRead(Triage{}, parsed)
	if len(got) != 1 || got[0] != "src/checkout/money.go" {
		t.Errorf("files = %v, want the path the triage script printed", got)
	}

	// what triage named wins when it named anything
	got = filesToRead(Triage{Files: []string{"src/checkout/other.go"}}, parsed)
	if len(got) != 1 || got[0] != "src/checkout/other.go" {
		t.Errorf("files = %v, want triage's own list", got)
	}
}

// Repositories reads from a pool, so a test substitutes the lookup by running
// the pipeline against a runner whose repository is already resolved.
func runWithRepo(t *testing.T, r *Runner, repo Repository) Outcome {
	t.Helper()
	return runWithRepoAndSHA(t, r, repo, "0c6f0ae")
}

func runWithRepoAndSHA(t *testing.T, r *Runner, repo Repository, sha string) Outcome {
	t.Helper()

	r.Repos = &Repositories{}
	r.lookup = func(context.Context, string) (Repository, error) { return repo, nil }

	return r.Run(context.Background(), Request{
		InvestigationID: "inv-1",
		IncidentID:      "inc-1",
		ServiceID:       repo.ServiceID,
		IncidentSummary: "checkout is charging negative amounts",
		VerdictAnswer:   "HOTFIX the discount clamp in " + sha,
		CommitSHA:       sha,
	})
}
