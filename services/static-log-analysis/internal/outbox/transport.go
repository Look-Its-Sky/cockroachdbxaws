// Package outbox publishes committed agent assignments from the CockroachDB
// transactional outbox to the regional agent queue.
//
// ADR 0004 puts the outbox row in the same transaction as the investigation, so
// by the time a message reaches this package it is already durable and already
// owed to an agent. Everything here is therefore about how a delivery attempt
// may fail, not about whether the message is real.
//
// The port below is deliberately owned here rather than in internal/queue: the
// publisher is the only component that has to tell a throttled queue apart from
// a queue that will never accept this message, and the classification is part of
// the port's contract rather than a detail of one adapter.
package outbox

import (
	"context"
	"errors"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/queue"
)

// Transport delivers one message at a time to a regional queue.
//
// Implementations MUST classify their failures with one of the three sentinels
// below. An unclassified failure is treated as retryable, because an
// unrecognized failure is a defect rather than a verdict about the message, and
// calling it permanent would move a message an operator never chose to move.
type Transport interface {
	// Publish delivers one message to the agent queue. A nil error means the
	// queue acknowledged it; anything else means the caller must assume nothing,
	// because a lost acknowledgement is indistinguishable from a lost request.
	Publish(ctx context.Context, message queue.Message) error
	// DeadLetter delivers one undeliverable message to the dead-letter queue,
	// together with the categorical reason it could not be published. The
	// message body is unchanged, so a replay from the dead-letter queue is a
	// replay of exactly what was committed.
	DeadLetter(ctx context.Context, message queue.Message, reason Reason) error
	// Region is the region this transport delivers into. It exists so a
	// publisher can refuse at startup to move regional content across a
	// regional boundary, rather than discovering the mismatch per message.
	Region() string
}

// The three failure classes a transport must choose between. They are sentinels
// rather than an enumeration so a transport can wrap its own diagnostic error,
// and so a test double can inject one without depending on this package's
// internal shape.
//
// The distinction is the whole point of this package. A permanent failure
// reported as retryable makes the publisher replay a poisoned message forever
// and starve its siblings. A retryable failure reported as permanent moves a
// committed assignment to the dead-letter queue that a second attempt would
// have delivered.
var (
	// ErrRetryable means this attempt failed and a later identical attempt may
	// succeed: throttling, a timeout, a 5xx, a connection that would not open.
	ErrRetryable = errors.New("outbox: retryable publish failure")

	// ErrMessageRejected means this exact message will never be accepted, no
	// matter how often it is offered: it is malformed, or it is larger than the
	// transport will carry. It is record-local, so it is the only class that may
	// reach the dead-letter path.
	ErrMessageRejected = errors.New("outbox: message permanently rejected")

	// ErrDeploymentFault means the publisher itself is wrong: the queue does not
	// exist, the credential may not write to it, or it is not in this region.
	// Nothing about any individual message caused it and nothing about any
	// individual message can fix it, so messages keep their claims and the fault
	// is reported to the operator instead of being spread across the batch.
	//
	// Bisecting a deployment-wide failure down to single messages and
	// dead-lettering each one is the outbox's version of quarantining a whole
	// batch for a configuration typo.
	ErrDeploymentFault = errors.New("outbox: publisher deployment fault")
)

// Reason is the categorical, safe explanation recorded with a message. It is a
// closed set: it is written to the database, attached to a dead-letter message,
// and logged, so it must never be able to carry record-derived text.
type Reason string

const (
	ReasonQueueUnavailable  Reason = "queue_unavailable"
	ReasonTimeout           Reason = "timeout"
	ReasonLostAck           Reason = "lost_ack"
	ReasonRejected          Reason = "rejected"
	ReasonAttemptsExhausted Reason = "attempts_exhausted"
	ReasonRegionMismatch    Reason = "region_mismatch"
	ReasonDeploymentFault   Reason = "deployment_fault"
)

// Valid reports whether r is one of the closed set. Anything else must never
// reach the database or a dead-letter attribute.
func (r Reason) Valid() bool {
	switch r {
	case ReasonQueueUnavailable, ReasonTimeout, ReasonLostAck, ReasonRejected,
		ReasonAttemptsExhausted, ReasonRegionMismatch, ReasonDeploymentFault:
		return true
	default:
		return false
	}
}

// failure maps a reason onto the four categorical failures the outbox row can
// store. The database column is deliberately narrower than this package's
// reasons: what an operator needs from a pending row is whether it is waiting on
// the queue, on a timeout, on a lost acknowledgement, or on a refusal.
func (r Reason) failure() persistence.OutboxFailure {
	switch r {
	case ReasonTimeout:
		return persistence.OutboxFailureTimeout
	case ReasonLostAck:
		return persistence.OutboxFailureLostAck
	case ReasonRejected, ReasonAttemptsExhausted, ReasonRegionMismatch:
		return persistence.OutboxFailureRejected
	default:
		return persistence.OutboxFailureQueueUnavailable
	}
}

// Failure is the error a transport returns. Class is one of the three sentinels
// and Reason is the categorical explanation carried into the database and the
// dead-letter queue.
type Failure struct {
	Class  error
	Reason Reason
	// Detail is a short transport-supplied code such as an AWS error code. It
	// never carries message content: a transport that cannot guarantee that must
	// leave it empty.
	Detail string
}

func (f *Failure) Error() string {
	if f == nil {
		return "<nil failure>"
	}
	text := f.Class.Error() + " (" + string(f.Reason) + ")"
	if f.Detail != "" {
		text += ": " + f.Detail
	}
	return text
}

func (f *Failure) Unwrap() error { return f.Class }

// NewFailure builds a classified transport failure. An invalid reason is
// replaced rather than propagated, because a reason is written to durable state
// and a transport bug must not be able to put arbitrary text there.
func NewFailure(class error, reason Reason, detail string) *Failure {
	if !reason.Valid() {
		reason = ReasonQueueUnavailable
	}
	return &Failure{Class: class, Reason: reason, Detail: safeDetail(detail)}
}

// safeDetail keeps a transport-supplied code to a short identifier-shaped
// string. Anything else is dropped: this value is logged and published as a
// dead-letter attribute, and neither is a place to discover that a dependency
// echoed a payload back in its error text.
func safeDetail(detail string) string {
	const maxDetailBytes = 64
	if len(detail) == 0 || len(detail) > maxDetailBytes {
		return ""
	}
	for _, r := range detail {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return ""
		}
	}
	return detail
}

// classify reduces any transport error to the class and reason the publisher
// acts on.
func classify(err error) (class error, reason Reason) {
	var failure *Failure
	if errors.As(err, &failure) && failure.Class != nil {
		return failure.Class, failure.Reason
	}
	switch {
	case errors.Is(err, ErrDeploymentFault):
		return ErrDeploymentFault, ReasonDeploymentFault
	case errors.Is(err, ErrMessageRejected):
		return ErrMessageRejected, ReasonRejected
	case errors.Is(err, ErrRetryable):
		return ErrRetryable, ReasonQueueUnavailable
	case errors.Is(err, context.DeadlineExceeded):
		return ErrRetryable, ReasonTimeout
	default:
		// An unrecognized failure is a defect in a transport, not a verdict
		// about the message. Treating it as permanent would move a committed
		// assignment to the dead-letter queue on the strength of a bug.
		return ErrRetryable, ReasonQueueUnavailable
	}
}
