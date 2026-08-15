// Package capture provides a queue.Publisher that records what was published
// and can be made to fail on demand.
//
// Outbox correctness depends on what happens around a failed publish, so the
// harness distinguishes the two failure shapes that matter: a publish that
// failed before anything was delivered, and a publish that delivered and then
// lost its acknowledgement. The second is why a consumer may see a message
// twice.
package capture

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/queue"
)

// FailureMode says where in a publish the injected failure happens.
type FailureMode int

const (
	// FailBeforeDelivery models a request that never reached the queue. The
	// messages are not recorded.
	FailBeforeDelivery FailureMode = iota
	// FailAfterDelivery models a lost acknowledgement: the queue accepted the
	// messages but the caller saw an error and will republish.
	FailAfterDelivery
)

// Publisher is a recording queue.Publisher.
type Publisher struct {
	mu        sync.Mutex
	published []queue.Message
	calls     int
	// injected failures apply to the next calls in order.
	injected []failure
	changed  chan struct{}
}

type failure struct {
	mode FailureMode
	err  error
}

// New returns an empty capturing publisher.
func New() *Publisher {
	return &Publisher{changed: make(chan struct{})}
}

var _ queue.Publisher = (*Publisher)(nil)

// Publish records the messages unless a failure is queued for this call.
//
// Structurally invalid messages are always rejected, because a publisher that
// accepted them would let a test pass while production rejected the same
// payload.
func (p *Publisher) Publish(ctx context.Context, messages []queue.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for i, message := range messages {
		if err := message.Validate(); err != nil {
			return fmt.Errorf("message %d: %w", i, err)
		}
	}

	p.mu.Lock()
	p.calls++
	var injected *failure
	if len(p.injected) > 0 {
		next := p.injected[0]
		p.injected = p.injected[1:]
		injected = &next
	}
	if injected == nil || injected.mode == FailAfterDelivery {
		// Copied so a caller that reuses its slice cannot rewrite history.
		p.published = append(p.published, cloneMessages(messages)...)
	}
	p.notifyLocked()
	p.mu.Unlock()

	if injected != nil {
		return injected.err
	}
	return nil
}

// FailNext queues a failure for each of the next count publishes.
//
// Bad arguments panic rather than being ignored: a scenario that asked for a
// failure and silently got none would pass while testing the success path.
func (p *Publisher) FailNext(count int, mode FailureMode, err error) {
	if count <= 0 {
		panic(fmt.Sprintf("capture: FailNext needs a positive count, got %d", count))
	}
	switch mode {
	case FailBeforeDelivery, FailAfterDelivery:
	default:
		panic(fmt.Sprintf("capture: unknown failure mode %d", mode))
	}
	if err == nil {
		panic("capture: FailNext requires a non-nil error")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 0; i < count; i++ {
		p.injected = append(p.injected, failure{mode: mode, err: err})
	}
}

// Messages returns a copy of everything delivered so far, in delivery order.
func (p *Publisher) Messages() []queue.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneMessages(p.published)
}

// Len returns how many messages have been delivered.
func (p *Publisher) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.published)
}

// Calls returns how many times Publish was called, including failed calls.
// A test that asserts on republish behavior needs this as well as Len.
func (p *Publisher) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// DeduplicationKeys returns the delivered keys in order, including repeats. A
// key appearing twice is the duplicate delivery consumers must tolerate.
func (p *Publisher) DeduplicationKeys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys := make([]string, 0, len(p.published))
	for _, message := range p.published {
		keys = append(keys, message.DeduplicationKey)
	}
	return keys
}

// Reset discards recorded messages and queued failures.
func (p *Publisher) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.published = nil
	p.injected = nil
	p.calls = 0
}

// WaitFor blocks until at least count messages have been delivered, returning
// them, or fails with the reason it could not.
func (p *Publisher) WaitFor(ctx context.Context, count int) ([]queue.Message, error) {
	for {
		p.mu.Lock()
		if len(p.published) >= count {
			messages := cloneMessages(p.published)
			p.mu.Unlock()
			return messages, nil
		}
		changed := p.changed
		delivered := len(p.published)
		p.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for %d messages, %d delivered: %w", count, delivered, ctx.Err())
		case <-changed:
		}
	}
}

// WaitForDuration is WaitFor with a timeout, for tests that do not carry a
// context of their own.
func (p *Publisher) WaitForDuration(timeout time.Duration, count int) ([]queue.Message, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return p.WaitFor(ctx, count)
}

// notifyLocked wakes every waiter by closing the current change channel and
// installing a fresh one.
func (p *Publisher) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func cloneMessages(messages []queue.Message) []queue.Message {
	cloned := make([]queue.Message, 0, len(messages))
	for _, message := range messages {
		copied := message
		copied.Body = append([]byte(nil), message.Body...)
		if message.Attributes != nil {
			copied.Attributes = make(map[string]string, len(message.Attributes))
			for key, value := range message.Attributes {
				copied.Attributes[key] = value
			}
		}
		cloned = append(cloned, copied)
	}
	return cloned
}
