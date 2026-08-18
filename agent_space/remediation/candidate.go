package remediation

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// how strongly a candidate was verified; ordered on purpose, since ranking
// sorts on it and a service with no tests must not outrank one whose tests ran
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

// what actually ran for one candidate, a field per stage that executed. Builds
// and tests are separate because Summary must never present one as the other.
type Verification struct {
	Applied  bool `json:"applied"`
	BuildRan bool `json:"build_ran"`
	Built    bool `json:"built"`
	TestRan  bool `json:"test_ran"`
	Tested   bool `json:"tested"`
	// the container hit its ceiling; what it reached still stands, nothing after it ran
	TimedOut   bool  `json:"timed_out"`
	DurationMS int64 `json:"duration_ms"`
	// the stage output an engineer reads when a candidate failed, capped
	Log string `json:"log,omitempty"`
}

// collapses the stages into the one number ranking sorts on
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

// whether nothing further is worth proving: it built, and passed tests if there are any
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

// whether the model could plausibly fix this if shown the output. Only a
// compiler or test suite saying no: a failed write is our bug, and a timeout
// produced no verdict to react to.
func (v Verification) NeedsRepair() bool {
	if !v.Applied || v.TimedOut {
		return false
	}
	return (v.BuildRan && !v.Built) || (v.TestRan && !v.Tested)
}

// exactly what was established, in words an engineer can act on; a service with
// no tests says so rather than going quiet
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

// puts Summary() on the wire as "summary" alongside the fields it is derived
// from, so the frontend renders the one sentence this system's discipline
// depends on rather than re-deriving it from booleans in two places that can
// drift. Computed at marshal time, never stored: encode() in store.go uses
// this same method, so the persisted row grows a redundant but harmless
// "summary" key that decode() ignores on the way back in.
func (v Verification) MarshalJSON() ([]byte, error) {
	type alias Verification
	return json.Marshal(struct {
		alias
		Summary string `json:"summary"`
	}{alias: alias(v), Summary: v.Summary()})
}

// where a candidate has got to
type CandidateStatus string

const (
	CandidateProposed CandidateStatus = "proposed"
	// an engineer picked this one in the UI
	CandidateSelected CandidateStatus = "selected"
	CandidatePROpen   CandidateStatus = "pr_opened"
	// passed over by an engineer, not failed: it may have built and passed and
	// still been turned down, which is the judgement worth learning from
	CandidateRejected CandidateStatus = "rejected"
	// the model would not produce a usable change, or the sandbox refused it
	CandidateFailed CandidateStatus = "failed"
)

// one proposed fix, and the evidence for it
type Candidate struct {
	ID              string `json:"id"`
	InvestigationID string `json:"investigation_id"`
	IncidentID      string `json:"incident_id,omitempty"`
	ServiceID       string `json:"service_id,omitempty"`

	// the prompt variation that produced this one, so lookalikes can be told apart
	Strategy  string `json:"strategy"`
	Summary   string `json:"summary,omitempty"`
	Rationale string `json:"rationale,omitempty"`

	Files []string `json:"files,omitempty"`
	// `git diff` from inside the sandbox: the real change, not the claimed one
	Diff string `json:"diff,omitempty"`
	// whole-file contents the PR is built from, off the wire because a reviewer
	// reads the diff and full files would dwarf a list response
	Edits []FileEdit `json:"-"`

	Verification Verification `json:"verification"`
	// how many times this was shown its own failure and asked again; a fix that
	// took three goes is worth reading more carefully than one that landed
	Repairs int             `json:"repairs,omitempty"`
	Status  CandidateStatus `json:"status"`
	// set once an engineer picks this candidate and the draft opens
	PRURL string `json:"pr_url,omitempty"`
	// why an engineer passed over this one, separate from Error, which is the
	// sandbox refusing a candidate rather than a human declining it
	RejectionReason string `json:"rejection_reason,omitempty"`
	// why a candidate never got as far as a diff
	Error string `json:"error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// whether this candidate is worth showing as a fix rather than as a failure
func (c Candidate) Usable() bool {
	return c.Error == "" && strings.TrimSpace(c.Diff) != "" && c.Verification.Applied
}

// everything the remediation pipeline produced for one investigation
type Outcome struct {
	InvestigationID string `json:"investigation_id"`
	IncidentID      string `json:"incident_id,omitempty"`
	ServiceID       string `json:"service_id,omitempty"`

	Status RemediationStatus `json:"status"`
	// the mapping this ran against, for the UI's header
	Repository *Repository `json:"repository,omitempty"`
	Triage     *Triage     `json:"triage,omitempty"`
	// best first, empty when triage stopped the run
	Candidates []Candidate `json:"candidates,omitempty"`
	// how many fixes this run produced, set by Recent only so a list row can
	// say "3 fixes" without fetching a diff per candidate
	CandidateCount int `json:"candidate_count,omitempty"`
	// whether an engineer has decided, so a list row can tell "waiting" from
	// "dealt with". Counted against decisions, since a chosen candidate keeps
	// pr_opened rather than moving to selected.
	Decided bool `json:"decided"`
	// what an engineer concluded, so the picker can tell a decided run from an
	// undecided one in the call it already makes
	Decision *Decision `json:"decision,omitempty"`

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

// orders candidates best first, in place. Strength dominates, so passing tests
// beat a prettier candidate that only compiles; ties break to the smaller diff.
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
