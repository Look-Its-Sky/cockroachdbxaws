package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_space/agent"
	"agent_space/incident"
	"agent_space/utils/queue"
)

// the body of a real message from the static-log-analysis producer
const sampleBody = `{"schema_version":"1.0","message_id":"019fe42e-18e1-7937-8c97-4be21ad3b984","message_type":"agent.assignment.v1","created_at":"2026-08-09T01:40:54.096548Z","region":"us-east-1","tenant_id":"local","classification":"SENSITIVE","producer":"static-log-analysis","correlation_id":"019fe42e-18e1-7936-8051-ce2536637167","incident_id":"6424cd115aa6f4be24f943bd4c10e776dbf4459c267fda69520dcd548831b5ed","incident_generation":3387192194469240033,"investigation_id":"019fe42e-18e1-7936-8051-ce2536637167","service_id":"payment","environment":"production","severity":"error","context_version":1}`

const sampleInvestigation = "019fe42e-18e1-7936-8051-ce2536637167"

// ─── doubles ────────────────────────────────────────────────────────────────

// an in-memory queue: messages are handed out once, and what the worker did
// with each one is recorded rather than guessed at from side effects.
type fakeQueue struct {
	mu       sync.Mutex
	pending  []queue.Message
	deleted  []string
	extended []string

	receiveErr error
	extendErr  error
}

func (q *fakeQueue) Receive(ctx context.Context) ([]queue.Message, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.receiveErr != nil {
		return nil, q.receiveErr
	}
	if len(q.pending) == 0 {
		return nil, nil
	}

	msg := q.pending[0]
	q.pending = q.pending[1:]
	return []queue.Message{msg}, nil
}

func (q *fakeQueue) Delete(_ context.Context, receiptHandle string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.deleted = append(q.deleted, receiptHandle)
	return nil
}

func (q *fakeQueue) ExtendVisibility(_ context.Context, receiptHandle string, _ int32) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.extended = append(q.extended, receiptHandle)
	return q.extendErr
}

func (q *fakeQueue) deletedCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.deleted)
}

func (q *fakeQueue) extendedCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.extended)
}

// resolver returning one canned context, or a failure
type fakeResolver struct {
	ctx incident.Context
	err error
}

func (r fakeResolver) Resolve(context.Context, queue.Assignment) (incident.Context, error) {
	return r.ctx, r.err
}

// an agent that records what it was asked and returns what it was told to
type fakeAgent struct {
	mu        sync.Mutex
	calls     int
	questions []string

	result agent.Result
	err    error
	delay  time.Duration
}

func (a *fakeAgent) Run(ctx context.Context, question string, _ int) (agent.Result, error) {
	a.mu.Lock()
	a.calls++
	a.questions = append(a.questions, question)
	delay := a.delay
	a.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return agent.Result{}, ctx.Err()
		}
	}
	return a.result, a.err
}

func (a *fakeAgent) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// ─── helpers ────────────────────────────────────────────────────────────────

func testConfig() queue.Config {
	return queue.Config{
		QueueURL:          "http://localhost:4566/000000000000/static-log-analysis",
		VisibilityTimeout: 90 * time.Millisecond, // heartbeats every 30ms
		WaitTime:          time.Millisecond,
		RunTimeout:        5 * time.Second,
		MaxAttempts:       5,
	}
}

func newWorker(q queue.Receiver, r ContextResolver, a Investigator) *Worker {
	return &Worker{Queue: q, Resolver: r, Agent: a, Results: NewStore(), Config: testConfig()}
}

func message(body string, receiveCount int) queue.Message {
	return queue.Message{
		MessageID:     "fbe22154-f096-43c0-98cc-405366c8809b",
		ReceiptHandle: "receipt-1",
		Body:          body,
		Attributes:    map[string]string{"ApproximateReceiveCount": fmt.Sprint(receiveCount)},
		MessageAttributes: map[string]string{
			"deduplication_key": "assignment:" + sampleInvestigation,
		},
	}
}

