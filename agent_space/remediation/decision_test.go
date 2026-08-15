package remediation

import (
	"errors"
	"strings"
	"testing"
)

func decidableOutcome() Outcome {
	return Outcome{
		InvestigationID: "inv-1",
		IncidentID:      "INC-412",
		ServiceID:       "checkout",
		Triage: &Triage{
			Status:        TriageConfirmed,
			Commit:        "51e85e6",
			CommitSubject: "simplify money.Sum carry handling",
			Evidence:      "Sum adds nanos as int32, so any total past one unit overflows.",
		},
		Candidates: []Candidate{
			{
				ID: "cand-defensive", Strategy: "defensive",
				Summary: "accumulate in int64 and derive the carry",
				Files:   []string{"src/checkout/money/money.go"},
				Diff:    "--- a\n+++ b\n",
				Verification: Verification{
					Applied: true, BuildRan: true, Built: true, TestRan: true, Tested: true,
				},
			},
			{
				ID: "cand-minimal", Strategy: "minimal",
				Summary: "widen the addition",
				Diff:    "--- a\n+++ b\n",
				Verification: Verification{
					Applied: true, BuildRan: true, Built: false,
				},
			},
			{
				ID: "cand-rootcause", Strategy: "root-cause",
				Summary: "rework the carry entirely",
				Diff:    "--- a\n+++ b\n",
				Verification: Verification{
					Applied: true, BuildRan: true, Built: true, TestRan: true, Tested: true,
				},
			},
		},
	}
}

func TestNewDecisionRecordsTheChoiceAndFillsInTheRest(t *testing.T) {
	o := decidableOutcome()

	d, err := NewDecision(o, DecisionInput{
		ChosenCandidateID: "cand-defensive",
		// only one rejection explained; the other is left to the sandbox
		Reasons:         map[string]string{"cand-rootcause": "rewrote more than the incident justified"},
		Notes:           "arithmetic carry is easier to prove",
		IncidentSummary: "Checkout returned 500s on orders over $10.",
	})
	if err != nil {
		t.Fatalf("NewDecision: %v", err)
	}

	if d.ChosenCandidateID != "cand-defensive" {
		t.Errorf("chosen = %q", d.ChosenCandidateID)
	}
	// every candidate that was not chosen is recorded, mentioned or not:
	// a record listing only the winner loses that the others were passed over
	if len(d.Rejections) != 2 {
		t.Fatalf("got %d rejections, want 2: %+v", len(d.Rejections), d.Rejections)
	}

	byID := map[string]Rejection{}
	for _, r := range d.Rejections {
		byID[r.CandidateID] = r
	}

	if got := byID["cand-rootcause"]; got.Source != RejectionFromEngineer ||
		got.Reason != "rewrote more than the incident justified" {
		t.Errorf("explained rejection = %+v", got)
	}
	// the unmentioned one falls back to what the sandbox established
	if got := byID["cand-minimal"]; got.Source != RejectionFromVerification ||
		got.Reason != "does not build" {
		t.Errorf("auto-filled rejection = %+v", got)
	}
	if d.EngineerWroteReasons() != 1 {
		t.Errorf("EngineerWroteReasons = %d, want 1", d.EngineerWroteReasons())
	}
	if d.RejectedEverything() {
		t.Error("RejectedEverything is true when a candidate was chosen")
	}
}

func TestDecisionDocumentStatesWhatWasVerified(t *testing.T) {
	o := decidableOutcome()

	d, err := NewDecision(o, DecisionInput{
		ChosenCandidateID: "cand-defensive",
		Reasons:           map[string]string{"cand-rootcause": "too broad"},
		Notes:             "prefer the arithmetic carry",
		IncidentSummary:   "Checkout returned 500s on orders over $10.",
	})
	if err != nil {
		t.Fatalf("NewDecision: %v", err)
	}

	for _, want := range []string{
		"service checkout",
		"Checkout returned 500s",
		"commit 51e85e6",
		"simplify money.Sum carry handling",
		"Chosen fix (defensive strategy)",
		// the claim that must never be overstated, carried verbatim
		"builds, and the service's tests pass",
		"root-cause: too broad [from the engineer]",
		"minimal: does not build [from the sandbox]",
		"prefer the arithmetic carry",
	} {
		if !strings.Contains(d.Document, want) {
			t.Errorf("document is missing %q\n---\n%s", want, d.Document)
		}
	}
}

