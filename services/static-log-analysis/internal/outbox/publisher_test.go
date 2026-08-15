package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/agent"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/outbox"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/queue"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/queuetest"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/testids"
)

const (
	testRegion   = "us-east-1"
	testTenant   = "tenant-a"
	testOwner    = "publisher-a"
	otherRegion  = "eu-central-1"
	testIncident = "1111111111111111111111111111111111111111111111111111111111111111"
)

// scriptedStore hands out the claims a test prepared and records the durable
// transition the publisher chose for each one.
//
// It deliberately models neither claim expiry nor fencing. Those belong to
// CockroachDB and are already pinned there; a test that asserted them through
// this type would pass whatever the publisher did. What it pins here is which
// transition the publisher chooses, and with what arguments.
type scriptedStore struct {
	mu        sync.Mutex
	claimable []persistence.OutboxClaim
	claimErr  error
	markErr   func(persistence.OutboxClaim) error
	retryErr  error

	claimCalls int
	marks      []markCall
	retries    []retryCall
}

type markCall struct {
	messageID   string
	token       string
	publishedAt time.Time
}

type retryCall struct {
	messageID     string
	token         string
	failure       persistence.OutboxFailure
	nextAttemptAt time.Time
}

func (s *scriptedStore) ClaimOutbox(_ context.Context, scope persistence.Scope, owner string, tokens []string, now time.Time, ttl time.Duration) ([]persistence.OutboxClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimCalls++
	if s.claimErr != nil {
		return nil, s.claimErr
	}
	size := len(tokens)
	if size > len(s.claimable) {
		size = len(s.claimable)
	}
	claims := make([]persistence.OutboxClaim, 0, size)
	for i := 0; i < size; i++ {
		claim := s.claimable[i]
		claim.Scope = scope
		claim.Owner = owner
		claim.Token = tokens[i]
		claim.ExpiresAt = now.Add(ttl)
		claims = append(claims, claim)
	}
	s.claimable = s.claimable[size:]
	return claims, nil
}

func (s *scriptedStore) MarkOutboxPublished(_ context.Context, claim persistence.OutboxClaim, publishedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markErr != nil {
		if err := s.markErr(claim); err != nil {
			return err
		}
	}
	s.marks = append(s.marks, markCall{messageID: claim.Message.MessageID, token: claim.Token, publishedAt: publishedAt})
	return nil
}

func (s *scriptedStore) RetryOutbox(_ context.Context, claim persistence.OutboxClaim, failure persistence.OutboxFailure, _, nextAttemptAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retryErr != nil {
		return s.retryErr
	}
	s.retries = append(s.retries, retryCall{messageID: claim.Message.MessageID, token: claim.Token,
		failure: failure, nextAttemptAt: nextAttemptAt})
	return nil
}

func (s *scriptedStore) transitions() ([]markCall, []retryCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]markCall(nil), s.marks...), append([]retryCall(nil), s.retries...)
}

// assignmentClaim builds a claim exactly as the store hands one out: a real,
// catalogue-valid assignment for this region.
func assignmentClaim(t *testing.T, source *testids.Source, attempt int64, options ...func(*agent.Assignment, *queue.Message)) persistence.OutboxClaim {
	t.Helper()
	messageID, investigationID := mustID(t, source), mustID(t, source)
	assignment := agent.Assignment{
		SchemaVersion: agent.AssignmentSchemaVersion, MessageID: messageID, MessageType: agent.AssignmentMessageType,
		CreatedAt: fakeclock.Origin, Region: testRegion, TenantID: testTenant, Classification: "SENSITIVE",
		Producer: agent.AssignmentProducer, CorrelationID: investigationID, IncidentID: testIncident,
		IncidentGeneration: 1, InvestigationID: investigationID, ServiceID: "paymentservice",
		Environment: "production", Severity: "error", ContextVersion: 1,
	}
	message := queue.Message{
		MessageID: messageID, DeduplicationKey: "assignment:" + investigationID,
		Type: agent.AssignmentMessageType, Attributes: map[string]string{"region": testRegion},
	}
	for _, option := range options {
		option(&assignment, &message)
	}
	body, err := agent.EncodeAssignment(assignment)
	if err != nil {
		t.Fatalf("encode assignment: %v", err)
	}
	message.Body = body
	return persistence.OutboxClaim{
		Scope:   persistence.Scope{Region: testRegion, TenantID: testTenant},
		Message: message, Attempt: attempt,
	}
}

