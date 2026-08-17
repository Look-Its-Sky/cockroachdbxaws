// Package queue receives assignments from SQS and knows nothing about the
// agent, so the worker tests without AWS and the client without an LLM.
package queue

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// the only message type this consumer understands
const (
	AssignmentType          = "agent.assignment.v1"
	assignmentSchemaVersion = "1.0"
	assignmentProducer      = "static-log-analysis"
	maxAssignmentBytes      = 16 << 10
	maxAssignmentTextBytes  = 128
)

var uuidV7Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// a message that will fail identically however many times it is redelivered,
// so the worker deletes it instead of letting the queue retry into a DLQ
var ErrPermanent = errors.New("queue: message cannot be processed by retrying")

// ErrConfiguration means the assignment is structurally valid but outside the
// worker's trusted deployment boundary. Deleting it would turn one bad worker
// setting into silent work loss, so callers must leave it for redelivery.
var ErrConfiguration = errors.New("queue: assignment conflicts with worker configuration")

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
	ContextVersion  int64     `json:"context_version"`
}

// Boundary is the trusted deployment scope. It comes from the worker's
// configuration, never from an assignment body.
type Boundary struct {
	Region         string
	TenantID       string
	Classification string
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
	if len(body) == 0 || len(body) > maxAssignmentBytes {
		return a, fmt.Errorf("%w: body size is outside the assignment limit", ErrPermanent)
	}
	decoder := json.NewDecoder(bytes.NewBufferString(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&a); err != nil {
		return a, fmt.Errorf("%w: decode body: %v", ErrPermanent, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return a, fmt.Errorf("%w: body contains trailing JSON", ErrPermanent)
	}

	if a.SchemaVersion != assignmentSchemaVersion || a.MessageType != AssignmentType ||
		a.Producer != assignmentProducer || !uuidV7Pattern.MatchString(a.MessageID) ||
		!uuidV7Pattern.MatchString(a.CorrelationID) || !uuidV7Pattern.MatchString(a.InvestigationID) ||
		a.CorrelationID != a.InvestigationID || a.CreatedAt.IsZero() || a.CreatedAt.Location() != time.UTC ||
		!validText(a.Region) || !validText(a.TenantID) || !validText(a.ServiceID) ||
		!validText(a.Environment) || !validIncidentID(a.IncidentID) || a.IncidentGen < 1 ||
		a.ContextVersion < 1 || !validClassification(a.Classification) || !validSeverity(a.Severity) {
		return a, fmt.Errorf("%w: assignment does not satisfy agent.assignment.v1", ErrPermanent)
	}
	return a, nil
}

// ParseMessage checks both the closed assignment body and the producer's exact
// SQS metadata mapping before the worker reads context or spends model tokens.
func ParseMessage(message Message, boundary Boundary) (Assignment, error) {
	a, err := ParseAssignment(message.Body)
	if err != nil {
		return a, err
	}
	if !validText(boundary.Region) || !validText(boundary.TenantID) || !validClassification(boundary.Classification) {
		return a, fmt.Errorf("%w: worker boundary is incomplete", ErrConfiguration)
	}
	attrs := message.MessageAttributes
	if len(attrs) != 4 || attrs["message_id"] != a.MessageID ||
		attrs["deduplication_key"] != "assignment:"+a.InvestigationID ||
		attrs["message_type"] != a.MessageType || attrs["region"] != a.Region {
		return a, fmt.Errorf("%w: assignment metadata mismatch", ErrPermanent)
	}
	if a.Region != boundary.Region || a.TenantID != boundary.TenantID ||
		a.Classification != boundary.Classification {
		return a, fmt.Errorf("%w: assignment is outside the configured scope", ErrConfiguration)
	}
	return a, nil
}

func validText(value string) bool {
	if value == "" || len(value) > maxAssignmentTextBytes || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("-_.:/", r)) {
			return false
		}
	}
	return true
}

func validIncidentID(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func validClassification(value string) bool {
	switch value {
	case "PUBLIC", "INTERNAL", "SENSITIVE", "RESTRICTED":
		return true
	default:
		return false
	}
}

func validSeverity(value string) bool {
	switch value {
	case "unspecified", "trace", "debug", "info", "warn", "error", "fatal":
		return true
	default:
		return false
	}
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
