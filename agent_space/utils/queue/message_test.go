package queue

import (
	"errors"
	"strings"
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
		{"unknown field", strings.TrimSuffix(sampleBody, "}") + `,"unexpected":true}`},
		{"wrong schema version", strings.Replace(sampleBody, `"schema_version":"1.0"`, `"schema_version":"2.0"`, 1)},
		{"wrong producer", strings.Replace(sampleBody, `"producer":"static-log-analysis"`, `"producer":"someone-else"`, 1)},
		{"correlation mismatch", strings.Replace(sampleBody, `"correlation_id":"019fe42e-18e1-7936-8051-ce2536637167"`, `"correlation_id":"019fe42e-18e1-7936-8051-ce2536637168"`, 1)},
		{"invalid classification", strings.Replace(sampleBody, `"classification":"SENSITIVE"`, `"classification":"SECRET"`, 1)},
		{"invalid severity", strings.Replace(sampleBody, `"severity":"error"`, `"severity":"urgent"`, 1)},
		{"trailing json", sampleBody + `{}`},
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

func TestParseMessagePinsTransportMetadataAndConfiguredScope(t *testing.T) {
	message := Message{
		MessageID: "aws-transport-id",
		Body:      sampleBody,
		MessageAttributes: map[string]string{
			"message_id":        "019fe42e-18e1-7937-8c97-4be21ad3b984",
			"deduplication_key": "assignment:019fe42e-18e1-7936-8051-ce2536637167",
			"message_type":      AssignmentType,
			"region":            "us-east-1",
		},
	}
	boundary := Boundary{Region: "us-east-1", TenantID: "local", Classification: "SENSITIVE"}

	if _, err := ParseMessage(message, boundary); err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*Message, *Boundary)
		wantErr error
	}{
		{"missing required attribute", func(m *Message, _ *Boundary) { delete(m.MessageAttributes, "region") }, ErrPermanent},
		{"additional attribute", func(m *Message, _ *Boundary) { m.MessageAttributes["extra"] = "value" }, ErrPermanent},
		{"message id mismatch", func(m *Message, _ *Boundary) {
			m.MessageAttributes["message_id"] = "019fe42e-18e1-7937-8c97-4be21ad3b985"
		}, ErrPermanent},
		{"dedup mismatch", func(m *Message, _ *Boundary) { m.MessageAttributes["deduplication_key"] = "assignment:other" }, ErrPermanent},
		{"type mismatch", func(m *Message, _ *Boundary) { m.MessageAttributes["message_type"] = "agent.assignment.v2" }, ErrPermanent},
		{"attribute region mismatch", func(m *Message, _ *Boundary) { m.MessageAttributes["region"] = "us-west-2" }, ErrPermanent},
		{"configured region mismatch", func(_ *Message, b *Boundary) { b.Region = "us-west-2" }, ErrConfiguration},
		{"configured tenant mismatch", func(_ *Message, b *Boundary) { b.TenantID = "another" }, ErrConfiguration},
		{"configured classification mismatch", func(_ *Message, b *Boundary) { b.Classification = "INTERNAL" }, ErrConfiguration},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := message
			candidate.MessageAttributes = make(map[string]string, len(message.MessageAttributes))
			for key, value := range message.MessageAttributes {
				candidate.MessageAttributes[key] = value
			}
			candidateBoundary := boundary
			tc.mutate(&candidate, &candidateBoundary)
			if _, err := ParseMessage(candidate, candidateBoundary); !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
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