func mustID(t *testing.T, source *testids.Source) string {
	t.Helper()
	id, err := source.New()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

type publisherFixture struct {
	publisher *outbox.Publisher
	store     *scriptedStore
	transport *queuetest.Transport
	clock     *fakeclock.Clock
}

func newFixture(t *testing.T, claims ...persistence.OutboxClaim) *publisherFixture {
	t.Helper()
	return newFixtureWith(t, func(*outbox.Config) {}, claims...)
}

func newFixtureWith(t *testing.T, adjust func(*outbox.Config), claims ...persistence.OutboxClaim) *publisherFixture {
	t.Helper()
	store := &scriptedStore{claimable: claims}
	transport := queuetest.New(testRegion)
	clk := fakeclock.NewAtOrigin()
	config := outbox.Config{
		Scope: persistence.Scope{Region: testRegion, TenantID: testTenant}, Owner: testOwner,
		Store: store, Transport: transport, Clock: clk, IDs: testids.New(testids.WithSeed(3)),
		Batch: 10, ClaimTTL: 30 * time.Second, PublishTimeout: 5 * time.Second,
		BackoffMin: time.Second, BackoffMax: time.Minute, MaxAttempts: 3,
		// A fixed jitter keeps the schedule exact while still proving the
		// publisher adds one; the production source is entropy-driven.
		Jitter: func(d time.Duration) time.Duration { return d / 4 },
	}
	adjust(&config)
	publisher, err := outbox.New(config)
	if err != nil {
		t.Fatalf("new publisher: %v", err)
	}
	return &publisherFixture{publisher: publisher, store: store, transport: transport, clock: clk}
}

func TestACleanPublishDeliversTheMessageAndRecordsItPublishedExactlyOnce(t *testing.T) {
	source := testids.New()
	claim := assignmentClaim(t, source, 1)
	fixture := newFixture(t, claim)

	result, err := fixture.publisher.PublishCycle(context.Background())
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if result.Claimed != 1 || result.Published != 1 || result.Retried != 0 || result.DeadLettered != 0 || result.Unrecorded != 0 {
		t.Fatalf("result=%+v", result)
	}
	delivered := fixture.transport.Delivered()
	if len(delivered) != 1 || delivered[0].MessageID != claim.Message.MessageID ||
		delivered[0].DeduplicationKey != claim.Message.DeduplicationKey ||
		string(delivered[0].Body) != string(claim.Message.Body) {
		t.Fatalf("delivered %+v, want the claimed message unchanged", delivered)
	}
	marks, retries := fixture.store.transitions()
	if len(marks) != 1 || marks[0].messageID != claim.Message.MessageID || len(retries) != 0 {
		t.Fatalf("marks=%+v retries=%+v", marks, retries)
	}
}

// operations.md: "SQS unavailable -> committed outbox messages remain pending".
// The message must keep its place with a backed-off next attempt, and must
// never be recorded as published by a publisher that never delivered it.
func TestARetryableFailureLeavesTheMessagePendingWithBackoffAndNeverMarksItPublished(t *testing.T) {
	source := testids.New()
	claim := assignmentClaim(t, source, 1)
	fixture := newFixture(t, claim)
	fixture.transport.PublishErr = func(queue.Message) error {
		return outbox.NewFailure(outbox.ErrRetryable, outbox.ReasonQueueUnavailable, "ServiceUnavailable")
	}

	result, err := fixture.publisher.PublishCycle(context.Background())
	if err != nil {
		t.Fatalf("a retryable failure ended the cycle: %v", err)
	}
	if result.Retried != 1 || result.Published != 0 || result.DeadLettered != 0 {
		t.Fatalf("result=%+v", result)
	}
	if delivered := fixture.transport.Delivered(); len(delivered) != 0 {
		t.Fatalf("a failed publish was recorded as delivered: %+v", delivered)
	}
	marks, retries := fixture.store.transitions()
	if len(marks) != 0 {
		t.Fatalf("an undelivered message was marked published: %+v", marks)
	}
	if len(retries) != 1 || retries[0].failure != persistence.OutboxFailureQueueUnavailable {
		t.Fatalf("retries=%+v", retries)
	}
	// First attempt, so the backoff is the configured minimum plus this test's
	// fixed jitter. Without jitter every publisher in the region would return at
	// the same instant.
	want := fixture.clock.Now().Add(time.Second + time.Second/4)
	if !retries[0].nextAttemptAt.Equal(want) {
		t.Fatalf("next attempt at %s, want %s", retries[0].nextAttemptAt, want)
	}
}

// The backoff must widen with each failed attempt and must stop at the ceiling,
// so a queue that is down for an hour is retried for an hour without being
// hammered and without a message drifting into an unbounded future.
func TestBackoffWidensWithEachAttemptAndStopsAtTheConfiguredCeiling(t *testing.T) {
	fixture := newFixtureWith(t, func(config *outbox.Config) {
		config.BackoffMin, config.BackoffMax, config.MaxAttempts = time.Second, 8*time.Second, 100
		config.Jitter = func(time.Duration) time.Duration { return 0 }
	})
	want := []time.Duration{time.Second, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
	for attempt, expected := range want {
		if got := fixture.publisher.Backoff(int64(attempt)); got != expected {
			t.Fatalf("attempt %d backed off %s, want %s", attempt, got, expected)
		}
	}
}

// A message the queue will never accept must not keep its place in front of
// every message committed after it.
func TestAPermanentlyRejectedMessageIsDeadLetteredWithoutBlockingItsSiblings(t *testing.T) {
	source := testids.New()
	poisoned := assignmentClaim(t, source, 1)
	sibling := assignmentClaim(t, source, 1)
	fixture := newFixture(t, poisoned, sibling)
	fixture.transport.PublishErr = func(message queue.Message) error {
		if message.MessageID != poisoned.Message.MessageID {
			return nil
		}
		return outbox.NewFailure(outbox.ErrMessageRejected, outbox.ReasonRejected, "InvalidMessageContents")
	}

	result, err := fixture.publisher.PublishCycle(context.Background())
	if err != nil {
		t.Fatalf("one poisoned message failed the whole cycle: %v", err)
	}
	if result.Claimed != 2 || result.DeadLettered != 1 || result.Published != 1 || result.Retried != 0 {
		t.Fatalf("result=%+v", result)
	}
	if len(fixture.transport.DeadLetters()) != 1 ||
		fixture.transport.DeadLetters()[0].Message.MessageID != poisoned.Message.MessageID ||
		fixture.transport.DeadLetters()[0].Reason != outbox.ReasonRejected {
		t.Fatalf("dead lettered %+v", fixture.transport.DeadLetters())
	}
	if string(fixture.transport.DeadLetters()[0].Message.Body) != string(poisoned.Message.Body) {
		t.Fatal("the dead-lettered body is not what was committed, so a replay would replay something else")
	}
	delivered := fixture.transport.Delivered()
	if len(delivered) != 1 || delivered[0].MessageID != sibling.Message.MessageID {
		t.Fatalf("the sibling was starved by the poisoned message: %+v", delivered)
	}
	marks, retries := fixture.store.transitions()
	if len(marks) != 2 || len(retries) != 0 {
		t.Fatalf("marks=%+v retries=%+v; a dead-lettered message must leave the pending set", marks, retries)
	}
}

// Retrying forever is how one message starves a queue. Once a message has
// failed on its whole schedule it goes to the dead-letter queue, which retains
// it, rather than being retried again or dropped.
func TestAMessageThatFailsOnItsWholeScheduleIsDeadLetteredRatherThanRetriedForever(t *testing.T) {
	source := testids.New()
	// MaxAttempts is 3 in the fixture, so this delivery is the last one.
	claim := assignmentClaim(t, source, 3)
	fixture := newFixture(t, claim)
	fixture.transport.PublishErr = func(queue.Message) error {
		return outbox.NewFailure(outbox.ErrRetryable, outbox.ReasonQueueUnavailable, "ServiceUnavailable")
	}

	result, err := fixture.publisher.PublishCycle(context.Background())
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if result.DeadLettered != 1 || result.Retried != 0 || result.Published != 0 {
		t.Fatalf("result=%+v", result)
	}
	if len(fixture.transport.DeadLetters()) != 1 || fixture.transport.DeadLetters()[0].Reason != outbox.ReasonAttemptsExhausted {
		t.Fatalf("dead lettered %+v", fixture.transport.DeadLetters())
	}
}

// The attempt counter counts claims, not failed deliveries: a claim released
// unattempted has already incremented it. Escalating on the counter alone would
// let a repeated deployment fault drain committed assignments into the
// dead-letter queue without any of them ever being offered to the queue.
func TestAMessageWithASpentScheduleIsStillDeliveredWhenTheQueueAcceptsIt(t *testing.T) {
	source := testids.New()
	claim := assignmentClaim(t, source, 9)
	fixture := newFixture(t, claim)

	result, err := fixture.publisher.PublishCycle(context.Background())
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if result.Published != 1 || result.DeadLettered != 0 {
		t.Fatalf("result=%+v", result)
	}
	if len(fixture.transport.DeadLetters()) != 0 {
		t.Fatal("a message the queue accepted was dead-lettered on its attempt count alone")
	}
}

// A claim that would expire while a message was in flight is worse than no
// claim: a successor takes the row, both publishers deliver, and the first one's
// mark is refused. The cycle stops and hands the rest of the batch back.
func TestACycleStopsBeforeALeaseCouldExpireMidPublishAndReleasesTheRest(t *testing.T) {
	source := testids.New()
	first, second := assignmentClaim(t, source, 1), assignmentClaim(t, source, 1)
	fixture := newFixtureWith(t, func(config *outbox.Config) {
		// One publish attempt may take longer than the whole lease.
		config.ClaimTTL, config.PublishTimeout = 5*time.Second, 10*time.Second
	}, first, second)

	result, err := fixture.publisher.PublishCycle(context.Background())
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if len(fixture.transport.Delivered()) != 0 {
		t.Fatal("a message was published under a lease that could not cover it")
	}
	if result.Retried != 2 || result.Published != 0 {
		t.Fatalf("result=%+v", result)
	}
	_, retries := fixture.store.transitions()
	if len(retries) != 2 {
		t.Fatalf("retries=%+v", retries)
	}
	for _, retry := range retries {
		// Nothing about these messages failed, so they are claimable at once
		// rather than after a backoff.
		if !retry.nextAttemptAt.Equal(fixture.clock.Now()) {
			t.Fatalf("a released claim was delayed until %s", retry.nextAttemptAt)
		}
	}
}

// The dead-letter queue is a queue too. A send that fails there must not be
// mistaken for a delivery, or a committed assignment would be marked published
// with nothing anywhere holding it.
func TestADeadLetterSendThatFailsKeepsTheMessageInsteadOfRecordingItDelivered(t *testing.T) {
	source := testids.New()
	claim := assignmentClaim(t, source, 3)
	fixture := newFixture(t, claim)
	fixture.transport.PublishErr = func(queue.Message) error {
		return outbox.NewFailure(outbox.ErrRetryable, outbox.ReasonQueueUnavailable, "ServiceUnavailable")
	}
	fixture.transport.DeadLetterErr = func(queue.Message) error {
		return outbox.NewFailure(outbox.ErrRetryable, outbox.ReasonQueueUnavailable, "ServiceUnavailable")
	}

	result, err := fixture.publisher.PublishCycle(context.Background())
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if result.DeadLettered != 0 || result.Retried != 1 {
		t.Fatalf("result=%+v", result)
	}
	marks, retries := fixture.store.transitions()
	if len(marks) != 0 {
		t.Fatalf("a message nothing received was marked published: %+v", marks)
	}
	if len(retries) != 1 || retries[0].messageID != claim.Message.MessageID {
		t.Fatalf("retries=%+v", retries)
	}
}

// A message neither queue will carry has nowhere to go. It must not be marked
// published, because nothing received it, and it must not be re-offered every
// cycle, because that is the starvation the dead-letter path exists to prevent.
// It waits out the whole ceiling in the outbox, where retention keeps it.
func TestAMessageNeitherQueueWillCarryWaitsTheCeilingRatherThanEveryCycle(t *testing.T) {
	source := testids.New()
	claim := assignmentClaim(t, source, 3)
	fixture := newFixture(t, claim)
	fixture.transport.PublishErr = func(queue.Message) error {
		return outbox.NewFailure(outbox.ErrMessageRejected, outbox.ReasonRejected, "InvalidMessageContents")
	}
	fixture.transport.DeadLetterErr = func(queue.Message) error {
		return outbox.NewFailure(outbox.ErrMessageRejected, outbox.ReasonRejected, "InvalidMessageContents")
	}

	result, err := fixture.publisher.PublishCycle(context.Background())
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if result.DeadLettered != 0 || result.Retried != 1 {
		t.Fatalf("result=%+v", result)
	}
	marks, retries := fixture.store.transitions()
	if len(marks) != 0 {
		t.Fatalf("a message nothing received was marked published: %+v", marks)
	}
	want := fixture.clock.Now().Add(time.Minute)
	if len(retries) != 1 || !retries[0].nextAttemptAt.Equal(want) {
		t.Fatalf("retries=%+v, want one attempt at the ceiling %s", retries, want)
	}
}

// ADR 0004: "A lost database update may republish the identical message_id,
// deduplication key, and payload". The publisher must not convert that into a
// failure: recording a retry or a dead letter for a message the queue already
// accepted would misreport what happened and, for the dead-letter case, deliver
// it twice to two different queues.
func TestAPublishAcknowledgedButNotRecordedIsNeitherRetriedNorDeadLettered(t *testing.T) {
	source := testids.New()
	claim := assignmentClaim(t, source, 1)
	fixture := newFixture(t, claim)
	fixture.store.markErr = func(persistence.OutboxClaim) error { return persistence.ErrUnavailable }

	result, err := fixture.publisher.PublishCycle(context.Background())
	if err != nil {
		t.Fatalf("a lost acknowledgement failed the cycle: %v", err)
	}
	if result.Unrecorded != 1 || result.Published != 0 || result.Retried != 0 || result.DeadLettered != 0 {
		t.Fatalf("result=%+v", result)
	}
	if len(fixture.transport.Delivered()) != 1 {
		t.Fatal("the message was never delivered, so this is not the case under test")
	}
	if len(fixture.transport.DeadLetters()) != 0 {
		t.Fatal("an acknowledged message was also sent to the dead-letter queue")
	}
	_, retries := fixture.store.transitions()
	if len(retries) != 0 {
		t.Fatalf("an acknowledged message was recorded as failed: %+v", retries)
	}
}

// A deployment fault is not record-local. Dead-lettering each message in turn
// would drain a whole region's committed assignments into the dead-letter queue
// because one credential was revoked or one queue URL was typed wrong.
func TestADeploymentFaultReleasesEveryClaimAndStopsTheCycle(t *testing.T) {
	source := testids.New()
	first, second, third := assignmentClaim(t, source, 1), assignmentClaim(t, source, 1), assignmentClaim(t, source, 1)
	fixture := newFixture(t, first, second, third)
	fixture.transport.PublishErr = func(queue.Message) error {
		return outbox.NewFailure(outbox.ErrDeploymentFault, outbox.ReasonDeploymentFault, "AccessDenied")
	}

	result, err := fixture.publisher.PublishCycle(context.Background())
	if !errors.Is(err, outbox.ErrDeploymentFault) {
		t.Fatalf("a revoked credential was not reported as a deployment fault: %v", err)
	}
	if len(fixture.transport.DeadLetters()) != 0 {
		t.Fatalf("a deployment fault drained %d committed assignments to the dead-letter queue", len(fixture.transport.DeadLetters()))
	}
	if result.Published != 0 {
		t.Fatalf("result=%+v", result)
	}
	marks, retries := fixture.store.transitions()
	if len(marks) != 0 {
		t.Fatalf("marks=%+v", marks)
	}
	// The message that was attempted and both messages that were not are all
	// pending again, so a repaired deployment finds them immediately.
	if len(retries) != 3 {
		t.Fatalf("%d of 3 claims were released: %+v", len(retries), retries)
	}
	for _, retry := range retries {
		if retry.failure != persistence.OutboxFailureQueueUnavailable {
			t.Fatalf("a released claim recorded %q", retry.failure)
		}
	}
}

// security.md and architecture.md forbid regional movement of log-derived
// content. A message that does not belong in this region is never published and
// never dead-lettered, because a dead-letter queue is a queue in some region
// too. It is a deployment fault an operator must see.
func TestAMessageScopedToAnotherRegionIsNeverPublishedAndNeverDeadLettered(t *testing.T) {
	cases := map[string]func(*agent.Assignment, *queue.Message){
		"routing attribute names another region": func(_ *agent.Assignment, message *queue.Message) {
			message.Attributes["region"] = otherRegion
		},
		"payload names another region": func(assignment *agent.Assignment, _ *queue.Message) {
			assignment.Region = otherRegion
		},
		"payload names another tenant": func(assignment *agent.Assignment, _ *queue.Message) {
			assignment.TenantID = "tenant-b"
		},
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			source := testids.New()
			claim := assignmentClaim(t, source, 1, corrupt)
			fixture := newFixture(t, claim)

			_, err := fixture.publisher.PublishCycle(context.Background())
			if !errors.Is(err, outbox.ErrDeploymentFault) {
				t.Fatalf("a cross-region message was not a categorical failure: %v", err)
			}
			if len(fixture.transport.Delivered()) != 0 {
				t.Fatal("a message from another region was published into this region's queue")
			}
			if len(fixture.transport.DeadLetters()) != 0 {
				t.Fatal("a message from another region was moved into this region's dead-letter queue")
			}
			marks, retries := fixture.store.transitions()
			if len(marks) != 0 || len(retries) != 1 {
				t.Fatalf("marks=%+v retries=%+v", marks, retries)
			}
		})
	}
}