func resolvedContext() incident.Context {
	return incident.Context{
		IncidentID:     "6424cd115aa6f4be24f943bd4c10e776dbf4459c267fda69520dcd548831b5ed",
		ContextVersion: 1,
		ServiceID:      "payment",
		Environment:    "production",
		Severity:       "error",
		Summary:        "Settlement error rate rose to 10.8%.",
		LogExcerpt:     "ERROR settle.write pool exhausted",
		DetectedAt:     time.Date(2026, 8, 9, 0, 31, 12, 0, time.UTC),
	}
}

// ─── tests ──────────────────────────────────────────────────────────────────

func TestHandleHappyPath(t *testing.T) {
	q := &fakeQueue{}
	a := &fakeAgent{result: agent.Result{Answer: "ROLLBACK 7e91d04", Iterations: 2, Sources: 4}}
	w := newWorker(q, fakeResolver{ctx: resolvedContext()}, a)

	w.handle(context.Background(), message(sampleBody, 1))

	if a.callCount() != 1 {
		t.Fatalf("agent ran %d times, want 1", a.callCount())
	}
	if q.deletedCount() != 1 {
		t.Errorf("message deleted %d times, want 1", q.deletedCount())
	}

	rec, ok := w.Results.Get(sampleInvestigation)
	if !ok {
		t.Fatal("no record for the investigation")
	}
	if rec.Status != StatusDone {
		t.Errorf("status = %q, want %q", rec.Status, StatusDone)
	}
	if rec.Result.Answer != "ROLLBACK 7e91d04" {
		t.Errorf("answer = %q", rec.Result.Answer)
	}
	if rec.FinishedAt == nil {
		t.Error("finished_at is not set on a completed record")
	}

	// the question is what the incident row said, not the bare envelope
	if q := a.questions[0]; !strings.Contains(q, "Settlement error rate") || !strings.Contains(q, "pool exhausted") {
		t.Errorf("question does not carry the incident context:\n%s", q)
	}
}

func TestHandleMissingContextLeavesMessage(t *testing.T) {
	q := &fakeQueue{}
	a := &fakeAgent{}
	w := newWorker(q, fakeResolver{err: incident.ErrNotFound}, a)

	w.handle(context.Background(), message(sampleBody, 1))

	// the producer may not have committed the context row yet: the message has
	// to come back, and no tokens may be spent in the meantime
	if a.callCount() != 0 {
		t.Errorf("agent ran %d times, want 0", a.callCount())
	}
	if q.deletedCount() != 0 {
		t.Errorf("message was deleted %d times, want 0", q.deletedCount())
	}

	rec, ok := w.Results.Get(sampleInvestigation)
	if !ok {
		t.Fatal("expected a retrying record explaining the wait")
	}
	if rec.Status != StatusRetrying {
		t.Errorf("status = %q, want %q", rec.Status, StatusRetrying)
	}
	// and a retrying record must not suppress the redelivery
	if w.Results.Seen(sampleInvestigation) {
		t.Error("Seen() is true for a retrying investigation; the redelivery would be dropped")
	}
}

func TestHandleMalformedBodyIsDropped(t *testing.T) {
	q := &fakeQueue{}
	a := &fakeAgent{}
	w := newWorker(q, fakeResolver{ctx: resolvedContext()}, a)

	w.handle(context.Background(), message("not json", 1))

	if a.callCount() != 0 {
		t.Errorf("agent ran %d times, want 0", a.callCount())
	}
	// retrying cannot fix it, so it must not be left to cycle into a DLQ
	if q.deletedCount() != 1 {
		t.Errorf("message deleted %d times, want 1", q.deletedCount())
	}
}

func TestHandleDuplicateIsNotReinvestigated(t *testing.T) {
	q := &fakeQueue{}
	a := &fakeAgent{result: agent.Result{Answer: "ROLLBACK 7e91d04"}}
	w := newWorker(q, fakeResolver{ctx: resolvedContext()}, a)

	w.handle(context.Background(), message(sampleBody, 1))
	w.handle(context.Background(), message(sampleBody, 2))

	if a.callCount() != 1 {
		t.Errorf("agent ran %d times for the same investigation, want 1", a.callCount())
	}
	if q.deletedCount() != 2 {
		t.Errorf("deleted %d messages, want both acknowledged", q.deletedCount())
	}
}

