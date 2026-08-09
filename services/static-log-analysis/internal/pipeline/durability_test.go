package pipeline_test

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/admission"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/incident"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/journal"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/pipeline"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/otlpgen"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/testids"
)

// A database outage can outlast a journal claim. When the claim expires the
// record returns to pending and is claimed again, so the coordinator must drop
// the in-memory attempt it is still holding instead of committing a stale token
// or presenting the record to the Store twice in one cohort.
func TestExpiredClaimIsReclaimedAndContributesExactlyOnce(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	store := &capturingStore{failures: 1}
	service := newService(t, clock, policy, j, store)
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	if _, err := service.Ingest(context.Background(), envelope(clock.Now()),
		marshalRequest(t, producer.Request(producer.PaymentError())), admission.EncodingIdentity); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2*time.Minute + 5*time.Second)
	// A cohort limit of one makes the stale in-memory attempt consume the whole
	// budget, so a coordinator which kept it would commit an expired token.
	if _, err := service.Process(context.Background(), 1); !errors.Is(err, persistence.ErrUnavailable) {
		t.Fatalf("first attempt error=%v, want the database outage", err)
	}
	if stats, err := j.Stats(); err != nil || stats.Claimed != 1 {
		t.Fatalf("first attempt did not retain its claim: stats=%+v err=%v", stats, err)
	}

	// The first attempt is still held in memory when its journal claim expires.
	clock.Advance(journal.DefaultClaimTTL)
	result, err := service.Process(context.Background(), 1)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if result.Claimed != 1 || result.Processed != 1 {
		t.Fatalf("expired claim was not re-claimed exactly once: %+v", result)
	}
	stats, err := j.Stats()
	if err != nil || stats.Committed != 1 || stats.Claimed != 0 || stats.Pending != 0 {
		t.Fatalf("re-claimed record did not commit: stats=%+v err=%v", stats, err)
	}
	last := store.inputs[len(store.inputs)-1]
	if len(last) != 1 {
		t.Fatalf("re-claim presented %d inputs, want the one record", len(last))
	}
	if last[0].Replay.Priority() != journal.PriorityHigh {
		t.Fatalf("re-claim lost the sealed replay identity: %+v", last[0].Replay)
	}
}

// Ingestion is the acknowledgement boundary. A transport may deliver the same
// bytes from several connections at once; every caller must be told the truth
// and the journal must still hold exactly one durable record.
func TestConcurrentDuplicateIngestAcknowledgesOneDurableRecord(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	service := newService(t, clock, policy, j, &capturingStore{})
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	payload := marshalRequest(t, producer.Request(producer.PaymentError()))
	sent := envelope(clock.Now())

	const callers = 8
	results := make(chan pipeline.IngestResult, callers)
	failures := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := service.Ingest(context.Background(), sent, payload, admission.EncodingIdentity)
			if err != nil {
				failures <- err
				return
			}
			results <- result
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatalf("concurrent duplicate ingest: %v", err)
	}
	acknowledged := 0
	for result := range results {
		if !result.Acknowledged || result.Accepted != 1 {
			t.Fatalf("concurrent duplicate ingest result: %+v", result)
		}
		acknowledged++
	}
	if acknowledged != callers {
		t.Fatalf("%d of %d concurrent deliveries were acknowledged", acknowledged, callers)
	}
	stats, err := j.Stats()
	if err != nil || stats.Pending != 1 {
		t.Fatalf("concurrent duplicates created %+v, want one pending record", stats)
	}
}

