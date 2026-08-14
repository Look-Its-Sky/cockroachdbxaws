package remediation

import (
	"sort"
	"strings"
	"time"
)

// how strongly a candidate was verified.
//
// Ordered on purpose: this is what ranking sorts on, and it is the reason a
// service with no test suite can never outrank one whose tests actually ran.
type Strength int

const (
	// the patch did not even apply to the checkout
	StrengthUnapplied Strength = iota
	// it applied, and nothing further was run or could be
	StrengthApplied
	// it compiles, or parses, depending on what the service calls a build
	StrengthBuilds
	// the service's own tests ran against it and passed
	StrengthPasses
)

// Verification is what actually ran for one candidate, and what that proves.
//
// Every field is a fact about a stage that executed. "Builds" must never be
// presented to an engineer as "tests passed", so the two are recorded
// separately and Summary refuses to conflate them.
type Verification struct {
	Applied  bool `json:"applied"`
	BuildRan bool `json:"build_ran"`
	Built    bool `json:"built"`
	TestRan  bool `json:"test_ran"`
	Tested   bool `json:"tested"`
	// TimedOut means the container hit its ceiling; whatever it had reached by
	// then is still recorded, but nothing after it ran
	TimedOut   bool  `json:"timed_out"`
	DurationMS int64 `json:"duration_ms"`
	// the stage output an engineer reads when a candidate failed, capped
	Log string `json:"log,omitempty"`
}

// Strength collapses the stages into the one number ranking sorts on.
func (v Verification) Strength() Strength {
	switch {
	case !v.Applied:
		return StrengthUnapplied
	case v.TestRan && v.Tested:
		return StrengthPasses
	case v.BuildRan && v.Built:
		return StrengthBuilds
	default:
		return StrengthApplied
	}
}

// Satisfied reports whether there is nothing further worth proving: it built,
// and it passed tests if the service has any.
func (v Verification) Satisfied(repo Repository) bool {
	if !v.Applied {
		return false
	}
	if repo.HasTests() {
		return v.TestRan && v.Tested
	}
	if strings.TrimSpace(repo.BuildCommand) != "" {
		return v.BuildRan && v.Built
	}
	// nothing is declared, so nothing can be established either way
	return true
}

// NeedsRepair reports whether the failure is one the model could plausibly fix
// if it were shown the output.
//
// Only a compiler or a test suite saying no. A write that never landed is our
// bug rather than the model's, and a run that hit the ceiling produced no
// verdict to react to — sending either back would spend a round on nothing.
func (v Verification) NeedsRepair() bool {
	if !v.Applied || v.TimedOut {
		return false
	}
	return (v.BuildRan && !v.Built) || (v.TestRan && !v.Tested)
}

// Summary says exactly what was established, in the words an engineer can act
// on. A service with no tests says so rather than going quiet about it.
func (v Verification) Summary() string {
	switch {
	case !v.Applied:
		return "the change could not be applied to a clean checkout"
	case v.TestRan && v.Tested:
		return "builds, and the service's tests pass"
	case v.TestRan && !v.Tested:
		return "builds, but the service's tests fail"
	case v.BuildRan && v.Built && !v.TestRan:
		return "builds; this service declares no tests, so nothing was proven beyond that it compiles"
	case v.BuildRan && !v.Built:
		return "does not build"
	case v.TimedOut:
		return "applied, but the sandbox hit its time limit before it could be verified"
	default:
		return "applied, but this service declares neither a build nor a test command, so nothing was verified"
	}
}

// where a candidate has got to
type CandidateStatus string

const (
	CandidateProposed CandidateStatus = "proposed"
	// an engineer picked this one in the UI
	CandidateSelected CandidateStatus = "selected"
	CandidatePROpen   CandidateStatus = "pr_opened"
	// the model would not produce a usable change, or the sandbox refused it
	CandidateFailed CandidateStatus = "failed"
)

// Candidate is one proposed fix, and the evidence for it.
type Candidate struct {
	ID              string `json:"id"`
	InvestigationID string `json:"investigation_id"`
	IncidentID      string `json:"incident_id,omitempty"`
	ServiceID       string `json:"service_id,omitempty"`

	// Strategy names the prompt variation that produced this one, so two
	// candidates that look alike can be told apart in the UI.
	Strategy  string `json:"strategy"`
	Summary   string `json:"summary,omitempty"`
	Rationale string `json:"rationale,omitempty"`

	Files []string `json:"files,omitempty"`
	// Diff is `git diff` from inside the sandbox: what the change really was,
	// rather than what the model said it would be.
	Diff string `json:"diff,omitempty"`
	// Edits are the whole-file contents the pull request is built from. Kept
	// out of the API: a reviewer reads the diff, and shipping every changed
	// file in full to a list endpoint would dwarf everything else on it.
	Edits []FileEdit `json:"-"`

	Verification Verification `json:"verification"`
	// Repairs is how many times the candidate was shown its own build or test
	// failure and asked again. Surfaced because a fix that took three goes is
	// worth reading more carefully than one that landed first time.
	Repairs int             `json:"repairs,omitempty"`
	Status  CandidateStatus `json:"status"`
	// PRURL is set once an engineer picks this candidate and the draft opens.
	PRURL string `json:"pr_url,omitempty"`
	// Error explains a candidate that never got as far as a diff.
	Error string `json:"error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// whether this candidate is worth showing as a fix rather than as a failure
func (c Candidate) Usable() bool {
	return c.Error == "" && strings.TrimSpace(c.Diff) != "" && c.Verification.Applied
}

// Outcome is everything the remediation pipeline produced for one investigation.
type Outcome struct {
	InvestigationID string `json:"investigation_id"`
	IncidentID      string `json:"incident_id,omitempty"`
	ServiceID       string `json:"service_id,omitempty"`

	Status RemediationStatus `json:"status"`
	// Repository is the mapping this ran against, for the UI's header.
	Repository *Repository `json:"repository,omitempty"`
	Triage     *Triage     `json:"triage,omitempty"`
	// Candidates, best first. Empty when triage stopped the run.
	Candidates []Candidate `json:"candidates,omitempty"`

	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// where a whole remediation run has got to
type RemediationStatus string

const (
	RemediationRunning RemediationStatus = "running"
	// triage said the fault is not there, so no containers were spent
	RemediationStopped RemediationStatus = "stopped"
	RemediationDone    RemediationStatus = "done"
	RemediationFailed  RemediationStatus = "failed"
)

// Rank orders candidates best first, in place.
//
// Strength dominates everything: a candidate whose tests pass beats a prettier
// one that only compiles. Ties break towards the smaller diff, because the
// smallest change that satisfies the same evidence is the one an engineer can
// review in a hurry, which is the situation this exists for.
func Rank(candidates []Candidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]

		if a.Usable() != b.Usable() {
			return a.Usable()
		}
		if sa, sb := a.Verification.Strength(), b.Verification.Strength(); sa != sb {
			return sa > sb
		}
		return len(a.Diff) < len(b.Diff)
	})
}
