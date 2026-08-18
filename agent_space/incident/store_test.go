package incident

import (
	"errors"
	"strings"
	"testing"
	"time"

	"agent_space/utils/queue"
)

func sampleAssignment() queue.Assignment {
	return queue.Assignment{
		MessageType:     queue.AssignmentType,
		Producer:        "static-log-analysis",
		CorrelationID:   "019fe42e-18e1-7936-8051-ce2536637167",
		IncidentID:      "6424cd115aa6f4be24f943bd4c10e776dbf4459c267fda69520dcd548831b5ed",
		InvestigationID: "019fe42e-18e1-7936-8051-ce2536637167",
		ServiceID:       "payment",
		Environment:     "production",
		Severity:        "error",
		ContextVersion:  1,
	}
}

const analysisSnapshot = `{"kind":"map","value":{"schema_version":{"kind":"string","value":"1.0"},"incident_id":{"kind":"string","value":"6424cd115aa6f4be24f943bd4c10e776dbf4459c267fda69520dcd548831b5ed"},"generation":{"kind":"int","value":3387192194469240033},"record_id":{"kind":"string","value":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"rule_id":{"kind":"string","value":"ordinary_error_v1"},"threshold":{"kind":"int","value":5},"window_seconds":{"kind":"int","value":300},"representative_error":{"kind":"string","value":"ERROR settle.write pool exhausted"}}}`

func TestContextFromAnalysisSnapshot(t *testing.T) {
	a := sampleAssignment()
	a.IncidentGen = 3387192194469240033
	created := time.Date(2026, 8, 9, 0, 31, 12, 0, time.UTC)

	got, err := contextFromAnalysisSnapshot(a, []byte(analysisSnapshot), created)
	if err != nil {
		t.Fatalf("contextFromAnalysisSnapshot: %v", err)
	}
	if got.IncidentID != a.IncidentID || got.ContextVersion != a.ContextVersion {
		t.Fatalf("identity/version = %q/%d", got.IncidentID, got.ContextVersion)
	}
	if got.Summary != "Rule ordinary_error_v1 reached 5 matching errors within 300 seconds." {
		t.Errorf("summary = %q", got.Summary)
	}
	if got.LogExcerpt != "ERROR settle.write pool exhausted" || !got.DetectedAt.Equal(created) {
		t.Errorf("excerpt/time = %q/%s", got.LogExcerpt, got.DetectedAt)
	}
}

func TestContextFromAnalysisSnapshotRejectsIdentityDrift(t *testing.T) {
	a := sampleAssignment()
	a.IncidentGen = 99
	if _, err := contextFromAnalysisSnapshot(a, []byte(analysisSnapshot), time.Now().UTC()); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("error = %v, want ErrInvalidContext", err)
	}
}

func sampleContext() Context {
	return Context{
		IncidentID:     "6424cd115aa6f4be24f943bd4c10e776dbf4459c267fda69520dcd548831b5ed",
		ContextVersion: 1,
		ServiceID:      "payment",
		Environment:    "production",
		Severity:       "error",
		Summary:        "Settlement error rate on /api/settle rose from 0.15% to 10.8%.",
		LogExcerpt:     "ERROR settle.write failed to acquire connection: pool exhausted",
		DetectedAt:     time.Date(2026, 8, 9, 0, 31, 12, 0, time.UTC),
	}
}

func TestBuildQuestionCarriesEverything(t *testing.T) {
	got := BuildQuestion(sampleAssignment(), sampleContext())

	// the agent cannot investigate what it was not told
	want := []string{
		"payment",
		"production",
		"error",
		"2026-08-09T00:31:12Z",
		"static-log-analysis",
		"Settlement error rate on /api/settle rose from 0.15% to 10.8%.",
		"pool exhausted",
		"6424cd115aa6f4be24f943bd4c10e776dbf4459c267fda69520dcd548831b5ed",
		"019fe42e-18e1-7936-8051-ce2536637167",
		"context version 1",
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("question is missing %q:\n%s", w, got)
		}
	}
}

func TestBuildQuestionLeavesTheDecisionToTheSystemPrompt(t *testing.T) {
	got := strings.ToLower(BuildQuestion(sampleAssignment(), sampleContext()))

	// the Runner's system prompt owns the framing; saying it twice means two
	// places to keep in step
	for _, forbidden := range []string{"rollback", "hotfix"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("question restates the decision framing (%q), which the system prompt already sets", forbidden)
		}
	}
}

func TestBuildQuestionFallsBackToTheEnvelope(t *testing.T) {
	// a context row that names no service still yields a usable question,
	// because the assignment named one
	sparse := Context{ContextVersion: 1, Summary: "Errors up."}

	got := BuildQuestion(sampleAssignment(), sparse)
	if !strings.Contains(got, "payment") || !strings.Contains(got, "production") {
		t.Errorf("envelope fields were not used as a fallback:\n%s", got)
	}
	// no detection time means no invented one
	if strings.Contains(got, "Detected at") {
		t.Errorf("a zero detected_at should be omitted:\n%s", got)
	}
}
