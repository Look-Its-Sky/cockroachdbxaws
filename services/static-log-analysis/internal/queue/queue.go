// Package queue defines the narrow publishing boundary the outbox writes
// through. The concrete implementation is Amazon SQS Standard with a dead
// letter queue; nothing above this boundary depends on that.
package queue

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/ids"
)

// Message is one investigation assignment or other outbox payload.
//
// Delivery is at least once and unordered, so every consumer is idempotent and
// DeduplicationKey rather than arrival order decides identity.
type Message struct {
	// MessageID is the outbox row identifier, reused across republish attempts.
	MessageID string
	// DeduplicationKey is unique per logical message. A republished message
	// carries the key it was first published with.
	DeduplicationKey string
	// Type names the payload schema, so a consumer can reject a message it does
	// not understand instead of guessing.
	Type string
	// Body is the versioned serialized payload.
	Body []byte
	// Attributes carry routing and diagnostic metadata. They never carry log
	// derived content. Individual catalogue entries may close this set further;
	// agent.assignment.v1 permits exactly its scoped region attribute.
	Attributes map[string]string
}

// Publishing limits. The transport maximum and the application's deliberately
// smaller safety limit have different names because changing an AWS limit must
// not silently change what this service is willing to put in an assignment.
const (
	// SQSMaxMessageBytes is Amazon SQS's hard maximum for one message, including
	// its body and message attributes.
	SQSMaxMessageBytes = 1024 * 1024

	// ApplicationSafetyMaxMessageBytes is the smaller limit this service elects
	// to publish. Assignments carry pointers and scheduling metadata rather than
	// evidence, so approaching even this limit indicates a payload design error.
	ApplicationSafetyMaxMessageBytes = 256 * 1024

	// MaxAttributes bounds how many attributes a message may carry.
	MaxAttributes = 10
	// RequiredSQSAttributes is the domain metadata every message maps to String
	// attributes so an unordered, at-least-once consumer can identify it.
	RequiredSQSAttributes = 3
	// MaxCustomAttributes leaves room for the required metadata within SQS's
	// ten-attribute hard maximum.
	MaxCustomAttributes = MaxAttributes - RequiredSQSAttributes

	// MaxAttributeNameBytes bounds one attribute name.
	MaxAttributeNameBytes = 256
)

// SQSStringAttributeDataType is the DataType used when each Attributes entry is
// mapped to an SQS MessageAttributeValue. The map key becomes the SQS attribute
// name and the map value becomes StringValue. Attributes carry routing and
// diagnostic metadata only; nothing binary and nothing log-derived travels in
// them.
const SQSStringAttributeDataType = "String"

// Required SQS message-attribute names. SQS Standard has no native
// deduplication-key or application message-type field, so these values must be
// carried explicitly rather than mistaken for SQS's transport-assigned ID.
const (
	SQSAttributeMessageID        = "message_id"
	SQSAttributeDeduplicationKey = "deduplication_key"
	SQSAttributeMessageType      = "message_type"
)

var requiredSQSAttributeNames = map[string]bool{
	SQSAttributeMessageID:        true,
	SQSAttributeDeduplicationKey: true,
	SQSAttributeMessageType:      true,
}

// reservedAttributePrefixes are refused by the transport because it uses them
// itself. Rejecting them here means the outbox row never becomes unpublishable.
var reservedAttributePrefixes = []string{"aws.", "amazon."}