// Ingestion and processing run on different goroutines in production. Neither
// may lose an acknowledged record or present one to the Store twice.
func TestConcurrentIngestAndProcessContributeEachRecordOnce(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	store := &capturingStore{}
	service := newService(t, clock, policy, j, store)
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	const records = 24
	payloads := make([][]byte, records)
	for i := range payloads {
		record := producer.PaymentError(otlpgen.AtEventTime(uint64(fakeclock.Origin.Add(time.Duration(i) * time.Second).UnixNano())))
		payloads[i] = marshalRequest(t, producer.Request(record))
	}
	// Everything is already finalized, so a Process racing an Ingest may or may
	// not see a given record; what it must never do is see one twice.
	clock.Advance(2*time.Minute + time.Duration(records)*time.Second)

	var processErr atomic.Value
	var wg sync.WaitGroup
	for _, payload := range payloads {
		wg.Add(1)
		go func(payload []byte) {
			defer wg.Done()
			if _, err := service.Ingest(context.Background(), envelope(clock.Now()), payload, admission.EncodingIdentity); err != nil {
				processErr.Store(err)
			}
		}(payload)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := service.Process(context.Background(), 100); err != nil {
				processErr.Store(err)
			}
		}()
	}
	wg.Wait()
	if err, ok := processErr.Load().(error); ok && err != nil {
		t.Fatalf("concurrent ingest and process: %v", err)
	}
	for attempts := 0; attempts < records; attempts++ {
		result, err := service.Process(context.Background(), 100)
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		if result.Claimed == 0 && result.Processed == 0 {
			break
		}
	}
	stats, err := j.Stats()
	if err != nil || stats.Committed != records || stats.Pending != 0 || stats.Claimed != 0 {
		t.Fatalf("concurrent run did not commit every acknowledged record: stats=%+v err=%v", stats, err)
	}
	contributed := map[string]int{}
	for _, batch := range store.inputs {
		for _, input := range batch {
			contributed[input.Record.RecordID]++
		}
	}
	if len(contributed) != records {
		t.Fatalf("%d distinct records reached the store, want %d", len(contributed), records)
	}
	for recordID, count := range contributed {
		if count != 1 {
			t.Fatalf("record %s was presented %d times", recordID, count)
		}
	}
}

// The slice's primary ordering invariant: nothing is acknowledged before the
// synchronized Pebble append succeeds.
func TestIngestDoesNotAcknowledgeWhenTheAppendFails(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	for name, appendErr := range map[string]error{
		"unhealthy": journal.ErrNotHealthy,
		"corrupt":   journal.ErrCorruption,
		"capacity":  journal.ErrCapacity,
	} {
		t.Run(name, func(t *testing.T) {
			service := newServiceOn(t, clock, policy,
				&stubJournal{appendErr: appendErr, manifest: matchingManifest(policy)}, &capturingStore{})
			producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
			result, err := service.Ingest(context.Background(), envelope(clock.Now()),
				marshalRequest(t, producer.Request(producer.PaymentError())), admission.EncodingIdentity)
			if result.Acknowledged {
				t.Fatalf("acknowledged a record the journal never accepted: %+v", result)
			}
			if !errors.Is(err, pipeline.ErrJournalUnavailable) {
				t.Fatalf("append failure error=%v, want retryable journal unavailability", err)
			}
			if got := service.Counters().JournalRejections; got != 0 {
				t.Fatalf("retryable append failure counted %d permanent rejections", got)
			}
		})
	}
}

