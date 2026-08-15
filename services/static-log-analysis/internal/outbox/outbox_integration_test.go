//go:build integration

package outbox_test

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/admission"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/agent"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/journal"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/outbox"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/pipeline"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/queue"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/crdbtest"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/localstacktest"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/otlpgen"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/queuetest"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/testids"
)

// These tests run against a real CockroachDB because the properties they are
// about are the database's: a claim that lapses, a fencing token that refuses a
// stale mark, a row that must still be pending after a failed publish. A fake
// store would agree with whatever the publisher did.
//
// The transport is a controllable double, because the failures under test -
// SQS refusing a request, an acknowledgement that never reaches the database -
// are not failures a real endpoint can be asked for on demand.

const (
	tenantID       = "tenant-a"
	classification = "SENSITIVE"
	publisherOwner = "publisher-a"
)

// outboxFixture is one committed vertical slice: a real journal, a real
// CockroachDB, and however many investigations the test asked to be committed.
type outboxFixture struct {
	store *persistence.Store
	pool  *pgxpool.Pool
	clock *fakeclock.Clock
}

// commitAssignments runs the real ingest-and-process path until services
// distinct investigations have been committed, each with its outbox row.
func commitAssignments(t *testing.T, services ...string) *outboxFixture {
	t.Helper()
	ctx := context.Background()
	pool := crdbtest.Pool(t)
	if err := persistence.ApplyMigrations(ctx, pool, persistence.TopologySingleRegion); err != nil {
		t.Fatal(err)
	}
	scope := persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID}
	store, err := persistence.New(pool, persistence.Config{Validator: redact.MinimalPolicy(), Scope: scope,
		Classification: classification, Topology: persistence.TopologySingleRegion})
	if err != nil {
		t.Fatal(err)
	}
	clk := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j, err := journal.Open(journal.Config{Dir: filepath.Join(t.TempDir(), "journal"), Owner: "outbox-integration",
		TenantID: tenantID, Region: otlpgen.DefaultRegion, Classification: classification, Clock: clk,
		Validator: policy, MaxBytes: 64 << 20, MinFreeBytes: 1,
		FreeSpace: func(string) (uint64, error) { return math.MaxUint64, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	service, err := pipeline.New(pipeline.Config{Clock: clk, IDs: testids.New(testids.WithClock(clk)),
		Journal: j, Store: store, Policy: policy, Scope: scope, Classification: classification})
	if err != nil {
		t.Fatal(err)
	}

	envelope := model.TrustedEnvelope{SourceType: model.SourceTypeOTLP, SourceAccount: "aws-account-a",
		Region: otlpgen.DefaultRegion, AllowedEnvironments: []string{otlpgen.DefaultEnvironment},
		AllowedServices: services, SourceInstance: "collector-a", CredentialIdentity: "workload-a"}
	for index, name := range services {
		producer := otlpgen.New(otlpgen.WithClock(clk), otlpgen.WithService(name),
			otlpgen.WithIDs(testids.New(testids.WithClock(clk), testids.WithSeed(uint64(index+1)))))
		// Five errors inside five minutes is the deterministic rule that elects
		// an investigation, which is what writes the outbox row.
		for i := 0; i < 5; i++ {
			record := producer.PaymentError(otlpgen.WithBody("payment declined"),
				otlpgen.AtEventTime(uint64(fakeclock.Origin.Add(time.Duration(i)*time.Second).UnixNano())))
			payload, err := otlpgen.Encode(producer.Request(record))
			if err != nil {
				t.Fatal(err)
			}
			envelope.ReceivedAt = clk.Now()
			if result, err := service.Ingest(ctx, envelope, payload, admission.EncodingIdentity); err != nil || !result.Acknowledged {
				t.Fatalf("ingest %s/%d: result=%+v err=%v", name, i, result, err)
			}
		}
	}
	clk.Advance(2*time.Minute + 5*time.Second)
	result, err := service.Process(ctx, 100)
	if err != nil || result.Investigations != len(services) {
		t.Fatalf("process: result=%+v err=%v", result, err)
	}
	return &outboxFixture{store: store, pool: pool, clock: clk}
}

func (f *outboxFixture) publisher(t *testing.T, transport outbox.Transport, adjust ...func(*outbox.Config)) *outbox.Publisher {
	t.Helper()
	return f.publisherOwned(t, publisherOwner, transport, adjust...)
}

func (f *outboxFixture) publisherOwned(t *testing.T, owner string, transport outbox.Transport, adjust ...func(*outbox.Config)) *outbox.Publisher {
	t.Helper()
	config := outbox.Config{
		Scope: persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID}, Owner: owner,
		Store: f.store, Transport: transport, Clock: f.clock,
		IDs:    testids.New(testids.WithClock(f.clock), testids.WithSeed(97)),
		Logger: slog.New(slog.DiscardHandler),
		Batch:  10, ClaimTTL: 30 * time.Second, PublishTimeout: 5 * time.Second,
		BackoffMin: time.Second, BackoffMax: time.Minute, MaxAttempts: 3,
		Jitter: func(time.Duration) time.Duration { return 0 },
	}
	for _, apply := range adjust {
		apply(&config)
	}
	publisher, err := outbox.New(config)
	if err != nil {
		t.Fatal(err)
	}
	return publisher
}

type outboxRow struct {
	state         string
	attempts      int64
	nextAttemptAt time.Time
	published     *time.Time
	lastSafeError *string
	claimToken    *string
}

func (f *outboxFixture) rows(t *testing.T) map[string]outboxRow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT message_id,state,attempts,next_attempt_at,
		published_at,last_safe_error,claim_token FROM outbox_messages`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := make(map[string]outboxRow)
	for rows.Next() {
		var id string
		var row outboxRow
		if err := rows.Scan(&id, &row.state, &row.attempts, &row.nextAttemptAt, &row.published,
			&row.lastSafeError, &row.claimToken); err != nil {
			t.Fatal(err)
		}
		result[id] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func onlyRow(t *testing.T, rows map[string]outboxRow) (string, outboxRow) {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("%d outbox rows, want exactly one", len(rows))
	}
	for id, row := range rows {
		return id, row
	}
	return "", outboxRow{}
}

// testing.md resilience scenario 6, and the operations.md failure matrix row
// "SQS unavailable -> committed outbox messages remain pending". The committed
// investigation must survive the outage in the outbox and be delivered by a
// later cycle, with nothing marked published in between.
func TestSQSFailureAfterIncidentCommitLeavesTheAssignmentPendingUntilALaterCycleDeliversIt(t *testing.T) {
	fixture := commitAssignments(t, otlpgen.DefaultService)
	transport := queuetest.New(otlpgen.DefaultRegion)
	unavailable := true
	transport.PublishErr = func(queue.Message) error {
		if !unavailable {
			return nil
		}
		return outbox.NewFailure(outbox.ErrRetryable, outbox.ReasonQueueUnavailable, "ServiceUnavailable")
	}
	publisher := fixture.publisher(t, transport)

	result, err := publisher.PublishCycle(context.Background())
	if err != nil || result.Claimed != 1 || result.Retried != 1 || result.Published != 0 {
		t.Fatalf("outage cycle: result=%+v err=%v", result, err)
	}
	messageID, row := onlyRow(t, fixture.rows(t))
	if row.state != "pending" || row.published != nil {
		t.Fatalf("the committed assignment did not stay pending through the outage: %+v", row)
	}
	if row.lastSafeError == nil || *row.lastSafeError != string(persistence.OutboxFailureQueueUnavailable) {
		t.Fatalf("last_safe_error=%v", row.lastSafeError)
	}
	if !row.nextAttemptAt.After(fixture.clock.Now()) {
		t.Fatalf("the message is immediately claimable again, so the queue would be hammered: %s", row.nextAttemptAt)
	}

	// Before the backoff elapses the message is deliberately not claimable.
	if result, err := publisher.PublishCycle(context.Background()); err != nil || result.Claimed != 0 {
		t.Fatalf("a backed-off message was claimed early: result=%+v err=%v", result, err)
	}

	// The queue comes back.
	unavailable = false
	fixture.clock.Advance(2 * time.Second)
	result, err = publisher.PublishCycle(context.Background())
	if err != nil || result.Published != 1 {
		t.Fatalf("recovery cycle: result=%+v err=%v", result, err)
	}
	delivered := transport.Delivered()
	if len(delivered) != 1 || delivered[0].MessageID != messageID {
		t.Fatalf("delivered %+v", delivered)
	}
	_, row = onlyRow(t, fixture.rows(t))
	if row.state != "published" || row.published == nil || row.claimToken != nil {
		t.Fatalf("a delivered assignment was not recorded published: %+v", row)
	}
	if row.attempts != 2 {
		t.Fatalf("attempts=%d, want the failed attempt and the successful one", row.attempts)
	}
}

// ADR 0004 and the operations.md row "Publish acknowledged but DB update fails
// -> Outbox republishes; orchestrator deduplicates". The message must never be
// dropped, and every delivery must carry one identity so the duplicate is
// harmless.
func TestAPublishAcknowledgedButNotRecordedIsRepublishedUnderOneIdentity(t *testing.T) {
	fixture := commitAssignments(t, otlpgen.DefaultService)
	transport := queuetest.New(otlpgen.DefaultRegion)
	failing := &failMarkOnceStore{Store: fixture.store, fail: true}
	publisher := fixture.publisher(t, transport, func(config *outbox.Config) { config.Store = failing })

	result, err := publisher.PublishCycle(context.Background())
	if err != nil || result.Unrecorded != 1 || result.Published != 0 {
		t.Fatalf("lost acknowledgement cycle: result=%+v err=%v", result, err)
	}
	messageID, row := onlyRow(t, fixture.rows(t))
	if row.state != "claimed" || row.published != nil {
		t.Fatalf("a lost acknowledgement changed the row anyway: %+v", row)
	}

	// The claim lapses and the message is claimed again. Nothing about its
	// identity changed, which is what makes the second delivery a duplicate the
	// consumer absorbs rather than a second assignment.
	fixture.clock.Advance(31 * time.Second)
	result, err = publisher.PublishCycle(context.Background())
	if err != nil || result.Published != 1 {
		t.Fatalf("republish cycle: result=%+v err=%v", result, err)
	}
	delivered := transport.Delivered()
	if len(delivered) != 2 {
		t.Fatalf("%d deliveries, want the acknowledged one and its republish", len(delivered))
	}
	if delivered[0].MessageID != delivered[1].MessageID ||
		delivered[0].DeduplicationKey != delivered[1].DeduplicationKey ||
		string(delivered[0].Body) != string(delivered[1].Body) {
		t.Fatalf("republishing changed the message identity:\nfirst=%+v\nsecond=%+v", delivered[0], delivered[1])
	}
	if delivered[0].MessageID != messageID {
		t.Fatalf("delivered %q, committed %q", delivered[0].MessageID, messageID)
	}
	_, row = onlyRow(t, fixture.rows(t))
	if row.state != "published" {
		t.Fatalf("the republished message was never recorded: %+v", row)
	}
}

// testing.md resilience scenario 7 read from the producing side: the same
// assignment delivered several times, in whatever order, is one assignment.
// Repeated delivery is normal for SQS Standard, so the identity that makes it
// harmless has to be the same on every attempt.
func TestRepeatedDeliveryOfOneAssignmentCarriesOneDeduplicationKey(t *testing.T) {
	fixture := commitAssignments(t, otlpgen.DefaultService)
	transport := queuetest.New(otlpgen.DefaultRegion)
	always := &failMarkStore{Store: fixture.store}
	publisher := fixture.publisher(t, transport, func(config *outbox.Config) { config.Store = always })

	for attempt := 0; attempt < 3; attempt++ {
		if _, err := publisher.PublishCycle(context.Background()); err != nil {
			t.Fatalf("cycle %d: %v", attempt, err)
		}
		fixture.clock.Advance(31 * time.Second)
	}
	delivered := transport.Delivered()
	if len(delivered) != 3 {
		t.Fatalf("%d deliveries, want three", len(delivered))
	}
	for _, message := range delivered[1:] {
		if message.DeduplicationKey != delivered[0].DeduplicationKey || message.MessageID != delivered[0].MessageID ||
			string(message.Body) != string(delivered[0].Body) {
			t.Fatalf("a redelivery differs from the first: %+v vs %+v", message, delivered[0])
		}
	}
	if _, row := onlyRow(t, fixture.rows(t)); row.state == "published" {
		t.Fatal("the store recorded a publication none of these cycles could record")
	}
}

// A message the queue will never accept must leave the pending set through the
// dead-letter queue, and the assignment committed beside it must still be
// delivered in the same cycle.
func TestAPermanentlyRejectedAssignmentIsDeadLetteredWhileItsSiblingIsDelivered(t *testing.T) {
	fixture := commitAssignments(t, otlpgen.DefaultService, "cartservice")
	rows := fixture.rows(t)
	if len(rows) != 2 {
		t.Fatalf("%d committed assignments, want two", len(rows))
	}
	var poisoned string
	for id := range rows {
		poisoned = id
		break
	}
	transport := queuetest.New(otlpgen.DefaultRegion)
	transport.PublishErr = func(message queue.Message) error {
		if message.MessageID != poisoned {
			return nil
		}
		return outbox.NewFailure(outbox.ErrMessageRejected, outbox.ReasonRejected, "InvalidMessageContents")
	}
	publisher := fixture.publisher(t, transport)

	result, err := publisher.PublishCycle(context.Background())
	if err != nil || result.Claimed != 2 || result.Published != 1 || result.DeadLettered != 1 {
		t.Fatalf("cycle: result=%+v err=%v", result, err)
	}
	deadLetters := transport.DeadLetters()
	if len(deadLetters) != 1 || deadLetters[0].Message.MessageID != poisoned {
		t.Fatalf("dead letters=%+v", deadLetters)
	}
	delivered := transport.Delivered()
	if len(delivered) != 1 || delivered[0].MessageID == poisoned {
		t.Fatalf("the healthy assignment was starved by the poisoned one: %+v", delivered)
	}
	// Both rows have left the pending set, so neither blocks the next cycle.
	for id, row := range fixture.rows(t) {
		if row.state != "published" {
			t.Fatalf("row %s is %s after the cycle", id, row.state)
		}
	}
	if result, err := publisher.PublishCycle(context.Background()); err != nil || result.Claimed != 0 {
		t.Fatalf("a resolved message was claimed again: result=%+v err=%v", result, err)
	}
}

// The store fences concurrent claimers; this pins that the publisher honours it
// rather than working around it. Two publishers racing the same outbox must
// deliver each committed assignment once between them, not once each.
func TestConcurrentPublishersDeliverEachAssignmentOnceBetweenThem(t *testing.T) {
	fixture := commitAssignments(t, otlpgen.DefaultService, "cartservice")
	transport := queuetest.New(otlpgen.DefaultRegion)
	first := fixture.publisherOwned(t, "publisher-a", transport)
	second := fixture.publisherOwned(t, "publisher-b", transport)

	start := make(chan struct{})
	var wait sync.WaitGroup
	for _, publisher := range []*outbox.Publisher{first, second} {
		wait.Add(1)
		go func(p *outbox.Publisher) {
			defer wait.Done()
			<-start
			if _, err := p.PublishCycle(context.Background()); err != nil {
				t.Errorf("concurrent cycle: %v", err)
			}
		}(publisher)
	}
	close(start)
	wait.Wait()

	delivered := transport.Delivered()
	if len(delivered) != 2 {
		t.Fatalf("%d deliveries for two committed assignments", len(delivered))
	}
	seen := map[string]bool{}
	for _, message := range delivered {
		if seen[message.MessageID] {
			t.Fatalf("assignment %s was delivered twice in one round of claims", message.MessageID)
		}
		seen[message.MessageID] = true
	}
	rows := fixture.rows(t)
	if len(rows) != 2 {
		t.Fatalf("%d rows", len(rows))
	}
	for id, row := range rows {
		if row.state != "published" || row.attempts != 1 {
			t.Fatalf("row %s: %+v; one delivery per message means one claim per message", id, row)
		}
	}
}

// A cycle cut off part way through - by a shutdown budget, or by the process
// leaving - must not strand the messages it never attempted. They belong to the
// next cycle, immediately.
func TestACycleCutOffMidBatchLeavesEveryUnattemptedAssignmentClaimableAgain(t *testing.T) {
	fixture := commitAssignments(t, otlpgen.DefaultService, "cartservice")
	transport := queuetest.New(otlpgen.DefaultRegion)
	// The first delivery is a deployment fault, which is the one failure that
	// ends a cycle rather than moving to the next message.
	transport.PublishErr = func(queue.Message) error {
		return outbox.NewFailure(outbox.ErrDeploymentFault, outbox.ReasonDeploymentFault, "AccessDenied")
	}
	publisher := fixture.publisher(t, transport)

	result, err := publisher.PublishCycle(context.Background())
	if !errors.Is(err, outbox.ErrDeploymentFault) {
		t.Fatalf("cycle: result=%+v err=%v", result, err)
	}
	if len(transport.DeadLetters()) != 0 {
		t.Fatal("a deployment fault dead-lettered a committed assignment")
	}
	for id, row := range fixture.rows(t) {
		if row.state != "pending" || row.claimToken != nil {
			t.Fatalf("row %s kept its claim after the cycle ended: %+v", id, row)
		}
		if row.nextAttemptAt.After(fixture.clock.Now()) {
			t.Fatalf("row %s is not claimable until %s, although nothing about it failed", id, row.nextAttemptAt)
		}
	}
	// A repaired deployment finds both messages waiting, with no clock advance.
	transport.PublishErr = nil
	result, err = publisher.PublishCycle(context.Background())
	if err != nil || result.Published != 2 {
		t.Fatalf("recovery cycle: result=%+v err=%v", result, err)
	}
}

// failMarkOnceStore loses exactly one acknowledgement, which is the gap ADR
// 0004 says the outbox exists to survive.
type failMarkOnceStore struct {
	*persistence.Store
	mu   sync.Mutex
	fail bool
}

func (s *failMarkOnceStore) MarkOutboxPublished(ctx context.Context, claim persistence.OutboxClaim, publishedAt time.Time) error {
	s.mu.Lock()
	failing := s.fail
	s.fail = false
	s.mu.Unlock()
	if failing {
		return persistence.ErrUnavailable
	}
	return s.Store.MarkOutboxPublished(ctx, claim, publishedAt)
}

// failMarkStore never records a publication, so every cycle republishes.
type failMarkStore struct{ *persistence.Store }

func (s *failMarkStore) MarkOutboxPublished(context.Context, persistence.OutboxClaim, time.Time) error {
	return persistence.ErrUnavailable
}

// TestCommittedAssignmentsReachARealQueueAndAreRecordedPublished is the
// end-to-end evidence for the outbox half of the delivery contract: a real
// journal, a real CockroachDB, the real publisher, and a real SQS API, with no
// double anywhere in the path.
//
// It is the acceptance test for "reliably publish investigation requests
// through the outbox". Everything narrower is covered elsewhere; what only this
// test can show is that the pieces agree with each other.
func TestCommittedAssignmentsReachARealQueueAndAreRecordedPublished(t *testing.T) {
	fixture := commitAssignments(t, "payment", "checkout")
	admin := localstacktest.SQSAdmin(t)
	main := localstacktest.Queue(t, admin, "main")
	dead := localstacktest.Queue(t, admin, "dlq")
	transport := localstacktest.Transport(t, main, dead)

	result, err := fixture.publisher(t, transport).PublishCycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Claimed != 2 || result.Published != 2 {
		t.Fatalf("cycle claimed %d and published %d, want 2 and 2 (%+v)", result.Claimed, result.Published, result)
	}

	// The durable side: every row is published, nothing is still pending.
	for id, row := range fixture.rows(t) {
		if row.state != "published" || row.published == nil {
			t.Fatalf("message %s is %q after a successful publish", id, row.state)
		}
	}

	// The queue side: both assignments arrived, each identifiable by the
	// attributes SQS Standard cannot supply itself.
	received := localstacktest.Drain(t, admin, main, 2)
	if len(received) != 2 {
		t.Fatalf("expected two assignments on the queue, got %d", len(received))
	}
	seen := map[string]bool{}
	for _, message := range received {
		for _, name := range []string{queue.SQSAttributeMessageID, queue.SQSAttributeDeduplicationKey, queue.SQSAttributeMessageType} {
			if _, ok := message.MessageAttributes[name]; !ok {
				t.Fatalf("a delivered assignment carries no %s; an at-least-once consumer could not deduplicate it", name)
			}
		}
		id := *message.MessageAttributes[queue.SQSAttributeMessageID].StringValue
		if seen[id] {
			t.Fatalf("message %s was delivered twice by one cycle", id)
		}
		seen[id] = true
		if got := *message.MessageAttributes[queue.SQSAttributeMessageType].StringValue; got != agent.AssignmentMessageType {
			t.Fatalf("assignment published with type %q", got)
		}
		// The body must still decode as the typed assignment that was committed.
		assignment, err := agent.DecodeAssignment([]byte(*message.Body))
		if err != nil {
			t.Fatalf("a delivered assignment does not decode: %v", err)
		}
		if assignment.Region != otlpgen.DefaultRegion || assignment.TenantID != tenantID {
			t.Fatalf("assignment crossed a boundary: region %q tenant %q", assignment.Region, assignment.TenantID)
		}
	}
	if len(localstacktest.Drain(t, admin, dead, 0)) != 0 {
		t.Fatal("a successful cycle also put something on the dead-letter queue")
	}
}

// TestASecondCycleAfterASuccessfulOnePublishesNothingAgain pins that the
// durable record of a delivery is what stops a republish, not luck. Without it
// every cycle would redeliver everything it had already delivered.
func TestASecondCycleAfterASuccessfulOnePublishesNothingAgain(t *testing.T) {
	fixture := commitAssignments(t, "payment")
	admin := localstacktest.SQSAdmin(t)
	main := localstacktest.Queue(t, admin, "main")
	dead := localstacktest.Queue(t, admin, "dlq")
	transport := localstacktest.Transport(t, main, dead)
	publisher := fixture.publisher(t, transport)

	if _, err := publisher.PublishCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := publisher.PublishCycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Claimed != 0 || second.Published != 0 {
		t.Fatalf("a second cycle claimed %d and published %d; a published message must not be claimable", second.Claimed, second.Published)
	}
	if got := len(localstacktest.Drain(t, admin, main, 2)); got != 1 {
		t.Fatalf("the queue holds %d copies of one assignment after two cycles", got)
	}
}