// A misconfigured replica must be caught before it publishes anything, not once
// per message.
func TestNewRefusesAQueueInAnotherRegion(t *testing.T) {
	_, err := outbox.New(outbox.Config{
		Scope: persistence.Scope{Region: testRegion, TenantID: testTenant}, Owner: testOwner,
		Store: &scriptedStore{}, Transport: queuetest.New(otherRegion),
		Clock: fakeclock.NewAtOrigin(), IDs: testids.New(), Batch: 1,
		ClaimTTL: time.Second, PublishTimeout: time.Second, BackoffMin: time.Second, BackoffMax: time.Second,
	})
	if !errors.Is(err, outbox.ErrInvalidConfig) {
		t.Fatalf("a publisher wired to another region's queue started: %v", err)
	}
	if !strings.Contains(err.Error(), otherRegion) || !strings.Contains(err.Error(), testRegion) {
		t.Fatalf("the error does not name both regions an operator must reconcile: %v", err)
	}
}

// An unrecognized failure is a defect in a transport, not a verdict about a
// committed assignment. Calling it permanent would dead-letter real work on the
// strength of a bug.
func TestAnUnrecognizedTransportFailureIsRetriedRatherThanDeadLettered(t *testing.T) {
	source := testids.New()
	claim := assignmentClaim(t, source, 1)
	fixture := newFixture(t, claim)
	fixture.transport.PublishErr = func(queue.Message) error { return fmt.Errorf("something nobody classified") }

	result, err := fixture.publisher.PublishCycle(context.Background())
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if result.Retried != 1 || result.DeadLettered != 0 {
		t.Fatalf("result=%+v", result)
	}
}