// operations.md: above 95% utilization mandatory traffic is backpressured. A
// full journal must produce a retryable failure and never a false
// acknowledgement, and it must not be reported as a permanent refusal.
func TestFullJournalBackpressuresIngestionWithoutAcknowledging(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	var free atomic.Uint64
	free.Store(1 << 40)
	j, err := journal.Open(journal.Config{
		Dir: filepath.Join(t.TempDir(), "journal"), Owner: "capacity-test", TenantID: tenantID,
		Region: otlpgen.DefaultRegion, Classification: classification, Clock: clock, Validator: policy,
		MaxBytes: 32 << 20, MinFreeBytes: 1 << 20, FreeSpace: func(string) (uint64, error) { return free.Load(), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	service := newService(t, clock, policy, j, &capturingStore{})
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	if _, err := service.Ingest(context.Background(), envelope(clock.Now()),
		marshalRequest(t, producer.Request(producer.PaymentError())), admission.EncodingIdentity); err != nil {
		t.Fatalf("healthy ingest: %v", err)
	}

	free.Store(0)
	result, err := service.Ingest(context.Background(), envelope(clock.Now()),
		marshalRequest(t, producer.Request(producer.PaymentError())), admission.EncodingIdentity)
	if result.Acknowledged {
		t.Fatalf("acknowledged a mandatory record the full journal shed: %+v", result)
	}
	if !errors.Is(err, pipeline.ErrJournalUnavailable) {
		t.Fatalf("full journal error=%v, want retryable backpressure", err)
	}
	if errors.Is(err, pipeline.ErrRequestRejected) {
		t.Fatalf("full journal was reported as a permanent rejection: %v", err)
	}
	stats, statsErr := j.Stats()
	if statsErr != nil || stats.Pending != 1 {
		t.Fatalf("shed record reached the journal anyway: stats=%+v err=%v", stats, statsErr)
	}

	// Capacity is transient by design: the same record succeeds once the
	// operator or compaction returns free space.
	free.Store(1 << 40)
	if retried, retryErr := service.Ingest(context.Background(), envelope(clock.Now()),
		marshalRequest(t, producer.Request(producer.PaymentError())), admission.EncodingIdentity); retryErr != nil || !retried.Acknowledged {
		t.Fatalf("retry after capacity recovered: result=%+v err=%v", retried, retryErr)
	}
}

// Startup recovery is paged so a high-volume journal cannot be replayed in one
// unbounded allocation. Every page must still be walked.
func TestStartupRecoveryPagesThroughEveryRetainedRecord(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	service := newService(t, clock, policy, j, &capturingStore{})
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	const records = 5
	for i := 0; i < records; i++ {
		// A record without service identity is admitted by design and cannot be
		// observed, so recovery counts it. That count is the honest observable
		// for "every page was walked".
		request := withoutServiceName(producer.Request(producer.PaymentError(
			otlpgen.AtEventTime(uint64(fakeclock.Origin.Add(time.Duration(i) * time.Second).UnixNano())))))
		if _, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, request), admission.EncodingIdentity); err != nil {
			t.Fatal(err)
		}
	}

	restarted, err := pipeline.New(pipeline.Config{Clock: clock, IDs: testids.New(testids.WithClock(clock)),
		Journal: j, Store: &capturingStore{}, Policy: policy, RecoveryMax: 2,
		Scope:          persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID},
		Classification: classification})
	if err != nil {
		t.Fatalf("paged restart: %v", err)
	}
	if got := restarted.Counters().RecoverySkipped; got != records {
		t.Fatalf("paged recovery walked %d of %d retained records", got, records)
	}
}

// A record strictly behind the finalization frontier is evidence: it is
// persisted and labeled, but it may not carry assignment material and may not
// cause an agent to run.
func TestLateRecordBecomesAnEvidenceOnlyOccurrenceWithNoCandidate(t *testing.T) {
	origin := fakeclock.Origin
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	store := &electingStore{}
	service := newService(t, clock, policy, j, store)
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	// The episode opens at the origin so its start is behind the frontier the
	// late arrival is measured against.
	for _, offset := range []time.Duration{0, time.Second} {
		record := producer.PaymentError(otlpgen.AtEventTime(uint64(origin.Add(offset).UnixNano())))
		if _, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, producer.Request(record)), admission.EncodingIdentity); err != nil {
			t.Fatal(err)
		}
	}
	// Observed time advances with the clock, because an event time far ahead of
	// it would be inferred away instead of carrying the frontier.
	clock.Advance(10 * time.Minute)
	frontier := producer.PaymentError(otlpgen.AtEventTime(uint64(origin.Add(10 * time.Minute).UnixNano())))
	if _, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, producer.Request(frontier)), admission.EncodingIdentity); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	if _, err := service.Process(context.Background(), 100); err != nil {
		t.Fatal(err)
	}

	lateAt := origin.Add(7*time.Minute + 30*time.Second)
	late := producer.PaymentError(otlpgen.AtEventTime(uint64(lateAt.UnixNano())))
	if _, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, producer.Request(late)), admission.EncodingIdentity); err != nil {
		t.Fatal(err)
	}
	result, err := service.Process(context.Background(), 100)
	if err != nil || result.Processed != 1 {
		t.Fatalf("late process: result=%+v err=%v", result, err)
	}
	if result.Investigations != 0 {
		t.Fatalf("an evidence-only record started %d investigations", result.Investigations)
	}
	latest := store.inputs[len(store.inputs)-1]
	if len(latest) != 1 || !latest[0].Record.EventTime.Equal(lateAt) {
		t.Fatalf("unexpected late cohort: %+v", latest)
	}
	if !latest[0].EvidenceOnly || !latest[0].Late {
		t.Fatalf("late arrival was not persisted as late evidence: %+v", latest[0])
	}
	if latest[0].InvestigationCandidate != nil {
		t.Fatalf("evidence-only occurrence carried assignment material: %+v", latest[0].InvestigationCandidate)
	}
}