// the case worth getting right: nobody took a proposed fix and the engineer
// wrote their own, which is a demonstration rather than a rejection
func TestDecisionRecordsTheFixAnEngineerWroteInstead(t *testing.T) {
	o := decidableOutcome()

	d, err := NewDecision(o, DecisionInput{
		Reasons:       map[string]string{"cand-defensive": "all three restructured code the incident did not implicate"},
		EngineerFix:   "widened the accumulator and left the branch structure alone",
		EngineerPRURL: "https://github.com/example/repo/pull/2",
	})
	if err != nil {
		t.Fatalf("NewDecision: %v", err)
	}

	if !d.RejectedEverything() {
		t.Error("RejectedEverything is false when nothing was chosen")
	}
	if len(d.Rejections) != 3 {
		t.Errorf("got %d rejections, want all 3", len(d.Rejections))
	}

	for _, want := range []string{
		"Chosen fix: none. All 3 proposed fixes were rejected.",
		"The engineer fixed it instead by: widened the accumulator",
		"https://github.com/example/repo/pull/2",
	} {
		if !strings.Contains(d.Document, want) {
			t.Errorf("document is missing %q\n---\n%s", want, d.Document)
		}
	}
}

func TestNewDecisionRejectsAnEmptyDecision(t *testing.T) {
	// no choice, no fix, no note: a record that would be embedded and recalled
	// while saying nothing at all
	_, err := NewDecision(decidableOutcome(), DecisionInput{})
	if !errors.Is(err, ErrEmptyDecision) {
		t.Errorf("err = %v, want ErrEmptyDecision", err)
	}
}

func TestNewDecisionRejectsCandidatesFromAnotherInvestigation(t *testing.T) {
	o := decidableOutcome()

	if _, err := NewDecision(o, DecisionInput{ChosenCandidateID: "cand-elsewhere"}); !errors.Is(err, ErrForeignCandidate) {
		t.Errorf("chosen from elsewhere: err = %v, want ErrForeignCandidate", err)
	}

	// and the same for a reason keyed to an id this run never produced, or a
	// typo would file judgement against another incident's candidate
	_, err := NewDecision(o, DecisionInput{
		ChosenCandidateID: "cand-defensive",
		Reasons:           map[string]string{"cand-elsewhere": "nope"},
	})
	if !errors.Is(err, ErrForeignCandidate) {
		t.Errorf("reason for an unknown candidate: err = %v, want ErrForeignCandidate", err)
	}
}

// A candidate that never produced a diff was not judged, and saying "not
// chosen" would imply a comparison that never happened.
func TestUnusableCandidateSaysItFailedRatherThanThatItLost(t *testing.T) {
	o := decidableOutcome()
	o.Candidates = append(o.Candidates, Candidate{
		ID: "cand-broken", Strategy: "minimal-2",
		Status: CandidateFailed,
		Error:  "the model proposed no change",
	})

	d, err := NewDecision(o, DecisionInput{ChosenCandidateID: "cand-defensive"})
	if err != nil {
		t.Fatalf("NewDecision: %v", err)
	}

	for _, r := range d.Rejections {
		if r.CandidateID != "cand-broken" {
			continue
		}
		if !strings.Contains(r.Reason, "never produced a usable change") {
			t.Errorf("reason = %q", r.Reason)
		}
		return
	}
	t.Error("the failed candidate was not recorded at all")
}

// A service with no test suite must never have a decision that reads as though
// its tests passed.
func TestDocumentDoesNotClaimTestsForAServiceWithoutThem(t *testing.T) {
	o := decidableOutcome()
	// tests are a property of the service, so no candidate for one without them
	// can have run any
	for i := range o.Candidates {
		o.Candidates[i].Verification = Verification{
			Applied: true, BuildRan: true, Built: true,
		}
	}

	d, err := NewDecision(o, DecisionInput{ChosenCandidateID: "cand-defensive"})
	if err != nil {
		t.Fatalf("NewDecision: %v", err)
	}

	if strings.Contains(d.Document, "tests pass") {
		t.Errorf("document claims tests passed for a build-only candidate\n---\n%s", d.Document)
	}
	if !strings.Contains(d.Document, "declares no tests") {
		t.Errorf("document does not say the service has no tests\n---\n%s", d.Document)
	}
}
