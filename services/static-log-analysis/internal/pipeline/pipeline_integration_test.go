//go:build integration

package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/admission"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/agent"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/journal"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/pipeline"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/crdbtest"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/otlpgen"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/testids"
)

type replaceableStore struct{ store *persistence.Store }

func (s *replaceableStore) Boundary() persistence.Boundary { return s.store.Boundary() }

func (s *replaceableStore) ProcessBatch(ctx context.Context, inputs []persistence.ProcessInput) ([]persistence.ProcessResult, error) {
	return s.store.ProcessBatch(ctx, inputs)
}

type failMarkOnceJournal struct {
	*journal.Journal
	fail bool
}

func (j *failMarkOnceJournal) MarkCommittedBatch(claims []journal.CommitClaim) error {
	if j.fail {
		j.fail = false
		return journal.ErrNotHealthy
	}
	return j.Journal.MarkCommittedBatch(claims)
}

func TestPaymentErrorVerticalSliceCreatesOneClaimableAssignmentAndContext(t *testing.T) {
	ctx := context.Background()
	store, pool := realStore(t)
	// Production time.Now values almost always carry sub-microsecond precision,
	// while CockroachDB TIMESTAMPTZ round-trips at microsecond precision. Keep
	// that remainder here so the outbox claim proves the assignment timestamp
	// survives the real database boundary.
	clock := fakeclock.New(fakeclock.Origin.Add(768 * time.Nanosecond))
	policy, err := redact.MinimalPolicy().WithForbiddenValues("customer-secret")
	if err != nil {
		t.Fatal(err)
	}
	j := integrationJournal(t, clock, policy, filepath.Join(t.TempDir(), "journal"))
	replaceable := &replaceableStore{store: store}
	service := newService(t, clock, policy, j, replaceable)
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	payloads := paymentPayloads(t, producer, "customer-secret")
	for _, payload := range payloads {
		result, err := service.Ingest(ctx, envelope(clock.Now()), payload, admission.EncodingIdentity)
		if err != nil || !result.Acknowledged || result.Accepted != 1 {
			t.Fatalf("ingest: result=%+v err=%v", result, err)
		}
	}
	clock.Advance(2*time.Minute + 5*time.Second)
	dsn := pool.Config().ConnString()
	pool.Close()
	if _, err := service.Process(ctx, 100); !errors.Is(err, persistence.ErrUnavailable) {
		t.Fatalf("outage result: %v", err)
	}
	pool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("restore database connection: %v", err)
	}
	t.Cleanup(pool.Close)
	replaceable.store, err = persistence.New(pool, persistence.Config{Validator: redact.MinimalPolicy(),
		Scope: persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID}, Classification: classification,
		Topology: persistence.TopologySingleRegion})
	if err != nil {
		t.Fatal(err)
	}
	store = replaceable.store
	processed, err := service.Process(ctx, 100)
	if err != nil || processed.Processed != 5 || processed.Investigations != 1 {
		t.Fatalf("retry: result=%+v err=%v", processed, err)
	}

	tokens := []string{mustTestID(t, testids.New(testids.WithClock(clock))), mustTestID(t, testids.New(testids.WithClock(clock), testids.WithSeed(9)))}
	claimNow := clock.Now().Add(time.Millisecond)
	// The assignment must be claimable on the first poll after the transaction
	// that created it commits, with no barrier read or retry in the caller.
	outbox, err := store.ClaimOutbox(ctx, persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID}, "orchestrator", tokens, claimNow, time.Minute)
	if err != nil || len(outbox) != 1 {
		t.Fatalf("outbox claims=%d err=%v", len(outbox), err)
	}
	assignment, err := agent.DecodeAssignment(outbox[0].Message.Body)
	if err != nil || assignment.MessageType != agent.AssignmentMessageType || assignment.ContextVersion != 1 {
		t.Fatalf("assignment=%+v err=%v", assignment, err)
	}
	contextValue, err := store.InvestigationContext(ctx, outbox[0].Scope, assignment.InvestigationID, assignment.ContextVersion)
	if err != nil || contextValue.Snapshot.Validate() != nil {
		t.Fatalf("context=%+v err=%v", contextValue, err)
	}
	recordID := contextValue.Snapshot.Map["record_id"].String
	occurrence, err := store.Occurrence(ctx, outbox[0].Scope, recordID)
	if err != nil || occurrence.Projection.Body.String == "payment declined customer-secret" || policy.ValidateValue(occurrence.Projection.Body) != nil {
		t.Fatalf("unsafe occurrence crossed persistence: occurrence=%+v err=%v", occurrence, err)
	}
	leaseToken := mustTestID(t, testids.New(testids.WithClock(clock), testids.WithSeed(17)))
	if _, err := store.AcquireInvestigation(ctx, outbox[0].Scope, assignment.InvestigationID, "agent-a", leaseToken, claimNow, time.Minute); err != nil {
		t.Fatalf("claim investigation: %v", err)
	}
	incidentSnapshot, err := store.Incident(ctx, outbox[0].Scope, assignment.IncidentID)
	if err != nil || incidentSnapshot.OccurrenceCount != 5 || incidentSnapshot.ActiveInvestigationID != assignment.InvestigationID {
		t.Fatalf("incident=%+v err=%v", incidentSnapshot, err)
	}
	var generations, investigations, messages int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM incident_generations),
		(SELECT count(*) FROM investigations),
		(SELECT count(*) FROM outbox_messages)`).Scan(&generations, &investigations, &messages); err != nil {
		t.Fatal(err)
	}
	if generations != 1 || investigations != 1 || messages != 1 {
		t.Fatalf("generations=%d investigations=%d outbox=%d", generations, investigations, messages)
	}
	// Replaying all producer deliveries after journal and database durability is
	// an acknowledged no-op: the original record IDs remain the global gate.
	for _, payload := range payloads {
		if result, err := service.Ingest(ctx, envelope(clock.Now()), payload, admission.EncodingIdentity); err != nil || !result.Acknowledged {
			t.Fatalf("duplicate ingest: result=%+v err=%v", result, err)
		}
	}
	stats, err := j.Stats()
	if err != nil || stats.Committed != 5 || stats.Pending != 0 || stats.Claimed != 0 {
		t.Fatalf("duplicate changed journal: stats=%+v err=%v", stats, err)
	}
}

func TestRestartAfterEachCommittedPrefixRebuildsOneGenerationAndTriggersOnFifth(t *testing.T) {
	for committed := 1; committed <= 4; committed++ {
		t.Run(fmt.Sprintf("%d_committed", committed), func(t *testing.T) {
			ctx := context.Background()
			store, pool := realStore(t)
			clock := fakeclock.NewAtOrigin()
			policy := redact.MinimalPolicy()
			dir := filepath.Join(t.TempDir(), "journal")
			j := integrationJournalNoCleanup(t, clock, policy, dir)
			producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
			payloads := paymentPayloads(t, producer, "")
			service := newService(t, clock, policy, j, store)
			for _, payload := range payloads[:committed] {
				if _, err := service.Ingest(ctx, envelope(clock.Now()), payload, admission.EncodingIdentity); err != nil {
					t.Fatal(err)
				}
			}
			clock.Advance(2*time.Minute + 5*time.Second)
			if result, err := service.Process(ctx, 100); err != nil || result.Processed != committed || result.Investigations != 0 {
				t.Fatalf("prefix process: result=%+v err=%v", result, err)
			}
			if err := j.Close(); err != nil {
				t.Fatal(err)
			}
			j = integrationJournalNoCleanup(t, clock, policy, dir)
			t.Cleanup(func() { _ = j.Close() })
			service = newService(t, clock, policy, j, store)
			for _, payload := range payloads[committed:] {
				if _, err := service.Ingest(ctx, envelope(clock.Now()), payload, admission.EncodingIdentity); err != nil {
					t.Fatal(err)
				}
			}
			result, err := service.Process(ctx, 100)
			if err != nil || result.Processed != 5-committed || result.Investigations != 1 {
				t.Fatalf("post-restart process: result=%+v err=%v", result, err)
			}
			var families, generations, occurrences, investigations int
			if err := pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM incident_families), (SELECT count(*) FROM incident_generations),
				(SELECT count(*) FROM occurrences), (SELECT count(*) FROM investigations)`).Scan(&families, &generations, &occurrences, &investigations); err != nil {
				t.Fatal(err)
			}
			if families != 1 || generations != 1 || occurrences != 5 || investigations != 1 {
				t.Fatalf("families=%d generations=%d occurrences=%d investigations=%d", families, generations, occurrences, investigations)
			}
		})
	}
}

