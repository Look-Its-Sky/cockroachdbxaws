package queue_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/queue"
)

func valid() queue.Message {
	return queue.Message{
		MessageID:        "0194f0a0-0000-7000-8000-000000000001",
		DeduplicationKey: "assignment:incident-1:generation-1",
		Type:             "agent.assignment.v1",
		Body:             []byte(`{"schema_version":"1.0"}`),
		Attributes:       map[string]string{"region": "us-east-1"},
	}
}

func TestValidateAcceptsAPublishableMessage(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid message rejected: %v", err)
	}
}

func TestValidateRejectsStructurallyUnpublishableMessages(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*queue.Message)
		want    string
		because string
	}{
		{
			name:    "blank message id",
			mutate:  func(m *queue.Message) { m.MessageID = "" },
			want:    "message_id",
			because: "the outbox row must be identifiable across republish attempts",
		},
		{
			name:    "whitespace-only deduplication key",
			mutate:  func(m *queue.Message) { m.DeduplicationKey = "\t" },
			want:    "deduplication_key",
			because: "delivery is unordered, so identity comes from the key alone",
		},
		{
			name:    "blank type",
			mutate:  func(m *queue.Message) { m.Type = "" },
			want:    "type",
			because: "a consumer must be able to reject a payload it does not understand",
		},
		{
			name:    "empty body",
			mutate:  func(m *queue.Message) { m.Body = nil },
			want:    "body",
			because: "an empty payload carries no versioned content",
		},
		{
			name:    "blank attribute name",
			mutate:  func(m *queue.Message) { m.Attributes[" "] = "x" },
			want:    "must not be empty or whitespace-only",
			because: "an unnamed attribute cannot be routed on",
		},
		{
			name: "too many attributes",
			mutate: func(m *queue.Message) {
				for i := 0; i <= queue.MaxAttributes; i++ {
					m.Attributes[fmt.Sprintf("attribute_%d", i)] = "x"
				}
			},
			want:    "over the limit",
			because: "the transport refuses the extra attributes rather than truncating them",
		},
		{
			name:    "attribute with no value",
			mutate:  func(m *queue.Message) { m.Attributes["generation"] = "" },
			want:    "must have a value",
			because: "a valueless attribute is dropped, so routing on it silently stops matching",
		},
		{
			name:    "attribute name with a disallowed character",
			mutate:  func(m *queue.Message) { m.Attributes["incident:id"] = "x" },
			want:    "only letters, digits",
			because: "the transport accepts a narrower character set than a Go map key",
		},
		{
			name:    "attribute name ending in a period",
			mutate:  func(m *queue.Message) { m.Attributes["incident."] = "x" },
			want:    "period",
			because: "a trailing period is refused by the transport",
		},
		{
			name:    "attribute name with consecutive periods",
			mutate:  func(m *queue.Message) { m.Attributes["incident..id"] = "x" },
			want:    "consecutive periods",
			because: "consecutive periods are refused by the transport",
		},
		{
			name:    "attribute name using a reserved prefix",
			mutate:  func(m *queue.Message) { m.Attributes["AWS.trace"] = "x" },
			want:    "reserved prefix",
			because: "the transport reserves that namespace for itself",
		},
		{
			name: "attribute name over the length limit",
			mutate: func(m *queue.Message) {
				m.Attributes[strings.Repeat("n", queue.MaxAttributeNameBytes+1)] = "x"
			},
			want:    "over the limit",
			because: "an over-long name is refused by the transport",
		},
		{
			name:    "message id that is not a uuid",
			mutate:  func(m *queue.Message) { m.MessageID = "outbox-row-1" },
			want:    "message_id",
			because: "an outbox row identifier is a generated sortable identifier",
		},
		{
			name:    "message id that is not version 7",
			mutate:  func(m *queue.Message) { m.MessageID = "9f8b7c6d-5e4f-4a3b-8c9d-0e1f2a3b4c5d" },
			want:    "message_id",
			because: "outbox identifiers sort by creation order, which only version 7 gives",
		},
		{
			name: "message over the queue limit",
			mutate: func(m *queue.Message) {
				m.Body = make([]byte, queue.MaxMessageBytes+1)
			},
			want:    "over the",
			because: "a message the transport will refuse would strand its outbox row forever",
		},
		{
			name: "message pushed over the limit by its attributes",
			mutate: func(m *queue.Message) {
				m.Body = make([]byte, queue.MaxMessageBytes-10)
				m.Attributes["padding"] = strings.Repeat("x", 100)
			},
			want:    "over the",
			because: "the limit covers attribute names and values, not only the body",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message := valid()
			test.mutate(&message)

			err := message.Validate()
			if err == nil {
				t.Fatalf("want a failure naming %s because %s, got none", test.want, test.because)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("want the failure to name %s, got %v", test.want, err)
			}
		})
	}
}

func TestAMessageAtExactlyTheLimitIsPublishable(t *testing.T) {
	message := valid()
	message.Attributes = nil
	message.Body = make([]byte, queue.MaxMessageBytes)

	// The limit is inclusive. Rejecting a message at exactly the limit would
	// lose capacity the transport actually offers.
	if err := message.Validate(); err != nil {
		t.Fatalf("want a message at exactly %d bytes accepted, got %v", queue.MaxMessageBytes, err)
	}
}

func TestSizeCountsBodyAttributeNamesTypesAndValues(t *testing.T) {
	message := queue.Message{
		Body:       []byte("12345"),
		Attributes: map[string]string{"ab": "cde"},
	}

	// The declared data type is billed along with the name and the value, so a
	// message sized on its body alone would be accepted here and refused by the
	// transport.
	want := 5 + len("ab") + len(queue.AttributeDataType) + len("cde")
	if got := message.Size(); got != want {
		t.Fatalf("want %d bytes, got %d", want, got)
	}
}

func TestExactlyTheAttributeLimitIsPublishable(t *testing.T) {
	message := valid()
	message.Attributes = map[string]string{}
	for i := 0; i < queue.MaxAttributes; i++ {
		message.Attributes[fmt.Sprintf("attribute_%d", i)] = "x"
	}

	if err := message.Validate(); err != nil {
		t.Fatalf("want exactly %d attributes accepted, got %v", queue.MaxAttributes, err)
	}
}

func TestAttributeNamesThatTheTransportAccepts(t *testing.T) {
	for _, name := range []string{"region", "incident_id", "incident-id", "incident.id", "v2", "AWSome"} {
		t.Run(name, func(t *testing.T) {
			message := valid()
			message.Attributes = map[string]string{name: "x"}

			if err := message.Validate(); err != nil {
				t.Fatalf("want %s accepted, got %v", name, err)
			}
		})
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	message := queue.Message{}

	err := message.Validate()

	if err == nil {
		t.Fatal("want an empty message rejected, got no error")
	}
	for _, want := range []string{"message_id", "deduplication_key", "type", "body"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("want the failure to name %s, got %v", want, err)
		}
	}
}