// Quiet changes detection state at exactly fifteen minutes of inactivity, and
// the coordinator persists the engine's classification rather than its own.
func TestDetectionBecomesQuietAtExactlyFifteenMinutesThroughTheCoordinator(t *testing.T) {
	for name, elapsed := range map[string]time.Duration{
		"one nanosecond before": incident.DefaultQuietPeriod - time.Nanosecond,
		"exact quiet period":    incident.DefaultQuietPeriod,
	} {
		t.Run(name, func(t *testing.T) {
			clock := fakeclock.NewAtOrigin()
			policy := redact.MinimalPolicy()
			j := openJournal(t, clock, policy)
			store := &capturingStore{}
			service := newService(t, clock, policy, j, store)
			producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
			// Bounded activity is min(event_time, observed_time); pinning both to
			// the origin makes the quiet instant exactly fifteen minutes later.
			record := producer.PaymentError(
				otlpgen.AtEventTime(uint64(fakeclock.Origin.UnixNano())),
				otlpgen.AtObservedTime(uint64(fakeclock.Origin.UnixNano())))
			if _, err := service.Ingest(context.Background(), envelope(clock.Now()),
				marshalRequest(t, producer.Request(record)), admission.EncodingIdentity); err != nil {
				t.Fatal(err)
			}
			clock.Advance(elapsed)
			if _, err := service.Process(context.Background(), 100); err != nil {
				t.Fatal(err)
			}
			if len(store.inputs) != 1 || len(store.inputs[0]) != 1 {
				t.Fatalf("unexpected cohort: %+v", store.inputs)
			}
			want := string(incident.DetectionActive)
			if elapsed == incident.DefaultQuietPeriod {
				want = string(incident.DetectionQuiet)
			}
			if got := store.inputs[0][0].DetectionStatus; got != want {
				t.Fatalf("detection_status=%q after %s, want %q", got, elapsed, want)
			}
			if quietAt := store.inputs[0][0].QuietAt; !quietAt.Equal(fakeclock.Origin.Add(incident.DefaultQuietPeriod)) {
				t.Fatalf("quiet_at=%s, want %s", quietAt, fakeclock.Origin.Add(incident.DefaultQuietPeriod))
			}
		})
	}
}

// Milestone 1 acceptance through the production coordinator: the sixth through
// hundredth errors join one incident and one generation. Suppressing the second
// agent itself is the Store's transactional job, covered against a real
// CockroachDB; here the Store elects once and the coordinator must report that
// once and no more.
func TestHundredErrorsReachOneIncidentGenerationWithOneElection(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	store := &electingStore{}
	service := newService(t, clock, policy, j, store)
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	const records = 100
	for i := 0; i < records; i++ {
		record := producer.PaymentError(otlpgen.AtEventTime(uint64(fakeclock.Origin.Add(time.Duration(i) * time.Second).UnixNano())))
		if _, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, producer.Request(record)), admission.EncodingIdentity); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(2*time.Minute + records*time.Second)
	result, err := service.Process(context.Background(), persistence.MaxProcessBatch)
	if err != nil || result.Processed != records {
		t.Fatalf("process: result=%+v err=%v", result, err)
	}
	if result.Investigations != 1 {
		t.Fatalf("%d investigations reported across %d errors, want one", result.Investigations, records)
	}
	inputs := store.inputs[len(store.inputs)-1]
	if len(inputs) != records {
		t.Fatalf("cohort of %d, want %d", len(inputs), records)
	}
	incidentID, generation := inputs[0].IncidentID, inputs[0].Generation
	for _, input := range inputs {
		if input.IncidentID != incidentID || input.Generation != generation {
			t.Fatalf("record %s split the incident: incident=%s generation=%d", input.Record.RecordID, input.IncidentID, input.Generation)
		}
	}
	if candidates := preparedCandidates(inputs); candidates != records {
		t.Fatalf("prepared %d candidates, want one per finalized ordinary error", candidates)
	}
}