func TestDatabaseCommitJournalMarkGapReplaysWithoutMultiplication(t *testing.T) {
	ctx := context.Background()
	store, pool := realStore(t)
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	base := integrationJournal(t, clock, policy, filepath.Join(t.TempDir(), "journal"))
	j := &failMarkOnceJournal{Journal: base, fail: true}
	service := newService(t, clock, policy, j, store)
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	for _, payload := range paymentPayloads(t, producer, "") {
		if _, err := service.Ingest(ctx, envelope(clock.Now()), payload, admission.EncodingIdentity); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(2*time.Minute + 5*time.Second)
	if _, err := service.Process(ctx, 100); !errors.Is(err, journal.ErrNotHealthy) {
		t.Fatalf("mark gap error=%v", err)
	}
	stats, _ := base.Stats()
	if stats.Claimed != 5 || stats.Committed != 0 {
		t.Fatalf("gap lost claims: %+v", stats)
	}
	result, err := service.Process(ctx, 100)
	if err != nil || result.Processed != 5 || result.Duplicates != 5 || result.Investigations != 0 {
		t.Fatalf("gap retry: result=%+v err=%v", result, err)
	}
	var occurrences, investigations, messages int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM occurrences),
		(SELECT count(*) FROM investigations), (SELECT count(*) FROM outbox_messages)`).Scan(&occurrences, &investigations, &messages); err != nil {
		t.Fatal(err)
	}
	if occurrences != 5 || investigations != 1 || messages != 1 {
		t.Fatalf("occurrences=%d investigations=%d messages=%d", occurrences, investigations, messages)
	}
}

func realStore(t *testing.T) (*persistence.Store, *pgxpool.Pool) {
	t.Helper()
	pool := crdbtest.Pool(t)
	if err := persistence.ApplyMigrations(context.Background(), pool, persistence.TopologySingleRegion); err != nil {
		t.Fatal(err)
	}
	store, err := persistence.New(pool, persistence.Config{Validator: redact.MinimalPolicy(), Scope: persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID}, Classification: classification, Topology: persistence.TopologySingleRegion})
	if err != nil {
		t.Fatal(err)
	}
	return store, pool
}

func integrationJournal(t *testing.T, clock *fakeclock.Clock, policy *redact.Policy, dir string) *journal.Journal {
	t.Helper()
	j := integrationJournalNoCleanup(t, clock, policy, dir)
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func integrationJournalNoCleanup(t *testing.T, clock *fakeclock.Clock, policy *redact.Policy, dir string) *journal.Journal {
	t.Helper()
	j, err := journal.Open(journal.Config{Dir: dir, Owner: "pipeline-integration", TenantID: tenantID,
		Region: otlpgen.DefaultRegion, Classification: classification, Clock: clock, Validator: policy,
		MaxBytes: 64 << 20, MinFreeBytes: 1, FreeSpace: func(string) (uint64, error) { return math.MaxUint64, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func paymentPayloads(t *testing.T, producer *otlpgen.Producer, secret string) [][]byte {
	t.Helper()
	payloads := make([][]byte, 5)
	for i := range payloads {
		body := "payment declined"
		if secret != "" {
			body += " " + secret
		}
		record := producer.PaymentError(otlpgen.WithBody(body), otlpgen.AtEventTime(uint64(fakeclock.Origin.Add(time.Duration(i)*time.Second).UnixNano())))
		payloads[i] = marshalRequest(t, producer.Request(record))
	}
	return payloads
}

func mustTestID(t *testing.T, source *testids.Source) string {
	t.Helper()
	id, err := source.New()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// dieOnMarkJournal terminates the process in the exact gap the journal contract
// names: after the CockroachDB transaction has committed and before the journal
// records that it did.
type dieOnMarkJournal struct{ *journal.Journal }

func (j *dieOnMarkJournal) MarkCommittedBatch([]journal.CommitClaim) error {
	fmt.Println("ACK")
	os.Exit(0)
	return nil
}

// journal.md: "If the process crashes after database commit but before journal
// update, replay is safe because CockroachDB's record_id uniqueness makes the
// transaction idempotent." The existing gap test stays in one process and keeps
// its in-memory state; this one kills the process outright, so replay has to
// come from the durable journal and the durable record gate alone.
func TestProcessDeathBetweenDatabaseCommitAndJournalMarkReplaysOnce(t *testing.T) {
	if os.Getenv("PIPELINE_CRASH_HELPER") == "1" {
		t.Skip("helper is selected by its dedicated test name")
	}
	ctx := context.Background()
	_, pool := realStore(t)
	dir := filepath.Join(t.TempDir(), "journal")

	cmd := exec.Command(os.Args[0], "-test.run=^TestPipelineCrashHelper$")
	cmd.Env = append(os.Environ(), "PIPELINE_CRASH_HELPER=1",
		"PIPELINE_CRASH_DIR="+dir, "PIPELINE_CRASH_DSN="+pool.Config().ConnString())
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %v: %s", err, output)
	}
	if !strings.Contains(string(output), "ACK\n") {
		t.Fatalf("helper did not reach the commit gap: %q", output)
	}

	var occurrences, investigations, messages int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM occurrences),
		(SELECT count(*) FROM investigations), (SELECT count(*) FROM outbox_messages)`).Scan(&occurrences, &investigations, &messages); err != nil {
		t.Fatal(err)
	}
	if occurrences != 5 || investigations != 1 || messages != 1 {
		t.Fatalf("the committed transaction did not survive the crash: occurrences=%d investigations=%d messages=%d",
			occurrences, investigations, messages)
	}

	// The replica comes back on the same volume. Nothing in memory survived, so
	// every record replays through M4, whose record gate answers duplicate.
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := integrationJournal(t, clock, policy, dir)
	if stats, statsErr := j.Stats(); statsErr != nil || stats.Claimed != 5 || stats.Committed != 0 {
		t.Fatalf("crash lost the claims: stats=%+v err=%v", stats, statsErr)
	}
	store, _ := storeOn(t, pool)
	service := newService(t, clock, policy, j, store)
	clock.Advance(2*time.Minute + 5*time.Second)
	result, err := service.Process(ctx, 100)
	if err != nil || result.Processed != 5 || result.Duplicates != 5 || result.Investigations != 0 {
		t.Fatalf("replay after crash: result=%+v err=%v", result, err)
	}
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM occurrences),
		(SELECT count(*) FROM investigations), (SELECT count(*) FROM outbox_messages)`).Scan(&occurrences, &investigations, &messages); err != nil {
		t.Fatal(err)
	}
	if occurrences != 5 || investigations != 1 || messages != 1 {
		t.Fatalf("replay multiplied durable state: occurrences=%d investigations=%d messages=%d",
			occurrences, investigations, messages)
	}
	stats, err := j.Stats()
	if err != nil || stats.Committed != 5 || stats.Claimed != 0 || stats.Pending != 0 {
		t.Fatalf("replay did not close the gap: stats=%+v err=%v", stats, err)
	}
}

// TestPipelineCrashHelper is the child process of the test above. It runs the
// real coordinator against the parent's CockroachDB and journal directory and
// exits abruptly inside the commit gap.
func TestPipelineCrashHelper(t *testing.T) {
	if os.Getenv("PIPELINE_CRASH_HELPER") != "1" {
		return
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("PIPELINE_CRASH_DSN"))
	if err != nil {
		os.Exit(20)
	}
	store, err := persistence.New(pool, persistence.Config{Validator: redact.MinimalPolicy(),
		Scope:          persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID},
		Classification: classification, Topology: persistence.TopologySingleRegion})
	if err != nil {
		os.Exit(21)
	}
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	base := integrationJournalNoCleanup(t, clock, policy, os.Getenv("PIPELINE_CRASH_DIR"))
	service, err := pipeline.New(pipeline.Config{Clock: clock, IDs: testids.New(testids.WithClock(clock)),
		Journal: &dieOnMarkJournal{Journal: base}, Store: store, Policy: policy,
		Scope:          persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID},
		Classification: classification})
	if err != nil {
		os.Exit(22)
	}
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	for _, payload := range paymentPayloads(t, producer, "") {
		if _, err := service.Ingest(ctx, envelope(clock.Now()), payload, admission.EncodingIdentity); err != nil {
			os.Exit(23)
		}
	}
	clock.Advance(2*time.Minute + 5*time.Second)
	if _, err := service.Process(ctx, 100); err != nil {
		os.Exit(24)
	}
	// Reaching here means the gap was never entered.
	os.Exit(25)
}

// After compaction the journal no longer holds the record at all, so the only
// thing standing between a redelivered payload and a second occurrence is
// records_seen in CockroachDB.
func TestDuplicateDeliveryAfterJournalCompactionIsGatedByRecordsSeen(t *testing.T) {
	ctx := context.Background()
	store, pool := realStore(t)
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	dir := filepath.Join(t.TempDir(), "journal")
	j := integrationJournal(t, clock, policy, dir)
	service := newService(t, clock, policy, j, store)
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	payloads := paymentPayloads(t, producer, "")
	for _, payload := range payloads {
		if _, err := service.Ingest(ctx, envelope(clock.Now()), payload, admission.EncodingIdentity); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(2*time.Minute + 5*time.Second)
	if result, err := service.Process(ctx, 100); err != nil || result.Processed != 5 || result.Investigations != 1 {
		t.Fatalf("first pass: result=%+v err=%v", result, err)
	}

	clock.Advance(journal.DefaultSafetyDelay + time.Nanosecond)
	if compacted, err := j.Compact(journal.MaxTransitionRecords); err != nil || compacted != 5 {
		t.Fatalf("compaction removed %d records: %v", compacted, err)
	}
	if stats, err := j.Stats(); err != nil || stats.Committed != 0 || stats.Pending != 0 {
		t.Fatalf("journal still retains the records: stats=%+v err=%v", stats, err)
	}

	// A restarted coordinator has no in-memory identity map and no retained
	// journal payload to rebuild one from.
	replay := newService(t, clock, policy, j, store)
	for _, payload := range payloads {
		result, err := replay.Ingest(ctx, envelope(clock.Now()), payload, admission.EncodingIdentity)
		if err != nil || !result.Acknowledged || result.Accepted != 1 {
			t.Fatalf("redelivery after compaction: result=%+v err=%v", result, err)
		}
	}
	clock.Advance(2*time.Minute + 5*time.Second)
	result, err := replay.Process(ctx, 100)
	if err != nil || result.Processed != 5 {
		t.Fatalf("redelivery process: result=%+v err=%v", result, err)
	}
	if result.Duplicates != 5 {
		t.Fatalf("%d of 5 redelivered records were counted as duplicates by the durable gate", result.Duplicates)
	}
	if result.Investigations != 0 {
		t.Fatalf("redelivery after compaction started %d investigations", result.Investigations)
	}
	var occurrences, investigations, messages int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM occurrences),
		(SELECT count(*) FROM investigations), (SELECT count(*) FROM outbox_messages)`).Scan(&occurrences, &investigations, &messages); err != nil {
		t.Fatal(err)
	}
	if occurrences != 5 || investigations != 1 || messages != 1 {
		t.Fatalf("post-compaction redelivery multiplied durable state: occurrences=%d investigations=%d messages=%d",
			occurrences, investigations, messages)
	}
}

func storeOn(t *testing.T, pool *pgxpool.Pool) (*persistence.Store, *pgxpool.Pool) {
	t.Helper()
	store, err := persistence.New(pool, persistence.Config{Validator: redact.MinimalPolicy(),
		Scope:          persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID},
		Classification: classification, Topology: persistence.TopologySingleRegion})
	if err != nil {
		t.Fatal(err)
	}
	return store, pool
}
