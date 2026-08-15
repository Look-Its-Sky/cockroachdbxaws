package remediation

import (
	"strings"
	"testing"
)

// the one thing this must never do is let a service with no tests look verified
func TestVerificationSummaryNeverClaimsTestsThatDidNotRun(t *testing.T) {
	buildOnly := Verification{Applied: true, BuildRan: true, Built: true}

	summary := buildOnly.Summary()
	if strings.Contains(summary, "tests pass") {
		t.Errorf("a build-only run claims passing tests: %q", summary)
	}
	if !strings.Contains(summary, "no tests") {
		t.Errorf("a build-only run does not say the service has no tests: %q", summary)
	}
	if got := buildOnly.Strength(); got != StrengthBuilds {
		t.Errorf("strength = %v, want StrengthBuilds", got)
	}
}

func TestVerificationStrengthOrdersTheEvidence(t *testing.T) {
	cases := []struct {
		name string
		v    Verification
		want Strength
	}{
		{"never applied", Verification{}, StrengthUnapplied},
		{"applied only", Verification{Applied: true}, StrengthApplied},
		{"builds", Verification{Applied: true, BuildRan: true, Built: true}, StrengthBuilds},
		{
			"tests pass",
			Verification{Applied: true, BuildRan: true, Built: true, TestRan: true, Tested: true},
			StrengthPasses,
		},
		{
			// a failing test suite is weaker than one that was never run, and
			// must not be dressed up as a build success
			"tests fail",
			Verification{Applied: true, BuildRan: true, Built: true, TestRan: true},
			StrengthBuilds,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.Strength(); got != tc.want {
				t.Errorf("strength = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRankPutsTheStrongestEvidenceFirst(t *testing.T) {
	candidates := []Candidate{
		{
			ID: "build-only", Diff: "d",
			Verification: Verification{Applied: true, BuildRan: true, Built: true},
		},
		{ID: "failed", Error: "the model proposed no change"},
		{
			ID: "tested-big", Diff: strings.Repeat("d", 500),
			Verification: Verification{Applied: true, BuildRan: true, Built: true, TestRan: true, Tested: true},
		},
		{
			ID: "tested-small", Diff: strings.Repeat("d", 50),
			Verification: Verification{Applied: true, BuildRan: true, Built: true, TestRan: true, Tested: true},
		},
	}

	Rank(candidates)

	want := []string{"tested-small", "tested-big", "build-only", "failed"}
	for i, id := range want {
		if candidates[i].ID != id {
			t.Fatalf("rank %d = %s, want %s (order: %v)", i, candidates[i].ID, id, ids(candidates))
		}
	}
}

// a candidate with no diff is not a fix, however well its container ran
func TestRankSinksCandidatesWithNothingToShow(t *testing.T) {
	candidates := []Candidate{
		{ID: "empty", Verification: Verification{Applied: true, BuildRan: true, Built: true}},
		{ID: "real", Diff: "diff --git a/a b/a", Verification: Verification{Applied: true}},
	}

	Rank(candidates)

	if candidates[0].ID != "real" {
		t.Errorf("order = %v, want the candidate with a diff first", ids(candidates))
	}
}

func ids(candidates []Candidate) []string {
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.ID)
	}
	return out
}