// analysis restart with unfinished work: the coordinator still owns the claims
// it held when it stopped. Waiting out a full claim TTL before touching them
// again is dead time the 30-minute outage budget in operations.md cannot spend.
func TestRestartReadoptsThisOwnersClaimsInsteadOfStalling(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	service := newService(t, clock, policy, j, &errorStore{err: persistence.ErrUnavailable})
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	const records = 2
	for i := 0; i < records; i++ {
		record := producer.PaymentError(otlpgen.AtEventTime(uint64(fakeclock.Origin.Add(time.Duration(i) * time.Second).UnixNano())))
		if _, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, producer.Request(record)), admission.EncodingIdentity); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(2*time.Minute + 5*time.Second)
	if _, err := service.Process(context.Background(), 100); !errors.Is(err, persistence.ErrUnavailable) {
		t.Fatalf("outage error=%v", err)
	}
	if stats, err := j.Stats(); err != nil || stats.Claimed != records {
		t.Fatalf("setup did not leave claims held: stats=%+v err=%v", stats, err)
	}

	// The process dies here and comes back on the same volume with the same
	// owner. No time passes, so nothing has expired.
	store := &capturingStore{}
	restarted := newService(t, clock, policy, j, store)
	result, err := restarted.Process(context.Background(), 100)
	if err != nil {
		t.Fatalf("restarted process: %v", err)
	}
	if result.Processed != records {
		t.Fatalf("restart stalled %d claimed records: %+v", records-result.Processed, result)
	}
	stats, err := j.Stats()
	if err != nil || stats.Committed != records || stats.Claimed != 0 || stats.Pending != 0 {
		t.Fatalf("re-adopted claims did not commit: stats=%+v err=%v", stats, err)
	}
	contributed := map[string]int{}
	for _, batch := range store.inputs {
		for _, input := range batch {
			contributed[input.Record.RecordID]++
		}
	}
	if len(contributed) != records {
		t.Fatalf("%d records reached the store after restart, want %d", len(contributed), records)
	}
}

// operations.md requires surviving a 30-minute CockroachDB outage with the
// journal retaining work. The claim TTL is much shorter than that, so a worker
// which is still retrying its cohort must hold its claims rather than let them
// lapse and re-derive the same work every TTL.
func TestClaimsAreRenewedThroughALongDatabaseOutage(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	store := &replaceableUnitStore{err: persistence.ErrUnavailable}
	service := newService(t, clock, policy, j, store)
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	const records = 5
	for i := 0; i < records; i++ {
		record := producer.PaymentError(otlpgen.AtEventTime(uint64(fakeclock.Origin.Add(time.Duration(i) * time.Second).UnixNano())))
		if _, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, producer.Request(record)), admission.EncodingIdentity); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(2*time.Minute + 5*time.Second)
	if _, err := service.Process(context.Background(), 100); !errors.Is(err, persistence.ErrUnavailable) {
		t.Fatalf("first outage attempt error=%v", err)
	}

	const outage = 30 * time.Minute
	step := journal.DefaultClaimTTL - time.Minute
	for elapsed := time.Duration(0); elapsed < outage; elapsed += step {
		clock.Advance(step)
		result, err := service.Process(context.Background(), 100)
		if !errors.Is(err, persistence.ErrUnavailable) {
			t.Fatalf("outage attempt at %s error=%v", elapsed, err)
		}
		if result.Claimed != 0 {
			t.Fatalf("outage attempt at %s re-claimed %d records instead of holding them", elapsed, result.Claimed)
		}
		stats, statsErr := j.Stats()
		if statsErr != nil || stats.Claimed != records || stats.Pending != 0 {
			t.Fatalf("outage attempt at %s lost its claims: stats=%+v err=%v", elapsed, stats, statsErr)
		}
	}

	store.err = nil
	result, err := service.Process(context.Background(), 100)
	if err != nil || result.Processed != records || result.Claimed != 0 {
		t.Fatalf("recovery after the outage: result=%+v err=%v", result, err)
	}
	stats, err := j.Stats()
	if err != nil || stats.Committed != records {
		t.Fatalf("outage lost acknowledged work: stats=%+v err=%v", stats, err)
	}
}

