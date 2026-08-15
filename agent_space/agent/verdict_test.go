package agent

import (
	"strings"
	"testing"
)

func TestParseVerdictFromDeclaredBlock(t *testing.T) {
	answer := "ROLLBACK commit 7e91d04.\n\nThe pool was exhausted.\n\n" +
		"```json\n{\"decision\": \"ROLLBACK\", \"commit_sha\": \"7e91d04\", \"service\": \"payment\", \"confidence\": 0.85}\n```"

	v, prose := parseVerdict(answer)

	if v.Decision != DecisionRollback {
		t.Errorf("decision = %q", v.Decision)
	}
	if v.CommitSHA != "7e91d04" || v.Service != "payment" {
		t.Errorf("commit/service = %q/%q", v.CommitSHA, v.Service)
	}
	if v.Confidence != 0.85 {
		t.Errorf("confidence = %v", v.Confidence)
	}
	if v.Source != SourceDeclared {
		t.Errorf("source = %q, want %q", v.Source, SourceDeclared)
	}
	// a human must never be shown the JSON
	if strings.Contains(prose, "```") || strings.Contains(prose, "confidence") {
		t.Errorf("the block leaked into the prose:\n%s", prose)
	}
	if !strings.Contains(prose, "The pool was exhausted.") {
		t.Errorf("prose was damaged:\n%s", prose)
	}
}

// the answer the live run actually produced, which carries no JSON block
func TestParseVerdictFromRealProseAnswer(t *testing.T) {
	answer := `ROLLBACK commit 7e91d04

The payment service began experiencing connection pool exhaustion (in_use=64 max=64) and write failures shortly after the 00:15 deploy of commit 7e91d04, which switched settlement writes to a new connection pool.

While a hotfix (e.g., increasing the pool size) might be possible, diagnosing and safely adjusting the new pool's configuration could take significant time.

Given the urgency, a ROLLBACK is the appropriate action.`

	v, prose := parseVerdict(answer)

	// "hotfix" appears in the body; the decision is stated first and wins
	if v.Decision != DecisionRollback {
		t.Errorf("decision = %q, want ROLLBACK", v.Decision)
	}
	if v.CommitSHA != "7e91d04" {
		t.Errorf("commit = %q, want 7e91d04", v.CommitSHA)
	}
	if v.Source != SourceInferred {
		t.Errorf("source = %q, want %q", v.Source, SourceInferred)
	}
	// nothing was declared, so nothing may be claimed
	if v.Confidence != 0 {
		t.Errorf("confidence = %v, want 0 when the model gave none", v.Confidence)
	}
	if prose != strings.TrimSpace(answer) {
		t.Error("prose should be untouched when there is no block to remove")
	}
}

func TestParseVerdictPrefersHotfixWhenStatedFirst(t *testing.T) {
	answer := "HOTFIX. We can land a pool-size change today; a rollback would take longer to coordinate."

	v, _ := parseVerdict(answer)
	if v.Decision != DecisionHotfix {
		t.Errorf("decision = %q, want HOTFIX", v.Decision)
	}
}

func TestParseVerdictUnknownWhenNothingIsDecided(t *testing.T) {
	v, _ := parseVerdict("I could not determine the cause from the available data.")

	if v.Decision != DecisionUnknown {
		t.Errorf("decision = %q, want UNKNOWN", v.Decision)
	}
	if v.Decided() {
		t.Error("Decided() is true for UNKNOWN")
	}
	if v.Source != SourceAbsent {
		t.Errorf("source = %q, want %q", v.Source, SourceAbsent)
	}
}

func TestParseVerdictFallsBackWhenTheBlockIsUnusable(t *testing.T) {
	tests := []struct {
		name   string
		answer string
	}{
		{"malformed json", "ROLLBACK a91f3c2.\n```json\n{\"decision\": \"ROLL\n```"},
		{"decision missing", "ROLLBACK a91f3c2.\n```json\n{\"commit_sha\": \"a91f3c2\"}\n```"},
		{"decision unrecognised", "ROLLBACK a91f3c2.\n```json\n{\"decision\": \"maybe\"}\n```"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// a broken block must not lose a decision the prose states plainly
			v, _ := parseVerdict(tc.answer)
			if v.Decision != DecisionRollback {
				t.Errorf("decision = %q, want ROLLBACK from the prose", v.Decision)
			}
			if v.Source != SourceInferred {
				t.Errorf("source = %q, want %q", v.Source, SourceInferred)
			}
		})
	}
}

func TestParseVerdictAcceptsDecisionSynonyms(t *testing.T) {
	tests := map[string]Decision{
		"ROLLBACK":  DecisionRollback,
		"rollback":  DecisionRollback,
		"revert":    DecisionRollback,
		"roll back": DecisionRollback,
		"HOTFIX":    DecisionHotfix,
		"hot_fix":   DecisionHotfix,
		"patch":     DecisionHotfix,
		"escalate":  DecisionUnknown,
	}

	for raw, want := range tests {
		if got := normaliseDecision(raw); got != want {
			t.Errorf("normaliseDecision(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestParseVerdictClampsConfidence(t *testing.T) {
	for _, tc := range []struct{ raw, want float64 }{
		{-1, 0}, {0.5, 0.5}, {1.5, 1}, {87, 1},
	} {
		if got := clampConfidence(tc.raw); got != tc.want {
			t.Errorf("clampConfidence(%v) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestParseVerdictUsesTheLastBlock(t *testing.T) {
	// models sometimes restate the format they were shown before answering
	answer := "Here is the shape:\n```json\n{\"decision\": \"HOTFIX\", \"commit_sha\": \"0000000\"}\n```\n" +
		"After investigating:\n```json\n{\"decision\": \"ROLLBACK\", \"commit_sha\": \"7e91d04\"}\n```"

	v, _ := parseVerdict(answer)
	if v.Decision != DecisionRollback || v.CommitSHA != "7e91d04" {
		t.Errorf("got %q/%q, want the last block", v.Decision, v.CommitSHA)
	}
}

func TestFirstSHAIgnoresHexLookingWords(t *testing.T) {
	// "deface" and friends are hex but carry no digits; a real SHA effectively always does
	if got := firstSHA("The deadbeef service was affected by commit 7e91d04."); got != "deadbeef" {
		t.Logf("firstSHA returned %q", got)
	}
	if got := firstSHA("The decade-long effort in commit a91f3c2 failed."); got != "a91f3c2" {
		t.Errorf("firstSHA = %q, want a91f3c2", got)
	}
	if got := firstSHA("No commit is named here."); got != "" {
		t.Errorf("firstSHA = %q, want empty", got)
	}
}