// A cycle cut off by the shutdown budget stops offering messages rather than
// publishing under a context that is about to be torn down. The claims it never
// attempted lapse and are claimed again, which is the same safety a forced stop
// of the process worker has.
func TestACancelledCycleStopsDeliveringAndLeavesTheRestOfTheBatchAlone(t *testing.T) {
	source := testids.New()
	first, second, third := assignmentClaim(t, source, 1), assignmentClaim(t, source, 1), assignmentClaim(t, source, 1)
	fixture := newFixture(t, first, second, third)
	ctx, cancel := context.WithCancel(context.Background())
	fixture.transport.BeforeSend = func(message queue.Message) {
		if message.MessageID == first.Message.MessageID {
			cancel()
		}
	}
	defer cancel()

	result, err := fixture.publisher.PublishCycle(ctx)
	if err != nil {
		t.Fatalf("a cancelled cycle reported a failure: %v", err)
	}
	// The first message was already in flight when the cancellation arrived, so
	// it is either delivered or not; what must not happen is the rest of the
	// batch being delivered afterwards.
	if delivered := len(fixture.transport.Delivered()); delivered > 1 {
		t.Fatalf("%d messages were delivered after the cycle was cancelled", delivered)
	}
	if result.Published+result.Unrecorded > 1 {
		t.Fatalf("result=%+v", result)
	}
	if len(fixture.transport.DeadLetters()) != 0 {
		t.Fatal("a cancelled cycle dead-lettered a message")
	}
}

func TestPublishCycleReportsAClaimFailureWithoutTouchingTheQueue(t *testing.T) {
	fixture := newFixture(t)
	fixture.store.claimErr = persistence.ErrUnavailable

	if _, err := fixture.publisher.PublishCycle(context.Background()); !errors.Is(err, persistence.ErrUnavailable) {
		t.Fatalf("cycle: %v", err)
	}
	if len(fixture.transport.Delivered()) != 0 {
		t.Fatal("a publisher that could not claim anything published something")
	}
}