// replaceableUnitStore fails until its error is cleared, modelling an outage
// which ends without the coordinator being rebuilt.
type replaceableUnitStore struct {
	capturingStore
	err error
}

func (s *replaceableUnitStore) ProcessBatch(ctx context.Context, inputs []persistence.ProcessInput) ([]persistence.ProcessResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.capturingStore.ProcessBatch(ctx, inputs)
}

// The engine compacts state past its retention horizon, but a claim held
// through an outage longer than that horizon must still resolve. Losing the
// payload would leave the claim renewable forever and never persistable.
func TestClaimsHeldLongerThanEngineRetentionStillPersist(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	store := &replaceableUnitStore{err: persistence.ErrUnavailable}
	service := newService(t, clock, policy, j, store)
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	const records = 5
	for i := 0; i < records; i++ {
		record := producer.PaymentError(otlpgen.AtEventTime(uint64(fakeclock.Origin.Add(time.Duration(i) * time.Second).UnixNano())))
		if _, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, producer.Request(record)), admission.EncodingIdentity); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(2*time.Minute + 5*time.Second)

	outage := incident.DefaultStateRetention + time.Hour
	step := journal.DefaultClaimTTL - time.Minute
	for elapsed := time.Duration(0); elapsed < outage; elapsed += step {
		if _, err := service.Process(context.Background(), 100); !errors.Is(err, persistence.ErrUnavailable) {
			t.Fatalf("outage attempt at %s error=%v", elapsed, err)
		}
		clock.Advance(step)
	}

	store.err = nil
	result, err := service.Process(context.Background(), 100)
	if err != nil || result.Processed != records {
		t.Fatalf("a claim held past the retention horizon never persisted: result=%+v err=%v", result, err)
	}
	stats, err := j.Stats()
	if err != nil || stats.Committed != records || stats.Claimed != 0 {
		t.Fatalf("outage longer than retention lost work: stats=%+v err=%v", stats, err)
	}
}

// interceptingJournal remembers the claims the journal handed out, so a test
// can act on one of them the way a lost response would, and can refuse to renew
// one designated record the way a journal which no longer agrees this worker
// holds it does. Every sibling's claim stays untouched and valid.
type interceptingJournal struct {
	*journal.Journal
	claims map[string]string
	stale  string
}

func newInterceptingJournal(inner *journal.Journal) *interceptingJournal {
	return &interceptingJournal{Journal: inner, claims: map[string]string{}}
}

func (j *interceptingJournal) Claim(want int, owner string) ([]journal.ClaimedRecord, error) {
	claimed, err := j.Journal.Claim(want, owner)
	for _, claim := range claimed {
		j.claims[claim.Record.RecordID] = claim.Token
	}
	return claimed, err
}

func (j *interceptingJournal) RenewClaims(claims []journal.CommitClaim) ([]journal.ClaimedRecord, error) {
	for _, claim := range claims {
		if j.stale != "" && claim.RecordID == j.stale {
			return nil, journal.ErrStaleClaim
		}
	}
	return j.Journal.RenewClaims(claims)
}

func (j *interceptingJournal) held() []string {
	held := make([]string, 0, len(j.claims))
	for recordID := range j.claims {
		held = append(held, recordID)
	}
	sort.Strings(held)
	return held
}

