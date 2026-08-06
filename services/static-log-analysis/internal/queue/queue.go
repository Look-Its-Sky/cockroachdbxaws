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
	// derived content.
	Attributes map[string]string
}

// Publishing limits.
//
// These are this service's limits, not a mirror of whatever the transport
// currently permits. They are chosen to fit SQS Standard, but a message that
// satisfies them is a message this service is willing to produce, and a change
// in the transport's own limits does not silently change what this service
// emits. A payload approaching either bound is a design problem in the payload:
// assignments carry pointers and scheduling metadata, not evidence.
const (
	// MaxMessageBytes bounds one whole message: the body plus each attribute's
	// name, declared data type, and value, matching how the transport bills a
	// message rather than how the body alone reads.
	MaxMessageBytes = 256 * 1024

	// MaxAttributes bounds how many attributes a message may carry.
	MaxAttributes = 10

	// MaxAttributeNameBytes bounds one attribute name.
	MaxAttributeNameBytes = 256
)

// AttributeDataType is the declared type of every attribute this service
// publishes. Attributes carry routing and diagnostic metadata as text; nothing
// binary and nothing log-derived travels in them.
const AttributeDataType = "String"

// reservedAttributePrefixes are refused by the transport because it uses them
// itself. Rejecting them here means the outbox row never becomes unpublishable.
var reservedAttributePrefixes = []string{"aws.", "amazon."}

// Validate reports whether a message is structurally publishable.
//
// It does not yet check that Type names a known payload schema or that the
// payload matches it. That check belongs with the message catalogue, which
// arrives with the agent contract schemas.
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
	}
	if strings.TrimSpace(m.Type) == "" {
		problems = append(problems, "type must not be empty or whitespace-only")
	}
	if len(m.Body) == 0 {
		problems = append(problems, "body must not be empty")
	}
	problems = append(problems, m.attributeProblems()...)
	if size := m.Size(); size > MaxMessageBytes {
		// Discovering this at the transport would strand an outbox row that can
		// never be published, so it is rejected where it is still fixable.
		problems = append(problems, fmt.Sprintf("message is %d bytes, over the %d byte limit",
			size, MaxMessageBytes))
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
	if len(m.Attributes) > MaxAttributes {
		problems = append(problems, fmt.Sprintf("message carries %d attributes, over the limit of %d",
			len(m.Attributes), MaxAttributes))
	}

	names := make([]string, 0, len(m.Attributes))
	for name := range m.Attributes {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if problem := attributeNameProblem(name); problem != "" {
			problems = append(problems, "attribute name "+quote(name)+" "+problem)
		}
		if m.Attributes[name] == "" {
			// An attribute declared with no value is dropped by the transport,
			// so a consumer routing on it would silently stop matching.
			problems = append(problems, "attribute "+quote(name)+" must have a value")
		}
	}
	return problems
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

// Size returns the bytes this message counts against MaxMessageBytes: the body
// plus each attribute's name, declared data type, and value.
func (m Message) Size() int {
	size := len(m.Body)
	for name, value := range m.Attributes {
		size += len(name) + len(AttributeDataType) + len(value)
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
