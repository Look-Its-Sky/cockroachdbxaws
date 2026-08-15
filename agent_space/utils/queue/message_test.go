package queue

import (
	"errors"
	"testing"
	"time"
)

// the body of a real message from the static-log-analysis producer, copied
// verbatim. If the producer's envelope changes, this test is where it shows.
const sampleBody = `{"schema_version":"1.0","message_id":"019fe42e-18e1-7937-8c97-4be21ad3b984","message_type":"agent.assignment.v1","created_at":"2026-08-09T01:40:54.096548Z","region":"us-east-1","tenant_id":"local","classification":"SENSITIVE","producer":"static-log-analysis","correlation_id":"019fe42e-18e1-7936-8051-ce2536637167","incident_id":"6424cd115aa6f4be24f943bd4c10e776dbf4459c267fda69520dcd548831b5ed","incident_generation":3387192194469240033,"investigation_id":"019fe42e-18e1-7936-8051-ce2536637167","service_id":"payment","environment":"production","severity":"error","context_version":1}`

func TestParseAssignmentSample(t *testing.T) {
	a, err := ParseAssignment(sampleBody)
	if err != nil {
		t.Fatalf("ParseAssignment: %v", err)
	}

	if a.IncidentID != "6424cd115aa6f4be24f943bd4c10e776dbf4459c267fda69520dcd548831b5ed" {
		t.Errorf("incident_id = %q", a.IncidentID)
	}
	if a.InvestigationID != "019fe42e-18e1-7936-8051-ce2536637167" {
		t.Errorf("investigation_id = %q", a.InvestigationID)
	}
	if a.ServiceID != "payment" || a.Environment != "production" || a.Severity != "error" {
		t.Errorf("service/environment/severity = %q/%q/%q", a.ServiceID, a.Environment, a.Severity)
	}
	if a.ContextVersion != 1 {
		t.Errorf("context_version = %d, want 1", a.ContextVersion)
	}
	// large enough that a float64 round-trip would corrupt it
	if a.IncidentGen != 3387192194469240033 {
		t.Errorf("incident_generation = %d, want 3387192194469240033", a.IncidentGen)
	}
	if want := time.Date(2026, 8, 9, 1, 40, 54, 96548000, time.UTC); !a.CreatedAt.Equal(want) {
		t.Errorf("created_at = %s, want %s", a.CreatedAt, want)
	}
	if a.Classification != "SENSITIVE" || a.Producer != "static-log-analysis" {
		t.Errorf("classification/producer = %q/%q", a.Classification, a.Producer)
	}
}

func TestParseAssignmentRejections(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not json", "not json"},
		{"empty", ""},
		{"wrong message type", `{"message_type":"agent.verdict.v1","incident_id":"i","investigation_id":"v","service_id":"s"}`},
		{"missing message type", `{"incident_id":"i","investigation_id":"v","service_id":"s"}`},
		{"missing incident id", `{"message_type":"agent.assignment.v1","investigation_id":"v","service_id":"s"}`},
		{"missing investigation id", `{"message_type":"agent.assignment.v1","incident_id":"i","service_id":"s"}`},
		{"missing service id", `{"message_type":"agent.assignment.v1","incident_id":"i","investigation_id":"v"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAssignment(tc.body)
			if err == nil {
				t.Fatal("expected an error")
			}
			// the worker deletes on ErrPermanent; if these were not permanent
			// a malformed message would loop until the DLQ took it
			if !errors.Is(err, ErrPermanent) {
				t.Errorf("error is not ErrPermanent: %v", err)
			}
		})
	}
}

func TestReceiveCount(t *testing.T) {
	tests := []struct {
		name string
		attr map[string]string
		want int
	}{
		{"absent", nil, 1},
		{"first delivery", map[string]string{"ApproximateReceiveCount": "1"}, 1},
		{"redelivered", map[string]string{"ApproximateReceiveCount": "3"}, 3},
		// a missing count must not read as "already exhausted"
		{"garbage", map[string]string{"ApproximateReceiveCount": "many"}, 1},
		{"zero", map[string]string{"ApproximateReceiveCount": "0"}, 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Message{Attributes: tc.attr}).ReceiveCount(); got != tc.want {
				t.Errorf("ReceiveCount() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDeduplicationKey(t *testing.T) {
	m := Message{
		MessageID:         "fbe22154",
		MessageAttributes: map[string]string{"deduplication_key": "assignment:019fe42e-18e1-7936-8051-ce2536637167"},
	}
	if got := m.DeduplicationKey(); got != "assignment:019fe42e-18e1-7936-8051-ce2536637167" {
		t.Errorf("DeduplicationKey() = %q", got)
	}

	// without the attribute the message id is the best available key
	if got := (Message{MessageID: "fbe22154"}).DeduplicationKey(); got != "fbe22154" {
		t.Errorf("DeduplicationKey() fallback = %q, want the message id", got)
	}
}