// Validate reports whether a message is structurally publishable.
//
// It does not check that Type names a known payload schema or that the payload
// matches it. The typed agent message catalogue performs that domain check at
// the persistence write and claim boundaries.
func (m Message) Validate() error {
	var problems []string
	if strings.TrimSpace(m.MessageID) == "" {
		problems = append(problems, "message_id must not be empty or whitespace-only")
	} else if err := ids.Validate(m.MessageID); err != nil {
		// The outbox row identifier is a generated sortable identifier. A
		// message carrying anything else did not come from the outbox.
		problems = append(problems, "message_id must be a canonical UUIDv7: "+err.Error())
	}
	if strings.TrimSpace(m.DeduplicationKey) == "" {
		problems = append(problems, "deduplication_key must not be empty or whitespace-only")
	} else if !validSQSText([]byte(m.DeduplicationKey)) {
		problems = append(problems, "deduplication_key must be valid SQS text")
	}
	if strings.TrimSpace(m.Type) == "" {
		problems = append(problems, "type must not be empty or whitespace-only")
	} else if !validSQSText([]byte(m.Type)) {
		problems = append(problems, "type must be valid SQS text")
	}
	if len(m.Body) == 0 {
		problems = append(problems, "body must not be empty")
	} else if !validSQSText(m.Body) {
		problems = append(problems, "body must be valid SQS text")
	}
	problems = append(problems, m.attributeProblems()...)
	if size := m.SQSSize(); size > ApplicationSafetyMaxMessageBytes {
		// Discovering this at the transport would strand an outbox row that can
		// never be published, so it is rejected where it is still fixable.
		problems = append(problems, fmt.Sprintf(
			"message is %d bytes, over the %d byte application safety limit (SQS hard maximum is %d bytes)",
			size, ApplicationSafetyMaxMessageBytes, SQSMaxMessageBytes))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid message: %s", strings.Join(problems, "; "))
}

// attributeProblems reports every way this message's attributes are
// unpublishable, in a deterministic order.
func (m Message) attributeProblems() []string {
	var problems []string
	total := RequiredSQSAttributes + len(m.Attributes)
	if total > MaxAttributes {
		problems = append(problems, fmt.Sprintf(
			"message maps to %d attributes (%d required and %d custom), over the limit of %d",
			total, RequiredSQSAttributes, len(m.Attributes), MaxAttributes))
	}

	names := make([]string, 0, len(m.Attributes))
	for name := range m.Attributes {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if requiredSQSAttributeNames[name] {
			problems = append(problems, "attribute "+quote(name)+" is reserved for required transport metadata")
		}
		if problem := attributeNameProblem(name); problem != "" {
			problems = append(problems, "attribute name "+quote(name)+" "+problem)
		}
		if m.Attributes[name] == "" {
			// An attribute declared with no value is dropped by the transport,
			// so a consumer routing on it would silently stop matching.
			problems = append(problems, "attribute "+quote(name)+" must have a value")
		} else if !validSQSText([]byte(m.Attributes[name])) {
			problems = append(problems, "attribute "+quote(name)+" must be valid SQS text")
		}
	}
	return problems
}

// validSQSText implements the XML-compatible Unicode repertoire SQS accepts:
// tab, line feed, carriage return, and scalar values in the documented ranges.
func validSQSText(value []byte) bool {
	if !utf8.Valid(value) {
		return false
	}
	for _, r := range string(value) {
		switch {
		case r == '\t', r == '\n', r == '\r':
		case r >= 0x20 && r <= 0xD7FF:
		case r >= 0xE000 && r <= 0xFFFD:
		case r >= 0x10000 && r <= 0x10FFFF:
		default:
			return false
		}
	}
	return true
}

func attributeNameProblem(name string) string {
	if strings.TrimSpace(name) == "" {
		return "must not be empty or whitespace-only"
	}
	if len(name) > MaxAttributeNameBytes {
		return fmt.Sprintf("is %d bytes, over the limit of %d", len(name), MaxAttributeNameBytes)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.':
		default:
			return fmt.Sprintf("contains %q; only letters, digits, underscore, hyphen, and period are allowed", r)
		}
	}
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return "must not start or end with a period"
	}
	if strings.Contains(name, "..") {
		return "must not contain consecutive periods"
	}
	lowered := strings.ToLower(name)
	for _, prefix := range reservedAttributePrefixes {
		if strings.HasPrefix(lowered, prefix) {
			return "uses the reserved prefix " + quote(prefix)
		}
	}
	return ""
}

// SQSAttributes maps the domain metadata plus custom attributes to the String
// attributes a concrete SQS publisher will send. It returns a fresh map so a
// caller cannot mutate the Message through the mapping.
func (m Message) SQSAttributes() map[string]string {
	attributes := make(map[string]string, RequiredSQSAttributes+len(m.Attributes))
	for name, value := range m.Attributes {
		attributes[name] = value
	}
	attributes[SQSAttributeMessageID] = m.MessageID
	attributes[SQSAttributeDeduplicationKey] = m.DeduplicationKey
	attributes[SQSAttributeMessageType] = m.Type
	return attributes
}

// SQSSize returns the bytes the intended SQS mapping counts toward the message
// limit: MessageBody plus every mapped MessageAttribute name, DataType, and
// StringValue.
func (m Message) SQSSize() int {
	size := len(m.Body)
	for name, value := range m.SQSAttributes() {
		size += len(name) + len(SQSStringAttributeDataType) + len(value)
	}
	return size
}

func quote(s string) string { return `"` + s + `"` }

// Publisher sends messages to the agent queue.
//
// A returned error means the caller must assume nothing about delivery: the
// messages may have been delivered, partially delivered, or not delivered. The
// outbox handles that by republishing, and consumers deduplicate.
type Publisher interface {
	Publish(ctx context.Context, messages []Message) error
}

// ErrPublishFailed is the sentinel a publisher wraps transport failures in so
// callers can distinguish them from programming errors such as an invalid
// message.
var ErrPublishFailed = errors.New("publish failed")
