// Package queuetest provides a controllable outbox transport.
//
// It exists for the failures a real queue endpoint will not produce on demand:
// a revoked credential, a throttled request, a queue deleted mid-flight, an
// acknowledgement that never arrives. What a queue accepts, and how its own
// errors map onto the publisher's three failure classes, is a property of the
// concrete adapter and belongs in that adapter's tests against a real
// SQS-compatible endpoint.
//
// A test using this transport pins the publisher's decisions, never the queue's.
package queuetest

import (
	"context"
	"sync"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/outbox"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/queue"
)

// DeadLetter is one message the publisher gave up on, with the categorical
// reason it recorded.
type DeadLetter struct {
	Message queue.Message
	Reason  outbox.Reason
}

// Transport records everything it is asked to deliver and answers however the
// test tells it to. Every field is safe to set before the publisher starts and
// safe to read afterwards.
type Transport struct {
	mu           sync.Mutex
	region       string
	published    []queue.Message
	deadLettered []DeadLetter

	// PublishErr and DeadLetterErr answer for one message. A nil result, or a
	// nil function, means the queue accepted it.
	PublishErr    func(queue.Message) error
	DeadLetterErr func(queue.Message) error
	// BeforeSend runs before Publish decides anything, so a test can block a
	// delivery in flight or cancel the cycle around it.
	BeforeSend func(queue.Message)
}

func New(region string) *Transport { return &Transport{region: region} }

func (t *Transport) Region() string { return t.region }

func (t *Transport) Publish(ctx context.Context, message queue.Message) error {
	if t.BeforeSend != nil {
		t.BeforeSend(message)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.PublishErr != nil {
		if err := t.PublishErr(message); err != nil {
			return err
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.published = append(t.published, message)
	return nil
}

func (t *Transport) DeadLetter(_ context.Context, message queue.Message, reason outbox.Reason) error {
	if t.DeadLetterErr != nil {
		if err := t.DeadLetterErr(message); err != nil {
			return err
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.deadLettered = append(t.deadLettered, DeadLetter{Message: message, Reason: reason})
	return nil
}

// Delivered returns every message the queue accepted, in delivery order and
// including duplicates: at-least-once delivery is the contract, so a test that
// could not see a duplicate could not tell one from a lost message.
func (t *Transport) Delivered() []queue.Message {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]queue.Message(nil), t.published...)
}

func (t *Transport) DeadLetters() []DeadLetter {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]DeadLetter(nil), t.deadLettered...)
}

var _ outbox.Transport = (*Transport)(nil)
