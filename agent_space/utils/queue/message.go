// Package queue receives assignments from SQS and knows nothing about the
// agent, so the worker tests without AWS and the client without an LLM.
package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// the only message type this consumer understands
const AssignmentType = "agent.assignment.v1"

// a message that will fail identically however many times it is redelivered,
// so the worker deletes it instead of letting the queue retry into a DLQ
var ErrPermanent = errors.New("queue: message cannot be processed by retrying")

// the assignment envelope, JSON in the SQS body. No description of the incident
// is here: the producer sends identifiers and the consumer fetches the prose.
type Assignment struct {
	SchemaVersion   string    `json:"schema_version"`
	MessageID       string    `json:"message_id"`
	MessageType     string    `json:"message_type"`
	CreatedAt       time.Time `json:"created_at"`
	Region          string    `json:"region"`
	TenantID        string    `json:"tenant_id"`
	Classification  string    `json:"classification"`
	Producer        string    `json:"producer"`
	CorrelationID   string    `json:"correlation_id"`
	IncidentID      string    `json:"incident_id"`
	IncidentGen     int64     `json:"incident_generation"`
	InvestigationID string    `json:"investigation_id"`
	ServiceID       string    `json:"service_id"`
	Environment     string    `json:"environment"`
	Severity        string    `json:"severity"`
	ContextVersion  int       `json:"context_version"`
}

// one message as received, flattened out of the SQS types so the worker and
// its tests share a shape that is trivial to construct.
type Message struct {
	MessageID     string
	ReceiptHandle string
	Body          string
	// SQS system attributes, e.g. ApproximateReceiveCount.
	Attributes map[string]string
	// producer-set attributes, e.g. deduplication_key
	MessageAttributes map[string]string
}

// how many times SQS has handed this out, counting this delivery; absent reads
// as 1, so a missing count cannot look like exhausted attempts
func (m Message) ReceiveCount() int {
	n, err := strconv.Atoi(m.Attributes["ApproximateReceiveCount"])
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// the producer's idempotency key, falling back to the message id
func (m Message) DeduplicationKey() string {
	if key := strings.TrimSpace(m.MessageAttributes["deduplication_key"]); key != "" {
		return key
	}
	return m.MessageID
}

// decode and validate the envelope; every failure here is permanent, since a
// bad body will not become valid on the next delivery
func ParseAssignment(body string) (Assignment, error) {
	var a Assignment
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		return a, fmt.Errorf("%w: decode body: %v", ErrPermanent, err)
	}

	if a.MessageType != AssignmentType {
		return a, fmt.Errorf("%w: message_type is %q, want %q", ErrPermanent, a.MessageType, AssignmentType)
	}
	if strings.TrimSpace(a.IncidentID) == "" {
		return a, fmt.Errorf("%w: incident_id is empty", ErrPermanent)
	}
	if strings.TrimSpace(a.InvestigationID) == "" {
		return a, fmt.Errorf("%w: investigation_id is empty", ErrPermanent)
	}
	if strings.TrimSpace(a.ServiceID) == "" {
		return a, fmt.Errorf("%w: service_id is empty", ErrPermanent)
	}

	return a, nil
}

// a short identifier for logs: the investigation is the unit of work, the
// incident is what it is about.
func (a Assignment) String() string {
	return fmt.Sprintf("investigation %s (incident %s, service %s, severity %s)",
		a.InvestigationID, short(a.IncidentID), a.ServiceID, a.Severity)
}

func short(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12] + "…"
}