// A database commit followed by a lost MarkCommittedBatch response leaves the
// coordinator holding a claim the journal has already retired. That one member
// must not reject the renewal of its cohort: the journal still holds valid
// claims for the siblings, and dropping them locally stalls the work for a full
// claim TTL, costs every record an extra attempt, and unpins their payloads for
// that window.
func TestALostCommitResponseDoesNotForfeitItsHealthySiblings(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	base := openJournal(t, clock, policy)
	j := newInterceptingJournal(base)
	store := &replaceableUnitStore{err: persistence.ErrUnavailable}
	service := newService(t, clock, policy, j, store)
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	const records = 5
	for i := 0; i < records; i++ {
		record := producer.PaymentError(otlpgen.AtEventTime(uint64(fakeclock.Origin.Add(time.Duration(i) * time.Second).UnixNano())))
		if _, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, producer.Request(record)), admission.EncodingIdentity); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(2*time.Minute + 5*time.Second)
	if _, err := service.Process(context.Background(), 100); !errors.Is(err, persistence.ErrUnavailable) {
		t.Fatalf("first attempt error=%v, want the outage", err)
	}
	held := j.held()
	if len(held) != records {
		t.Fatalf("setup claimed %d records, want %d", len(held), records)
	}
	lost := held[0]
	if err := base.MarkCommitted(lost, j.claims[lost]); err != nil {
		t.Fatalf("simulate a lost commit response: %v", err)
	}

	clock.Advance(journal.DefaultClaimTTL/2 + time.Second)
	if _, err := service.Process(context.Background(), 100); !errors.Is(err, persistence.ErrUnavailable) {
		t.Fatalf("renewal cycle error=%v, want the outage to continue; a nil error means "+
			"the cohort was dropped locally and no work was attempted at all", err)
	}

	store.err = nil
	result, err := service.Process(context.Background(), 100)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if result.Claimed != 0 {
		t.Fatalf("the cohort was forfeited and re-claimed instead of held: %+v", result)
	}
	if result.Processed != records {
		t.Fatalf("recovery processed %d of %d records", result.Processed, records)
	}
	stats, err := j.Stats()
	if err != nil || stats.Committed != records || stats.Claimed != 0 || stats.Pending != 0 {
		t.Fatalf("a lost commit response cost the cohort its claims: stats=%+v err=%v", stats, err)
	}
}

// One genuinely stale member is the journal's answer about that record alone.
// Its siblings keep their valid claims, so the coordinator has to fail per
// record instead of forfeiting the whole cohort.
func TestOneStaleMemberDoesNotForfeitItsHealthySiblings(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := newInterceptingJournal(openJournal(t, clock, policy))
	store := &replaceableUnitStore{err: persistence.ErrUnavailable}
	service := newService(t, clock, policy, j, store)
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	const records = 5
	for i := 0; i < records; i++ {
		record := producer.PaymentError(otlpgen.AtEventTime(uint64(fakeclock.Origin.Add(time.Duration(i) * time.Second).UnixNano())))
		if _, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, producer.Request(record)), admission.EncodingIdentity); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(2*time.Minute + 5*time.Second)
	if _, err := service.Process(context.Background(), 100); !errors.Is(err, persistence.ErrUnavailable) {
		t.Fatalf("first attempt error=%v, want the outage", err)
	}
	held := j.held()
	if len(held) != records {
		t.Fatalf("setup claimed %d records, want %d", len(held), records)
	}
	j.stale = held[0]

	clock.Advance(journal.DefaultClaimTTL/2 + time.Second)
	if _, err := service.Process(context.Background(), 100); !errors.Is(err, persistence.ErrUnavailable) {
		t.Fatalf("renewal cycle error=%v, want the outage to continue; a nil error means "+
			"the cohort was dropped locally and no work was attempted at all", err)
	}

	store.err = nil
	result, err := service.Process(context.Background(), 100)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if result.Claimed != 0 {
		t.Fatalf("the healthy siblings were forfeited and re-claimed: %+v", result)
	}
	if result.Processed != records-1 {
		t.Fatalf("one stale member cost %d healthy siblings their attempt: %+v", records-1-result.Processed, result)
	}
	stats, err := j.Stats()
	if err != nil || stats.Committed != records-1 {
		t.Fatalf("one stale member forfeited its cohort: stats=%+v err=%v", stats, err)
	}
}