func TestHandleGivesUpAtMaxAttempts(t *testing.T) {
	q := &fakeQueue{}
	a := &fakeAgent{}
	w := newWorker(q, fakeResolver{ctx: resolvedContext()}, a)

	w.handle(context.Background(), message(sampleBody, 5))

	if a.callCount() != 0 {
		t.Errorf("agent ran %d times on the give-up delivery, want 0", a.callCount())
	}
	if q.deletedCount() != 1 {
		t.Errorf("message deleted %d times, want 1", q.deletedCount())
	}

	rec, _ := w.Results.Get(sampleInvestigation)
	if rec.Status != StatusFailed {
		t.Errorf("status = %q, want %q", rec.Status, StatusFailed)
	}
	if !strings.Contains(rec.Error, "5 deliveries") {
		t.Errorf("error does not say why it stopped: %q", rec.Error)
	}
}

func TestHandleExtendsVisibilityDuringLongRun(t *testing.T) {
	q := &fakeQueue{}
	// visibility is 90ms in the test config, so heartbeats fire every 30ms
	a := &fakeAgent{delay: 110 * time.Millisecond, result: agent.Result{Answer: "HOTFIX"}}
	w := newWorker(q, fakeResolver{ctx: resolvedContext()}, a)

	w.handle(context.Background(), message(sampleBody, 1))

	if got := q.extendedCount(); got < 2 {
		t.Errorf("visibility extended %d times during a run 1.2x longer than the timeout, want at least 2", got)
	}
	if q.deletedCount() != 1 {
		t.Errorf("message deleted %d times, want 1", q.deletedCount())
	}
}

func TestHandleTransportErrorLeavesMessage(t *testing.T) {
	q := &fakeQueue{}
	a := &fakeAgent{
		err:    fmt.Errorf("%w: connection closed", agent.ErrTransport),
		result: agent.Result{Iterations: 1},
	}
	w := newWorker(q, fakeResolver{ctx: resolvedContext()}, a)

	w.handle(context.Background(), message(sampleBody, 1))

	// MCP being unreachable is the one run failure worth another delivery
	if q.deletedCount() != 0 {
		t.Errorf("message deleted %d times, want 0", q.deletedCount())
	}
	rec, _ := w.Results.Get(sampleInvestigation)
	if rec.Status != StatusRetrying {
		t.Errorf("status = %q, want %q", rec.Status, StatusRetrying)
	}
}

func TestHandleModelErrorIsTerminal(t *testing.T) {
	q := &fakeQueue{}
	a := &fakeAgent{
		err:    errors.New("agent: generate: 402 insufficient credit"),
		result: agent.Result{Iterations: 1},
	}
	w := newWorker(q, fakeResolver{ctx: resolvedContext()}, a)

	w.handle(context.Background(), message(sampleBody, 1))

	// four more identical failures would cost four more calls and tell us nothing
	if q.deletedCount() != 1 {
		t.Errorf("message deleted %d times, want 1", q.deletedCount())
	}
	rec, _ := w.Results.Get(sampleInvestigation)
	if rec.Status != StatusFailed {
		t.Errorf("status = %q, want %q", rec.Status, StatusFailed)
	}
	if !strings.Contains(rec.Error, "insufficient credit") {
		t.Errorf("error = %q, want the model's failure", rec.Error)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	q := &fakeQueue{}
	w := newWorker(q, fakeResolver{ctx: resolvedContext()}, &fakeAgent{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

func TestRunSurvivesReceiveErrors(t *testing.T) {
	// a queue that is unreachable must not take the process down with it
	q := &fakeQueue{receiveErr: errors.New("dial tcp 127.0.0.1:4566: connection refused")}
	w := newWorker(q, fakeResolver{ctx: resolvedContext()}, &fakeAgent{})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return; a receive error should back off, not block")
	}
}

func TestStoreEviction(t *testing.T) {
	s := NewStore()
	s.capacity = 2

	for i := range 3 {
		s.Fail(queue.Assignment{InvestigationID: fmt.Sprint(i)}, "nope")
	}

	if _, ok := s.Get("0"); ok {
		t.Error("the oldest record was not evicted")
	}
	for _, id := range []string{"1", "2"} {
		if _, ok := s.Get(id); !ok {
			t.Errorf("record %s is missing", id)
		}
	}
}
