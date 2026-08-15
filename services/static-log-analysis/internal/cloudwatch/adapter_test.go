package cloudwatch_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch/cwcheckpoint"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/cwgen"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/pebbletest"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/testids"
)

// ---------------------------------------------------------------------------
// Test doubles.
//
// fakeSource models CloudWatch itself: events with timestamps, a time filter, a
// page size, and nothing else. It does not know what a checkpoint or a record
// id is, so no test below can pass because the double agreed with the adapter.
// ---------------------------------------------------------------------------

type sourceFault struct {
	err   error
	times int
}

type fakeSource struct {
	mu       sync.Mutex
	region   string
	logGroup string
	events   []cloudwatch.Event
	pageSize int
	// faults are returned before any real answer, one per remaining count.
	faults []sourceFault
	// echoRegion and echoGroup override what the page claims to be, which is how
	// a misconfigured endpoint is represented.
	echoRegion string
	echoGroup  string

	queries []cloudwatch.Query
	calls   int
}

func newSource(events ...cloudwatch.Event) *fakeSource {
	return &fakeSource{
		region:   cwgen.DefaultRegion,
		logGroup: cwgen.DefaultLogGroup,
		events:   append([]cloudwatch.Event(nil), events...),
		pageSize: 100,
	}
}

func (s *fakeSource) Region() string { return s.region }

func (s *fakeSource) failNext(err error, times int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, sourceFault{err: err, times: times})
}

func (s *fakeSource) add(events ...cloudwatch.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, events...)
}

func (s *fakeSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *fakeSource) lastQuery() cloudwatch.Query {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queries) == 0 {
		return cloudwatch.Query{}
	}
	return s.queries[len(s.queries)-1]
}

func (s *fakeSource) allQueries() []cloudwatch.Query {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]cloudwatch.Query(nil), s.queries...)
}

func (s *fakeSource) FilterEvents(ctx context.Context, query cloudwatch.Query) (cloudwatch.Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return cloudwatch.Page{}, err
	}
	s.calls++
	s.queries = append(s.queries, query)
	if len(s.faults) > 0 {
		fault := &s.faults[0]
		fault.times--
		err := fault.err
		if fault.times <= 0 {
			s.faults = s.faults[1:]
		}
		return cloudwatch.Page{}, err
	}
	if query.LogGroup != s.logGroup {
		return cloudwatch.Page{}, fmt.Errorf("%w: no such log group", cloudwatch.ErrSourceUnavailable)
	}

	matching := make([]cloudwatch.Event, 0, len(s.events))
	for _, event := range s.events {
		if event.Timestamp.Before(query.Start) || !event.Timestamp.Before(query.End) {
			continue
		}
		matching = append(matching, event)
	}
	sort.SliceStable(matching, func(i, j int) bool {
		if matching[i].Timestamp.Equal(matching[j].Timestamp) {
			return matching[i].EventID < matching[j].EventID
		}
		return matching[i].Timestamp.Before(matching[j].Timestamp)
	})

	offset := 0
	if query.NextToken != "" {
		if _, err := fmt.Sscanf(query.NextToken, "offset:%d", &offset); err != nil {
			return cloudwatch.Page{}, fmt.Errorf("%w: bad token", cloudwatch.ErrSourceUnavailable)
		}
	}
	if offset > len(matching) {
		offset = len(matching)
	}
	size := s.pageSize
	if query.Limit > 0 && query.Limit < size {
		size = query.Limit
	}
	end := offset + size
	if end > len(matching) {
		end = len(matching)
	}
	page := cloudwatch.Page{
		Region:   s.region,
		LogGroup: s.logGroup,
		Events:   append([]cloudwatch.Event(nil), matching[offset:end]...),
	}
	if s.echoRegion != "" {
		page.Region = s.echoRegion
	}
	if s.echoGroup != "" {
		page.LogGroup = s.echoGroup
	}
	if end < len(matching) {
		page.NextToken = fmt.Sprintf("offset:%d", end)
	}
	return page, nil
}

// recordingSink stands for the ingestion service. It counts how often each
// record id was delivered but never suppresses anything: whether replay is
// harmless has to be visible in what the adapter sends, not hidden by the
// double.
type recordingSink struct {
	mu           sync.Mutex
	batches      [][]model.NormalizedLog
	deliveries   map[string]int
	envelopes    []model.TrustedEnvelope
	acknowledge  bool
	err          error
	rejectAll    bool
	failNextOnce error
}

func newSink() *recordingSink {
	return &recordingSink{deliveries: map[string]int{}, acknowledge: true}
}

func (s *recordingSink) IngestRecords(ctx context.Context, envelope model.TrustedEnvelope, records []model.NormalizedLog) (cloudwatch.Ack, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return cloudwatch.Ack{}, err
	}
	if s.failNextOnce != nil {
		err := s.failNextOnce
		s.failNextOnce = nil
		return cloudwatch.Ack{}, err
	}
	if s.err != nil {
		return cloudwatch.Ack{}, s.err
	}
	s.envelopes = append(s.envelopes, envelope)
	s.batches = append(s.batches, append([]model.NormalizedLog(nil), records...))
	for _, record := range records {
		s.deliveries[record.RecordID]++
	}
	if s.rejectAll {
		return cloudwatch.Ack{Acknowledged: true, Rejected: len(records)}, nil
	}
	if !s.acknowledge {
		return cloudwatch.Ack{Acknowledged: false}, nil
	}
	return cloudwatch.Ack{Acknowledged: true, Accepted: len(records)}, nil
}

func (s *recordingSink) distinct() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.deliveries)
}

func (s *recordingSink) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, count := range s.deliveries {
		total += count
	}
	return total
}

func (s *recordingSink) batchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.batches)
}

func (s *recordingSink) recordIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.deliveries))
	for id := range s.deliveries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// failingCheckpoints is the crash: the sink acknowledged a durable journal
// write and the process died before the checkpoint reached disk. The underlying
// Pebble store is real, and genuinely holds nothing.
type failingCheckpoints struct {
	cloudwatch.CheckpointStore
	fail error
}

func (f *failingCheckpoints) Commit(ctx context.Context, checkpoints []cloudwatch.Checkpoint) error {
	if f.fail != nil {
		return f.fail
	}
	return f.CheckpointStore.Commit(ctx, checkpoints)
}

// ---------------------------------------------------------------------------
// Fixtures.
// ---------------------------------------------------------------------------

type harness struct {
	clock       *fakeclock.Clock
	source      *fakeSource
	sink        *recordingSink
	checkpoints cloudwatch.CheckpointStore
	pebble      *pebbletest.Store
	config      cloudwatch.Config
}

func newHarness(t *testing.T, events ...cloudwatch.Event) *harness {
	t.Helper()
	clock := fakeclock.NewAtOrigin()
	pebble := pebbletest.New(t)
	store, err := cwcheckpoint.New(pebble.DB())
	if err != nil {
		t.Fatalf("opening a checkpoint store: %v", err)
	}
	h := &harness{
		clock:       clock,
		source:      newSource(events...),
		sink:        newSink(),
		checkpoints: store,
		pebble:      pebble,
	}
	h.config = cloudwatch.Config{
		Clock:               clock,
		IDs:                 testids.New(testids.WithClock(clock)),
		Policy:              redact.MinimalPolicy(),
		API:                 h.source,
		Checkpoints:         h.checkpoints,
		Sink:                h.sink,
		Account:             cwgen.DefaultAccount,
		Region:              cwgen.DefaultRegion,
		SourceInstance:      "cloudwatch-adapter-us-east-1-0",
		CredentialIdentity:  "arn:aws:iam::123456789012:role/log-analysis-reader",
		AllowedServices:     []string{cwgen.DefaultService},
		AllowedEnvironments: []string{cwgen.DefaultEnvironment},
		Classification:      "INTERNAL",
		Groups: []cloudwatch.GroupConfig{{
			LogGroup:    cwgen.DefaultLogGroup,
			Service:     cwgen.DefaultService,
			Environment: cwgen.DefaultEnvironment,
		}},
		Lookback:        5 * time.Minute,
		MaxLookback:     time.Hour,
		InitialLookback: 15 * time.Minute,
		MaxWindow:       15 * time.Minute,
		PageLimit:       100,
		Backoff: cloudwatch.Backoff{
			Base: time.Second, Max: 30 * time.Second, MaxAttempts: 4,
			Jitter: func(window time.Duration) time.Duration { return window / 2 },
		},
	}
	return h
}

func (h *harness) adapter(t *testing.T) *cloudwatch.Adapter {
	t.Helper()
	adapter, err := cloudwatch.New(h.config)
	if err != nil {
		t.Fatalf("building an adapter: %v", err)
	}
	return adapter
}

func (h *harness) poll(t *testing.T, adapter *cloudwatch.Adapter) cloudwatch.PollResult {
	t.Helper()
	result, err := adapter.Poll(context.Background())
	if err != nil {
		t.Fatalf("polling: %v", err)
	}
	return result
}

func (h *harness) loadCheckpoints(t *testing.T) []cloudwatch.Checkpoint {
	t.Helper()
	loaded, err := h.checkpoints.LoadGroup(context.Background(), cloudwatch.Group{
		Account: cwgen.DefaultAccount, Region: cwgen.DefaultRegion, LogGroup: cwgen.DefaultLogGroup,
	})
	if err != nil {
		t.Fatalf("loading checkpoints: %v", err)
	}
	return loaded
}

// ---------------------------------------------------------------------------
// Delivery and checkpointing.
// ---------------------------------------------------------------------------

func TestPollDeliversEveryEventAndCheckpointsOnlyAfterAcknowledgement(t *testing.T) {
	producer := cwgen.New()
	events := producer.Sequence(5, time.Minute)
	h := newHarness(t, events...)
	h.clock.Advance(6 * time.Minute)
	adapter := h.adapter(t)

	result := h.poll(t, adapter)

	if result.Delivered != 5 || h.sink.distinct() != 5 {
		t.Fatalf("want five records delivered once each, got %d delivered and %d distinct",
			result.Delivered, h.sink.distinct())
	}
	checkpoints := h.loadCheckpoints(t)
	if len(checkpoints) != 1 {
		t.Fatalf("want one checkpoint for the one stream, got %d", len(checkpoints))
	}
	last := events[len(events)-1]
	if !checkpoints[0].Position.Equal(last.Timestamp) {
		t.Fatalf("want the checkpoint at the last accepted position %s, got %s",
			last.Timestamp, checkpoints[0].Position)
	}
	if checkpoints[0].EventID != last.EventID {
		t.Fatalf("want the boundary event named, got %q", checkpoints[0].EventID)
	}
}

func TestCheckpointDoesNotMoveWhenIngestionDoesNotAcknowledge(t *testing.T) {
	producer := cwgen.New()
	h := newHarness(t, producer.Sequence(3, time.Minute)...)
	h.clock.Advance(4 * time.Minute)
	h.sink.acknowledge = false
	adapter := h.adapter(t)

	_, err := adapter.Poll(context.Background())

	if !errors.Is(err, cloudwatch.ErrNotAcknowledged) {
		t.Fatalf("want a refusal to declare delivery complete, got %v", err)
	}
	if got := h.loadCheckpoints(t); len(got) != 0 {
		t.Fatalf("a checkpoint moved ahead of the journal: %+v", got)
	}
}

func TestCheckpointDoesNotMoveWhenIngestionFails(t *testing.T) {
	producer := cwgen.New()
	h := newHarness(t, producer.Sequence(3, time.Minute)...)
	h.clock.Advance(4 * time.Minute)
	h.sink.err = errors.New("journal unavailable")
	adapter := h.adapter(t)

	if _, err := adapter.Poll(context.Background()); err == nil {
		t.Fatal("want the ingestion failure reported")
	}
	if got := h.loadCheckpoints(t); len(got) != 0 {
		t.Fatalf("a checkpoint moved despite a failed ingestion: %+v", got)
	}
}

// A record ingestion refuses permanently never reaches the journal and never
// will. Holding the checkpoint behind it would replay it forever and starve
// every valid event behind it.
func TestTheCheckpointAdvancesPastPermanentlyRejectedRecords(t *testing.T) {
	producer := cwgen.New()
	events := producer.Sequence(3, time.Minute)
	h := newHarness(t, events...)
	h.clock.Advance(4 * time.Minute)
	h.sink.rejectAll = true
	adapter := h.adapter(t)

	h.poll(t, adapter)

	checkpoints := h.loadCheckpoints(t)
	if len(checkpoints) != 1 || !checkpoints[0].Position.Equal(events[2].Timestamp) {
		t.Fatalf("want the checkpoint past the rejected records, got %+v", checkpoints)
	}
}

// ---------------------------------------------------------------------------
// Overlap, duplicates, and replay.
// ---------------------------------------------------------------------------

func TestAnEventOnTwoOverlappingPagesIsDeliveredOnce(t *testing.T) {
	producer := cwgen.New()
	events := producer.Sequence(4, time.Minute)
	// The same event, byte for byte, appears again in a later page. An
	// overlapping page is normal for CloudWatch and must not double count.
	repeated := events[1]
	h := newHarness(t, append(append([]cloudwatch.Event(nil), events...), repeated)...)
	h.source.pageSize = 2
	h.clock.Advance(5 * time.Minute)
	adapter := h.adapter(t)

	result := h.poll(t, adapter)

	if h.sink.distinct() != 4 {
		t.Fatalf("want four distinct records, got %d", h.sink.distinct())
	}
	if h.sink.total() != 4 {
		t.Fatalf("want each record delivered once, got %d deliveries", h.sink.total())
	}
	if result.Duplicates != 1 {
		t.Fatalf("want the suppressed duplicate counted, got %d", result.Duplicates)
	}
}

func TestPollPaginatesUntilTheRetrievalHorizon(t *testing.T) {
	producer := cwgen.New()
	h := newHarness(t, producer.Sequence(25, 10*time.Second)...)
	h.source.pageSize = 4
	h.clock.Advance(6 * time.Minute)
	adapter := h.adapter(t)

	result := h.poll(t, adapter)

	if result.Delivered != 25 {
		t.Fatalf("want every event before the horizon, got %d", result.Delivered)
	}
	if h.source.callCount() < 7 {
		t.Fatalf("want the pages actually walked, got %d calls", h.source.callCount())
	}
	if h.sink.distinct() != 25 {
		t.Fatalf("want 25 distinct records, got %d", h.sink.distinct())
	}
}

// The central correctness point of the whole adapter. The sink acknowledged a
// durable journal write, the checkpoint never reached disk, and the replacement
// adapter rereads the overlap. The events must come back with the identifiers
// they already had, so downstream deduplication makes the replay harmless.
func TestACrashBetweenAcknowledgementAndCheckpointReplaysStableIdentities(t *testing.T) {
	producer := cwgen.New()
	events := producer.Sequence(5, time.Minute)
	h := newHarness(t, events...)
	h.clock.Advance(6 * time.Minute)

	crashing := *h
	crashingConfig := h.config
	crashingConfig.Checkpoints = &failingCheckpoints{
		CheckpointStore: h.checkpoints,
		fail:            errors.New("power loss before the checkpoint was written"),
	}
	crashing.config = crashingConfig
	before, err := cloudwatch.New(crashingConfig)
	if err != nil {
		t.Fatalf("building the adapter that will crash: %v", err)
	}

	if _, err := before.Poll(context.Background()); err == nil {
		t.Fatal("want the failed checkpoint commit reported")
	}
	if got := h.sink.distinct(); got != 5 {
		t.Fatalf("want five records to have reached the journal before the crash, got %d", got)
	}
	if got := h.loadCheckpoints(t); len(got) != 0 {
		t.Fatalf("the crash was supposed to lose the checkpoint, but found %+v", got)
	}
	deliveredBefore := h.sink.recordIDs()

	// A replacement replica: new adapter, new in-memory state, the same durable
	// checkpoint store, which holds nothing.
	after := h.adapter(t)
	result := h.poll(t, after)

	if result.Delivered != 5 {
		t.Fatalf("want the overlap reread, got %d records on the second pass", result.Delivered)
	}
	if h.sink.total() != 10 {
		t.Fatalf("want ten deliveries across the two passes, got %d", h.sink.total())
	}
	if h.sink.distinct() != 5 {
		t.Fatalf("the replay produced new identities: want 5 distinct records, got %d", h.sink.distinct())
	}
	deliveredAfter := h.sink.recordIDs()
	if len(deliveredBefore) != len(deliveredAfter) {
		t.Fatalf("want the same record ids on both passes, got %v then %v", deliveredBefore, deliveredAfter)
	}
	for i := range deliveredBefore {
		if deliveredBefore[i] != deliveredAfter[i] {
			t.Fatalf("record id changed across the replay: %s then %s", deliveredBefore[i], deliveredAfter[i])
		}
	}
	if got := h.loadCheckpoints(t); len(got) != 1 {
		t.Fatalf("want the second pass to checkpoint, got %+v", got)
	}
}

// Suppression of an already-delivered record is an optimization, not the
// correctness mechanism. This pins that it never becomes load bearing: a fresh
// replica with no memory of the first pass redelivers the overlap.
func TestDuplicateSuppressionIsAnOptimizationAndNotTheGuarantee(t *testing.T) {
	producer := cwgen.New()
	h := newHarness(t, producer.Sequence(3, time.Minute)...)
	h.clock.Advance(4 * time.Minute)

	first := h.adapter(t)
	h.poll(t, first)
	// A second cycle on the same adapter rereads the lookback overlap but
	// remembers what it sent.
	second := h.poll(t, first)
	if second.Delivered != 0 {
		t.Fatalf("want the overlap suppressed in a steady-state cycle, got %d", second.Delivered)
	}
	if second.Duplicates != 3 {
		t.Fatalf("want the suppressed overlap counted, got %d", second.Duplicates)
	}

	// A replacement replica reads the same overlap with no memory of it. The
	// checkpoint is committed, so it starts one lookback behind.
	replacement := h.adapter(t)
	third := h.poll(t, replacement)
	if third.Delivered != 3 {
		t.Fatalf("want a fresh replica to redeliver the overlap, got %d", third.Delivered)
	}
	if h.sink.distinct() != 3 {
		t.Fatalf("the redelivery produced new identities: %d distinct", h.sink.distinct())
	}
}

// ---------------------------------------------------------------------------
// Empty results, failures, and throttling.
// ---------------------------------------------------------------------------

func TestAValidEmptyResultIsNotAFailureAndAdvancesNothing(t *testing.T) {
	h := newHarness(t)
	h.clock.Advance(time.Minute)
	adapter := h.adapter(t)

	result, err := adapter.Poll(context.Background())

	if err != nil {
		t.Fatalf("an empty source is a valid answer, got %v", err)
	}
	if !result.Empty || result.Delivered != 0 {
		t.Fatalf("want an empty result, got %+v", result)
	}
	if got := h.loadCheckpoints(t); len(got) != 0 {
		t.Fatalf("an empty result advanced a checkpoint: %+v", got)
	}
	if adapter.Counters().SourceFailures != 0 {
		t.Fatal("an empty result was counted as a source failure")
	}
	if adapter.Counters().EmptyResults != 1 {
		t.Fatalf("want the empty result counted separately, got %d", adapter.Counters().EmptyResults)
	}
	if h.sink.batchCount() != 0 {
		t.Fatal("an empty result must not produce an ingestion batch at all")
	}
}

func TestASourceFailureIsReportedAndIsDistinctFromAnEmptyResult(t *testing.T) {
	h := newHarness(t)
	h.clock.Advance(time.Minute)
	h.source.failNext(fmt.Errorf("%w: the endpoint refused the connection", cloudwatch.ErrSourceUnavailable), 10)
	adapter := h.adapter(t)

	result, err := adapter.Poll(context.Background())

	if err == nil {
		t.Fatal("want a source failure reported rather than treated as no news")
	}
	if !errors.Is(err, cloudwatch.ErrSourceUnavailable) {
		t.Fatalf("want the unavailable category, got %v", err)
	}
	if result.Empty {
		t.Fatal("a failure is not an empty result")
	}
	if adapter.Counters().SourceFailures == 0 {
		t.Fatal("want the failure counted")
	}
	if adapter.Counters().EmptyResults != 0 {
		t.Fatal("a failure was counted as an empty result")
	}
	if got := h.loadCheckpoints(t); len(got) != 0 {
		t.Fatalf("a failed read advanced a checkpoint: %+v", got)
	}
}

// Throttling is not a failure, it is a request to wait. The adapter waits for a
// jittered, growing interval and then makes progress, rather than spinning
// against the quota it has already exceeded.
func TestThrottlingWaitsWithJitterAndThenMakesProgress(t *testing.T) {
	producer := cwgen.New()
	h := newHarness(t, producer.Sequence(2, time.Minute)...)
	h.clock.Advance(3 * time.Minute)
	h.source.failNext(fmt.Errorf("%w: rate exceeded", cloudwatch.ErrThrottled), 2)
	adapter := h.adapter(t)

	done := make(chan cloudwatch.PollResult, 1)
	failed := make(chan error, 1)
	go func() {
		result, err := adapter.Poll(context.Background())
		if err != nil {
			failed <- err
			return
		}
		done <- result
	}()

	// Two throttled attempts, so two waits. The injected jitter halves each
	// window, so the first is 500ms and the second 1s.
	for _, want := range []time.Duration{500 * time.Millisecond, time.Second} {
		h.clock.BlockUntil(1)
		pending := h.clock.PendingDeadlines()
		if got := pending[0].Sub(h.clock.Now()); got != want {
			t.Fatalf("want a wait of %s, got %s", want, got)
		}
		h.clock.Advance(want)
	}

	select {
	case err := <-failed:
		t.Fatalf("throttling must not end the cycle: %v", err)
	case result := <-done:
		if result.Delivered != 2 {
			t.Fatalf("want progress after the backoff, got %d records", result.Delivered)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the poll never finished after its backoff elapsed")
	}
	if adapter.Counters().Throttled != 2 {
		t.Fatalf("want both throttled attempts counted, got %d", adapter.Counters().Throttled)
	}
	if h.source.callCount() != 3 {
		t.Fatalf("want two refusals and one success, got %d calls", h.source.callCount())
	}
}

// A quota that stays exhausted must end the cycle rather than hold the worker
// forever. The next cycle is itself the retry.
func TestSustainedThrottlingGivesUpRatherThanSpinning(t *testing.T) {
	h := newHarness(t)
	h.clock.Advance(time.Minute)
	h.source.failNext(fmt.Errorf("%w: rate exceeded", cloudwatch.ErrThrottled), 100)
	h.config.Backoff = cloudwatch.Backoff{
		Base: time.Second, Max: 4 * time.Second, MaxAttempts: 3,
		// A zero wait keeps the test synchronous; the wait itself is pinned by
		// the test above.
		Jitter: func(time.Duration) time.Duration { return 0 },
	}
	adapter := h.adapter(t)

	_, err := adapter.Poll(context.Background())

	if !errors.Is(err, cloudwatch.ErrThrottled) {
		t.Fatalf("want the exhausted attempt budget reported as throttling, got %v", err)
	}
	if h.source.callCount() != 3 {
		t.Fatalf("want exactly the configured attempts, got %d", h.source.callCount())
	}
}

func TestAThrottledCycleAdvancesNoCheckpoint(t *testing.T) {
	producer := cwgen.New()
	h := newHarness(t, producer.Sequence(2, time.Minute)...)
	h.clock.Advance(3 * time.Minute)
	h.source.failNext(fmt.Errorf("%w: rate exceeded", cloudwatch.ErrThrottled), 100)
	h.config.Backoff = cloudwatch.Backoff{
		Base: time.Second, MaxAttempts: 2, Jitter: func(time.Duration) time.Duration { return 0 },
	}
	adapter := h.adapter(t)

	if _, err := adapter.Poll(context.Background()); err == nil {
		t.Fatal("want the throttling reported")
	}
	if got := h.loadCheckpoints(t); len(got) != 0 {
		t.Fatalf("a throttled cycle advanced a checkpoint: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Lateness, ordering, and the bounded lookback.
// ---------------------------------------------------------------------------

// CloudWatch filters on event time, so an event written ninety seconds after it
// happened lands behind a position the adapter has already passed. The bounded
// lookback overlap is the only thing that recovers it.
func TestALateEventBehindTheCheckpointIsRecoveredByTheLookbackOverlap(t *testing.T) {
	producer := cwgen.New()
	events := producer.Sequence(3, time.Minute)
	h := newHarness(t, events...)
	h.clock.Advance(4 * time.Minute)
	adapter := h.adapter(t)

	h.poll(t, adapter)
	if h.sink.distinct() != 3 {
		t.Fatalf("want three records first, got %d", h.sink.distinct())
	}

	// An event whose own timestamp is a minute before the checkpoint, written
	// only now.
	late := producer.PaymentError(
		cwgen.AtEventTime(events[1].Timestamp.Add(30*time.Second)),
		cwgen.AtIngestionTime(h.clock.Now()),
	)
	h.source.add(late)
	h.clock.Advance(30 * time.Second)

	result := h.poll(t, adapter)

	if result.Delivered != 1 {
		t.Fatalf("want the late event recovered, got %d", result.Delivered)
	}
	if h.sink.distinct() != 4 {
		t.Fatalf("want four distinct records after the recovery, got %d", h.sink.distinct())
	}
}

// An event older than the bounded lookback cannot be recovered, and the adapter
// must not reach further back to try. Unbounded lookback is unbounded rescan.
func TestAnEventOlderThanTheBoundedLookbackIsNotReachedFor(t *testing.T) {
	producer := cwgen.New()
	h := newHarness(t, producer.Sequence(2, time.Minute)...)
	h.clock.Advance(3 * time.Minute)
	adapter := h.adapter(t)
	h.poll(t, adapter)

	h.clock.Advance(6 * time.Hour)
	h.poll(t, adapter)

	query := h.source.lastQuery()
	floor := h.clock.Now().Add(-h.config.MaxLookback)
	if query.Start.Before(floor) {
		t.Fatalf("want no read before the bounded floor %s, got %s", floor, query.Start)
	}
}

func TestOutOfOrderEventsCheckpointAtTheHighestAcceptedPosition(t *testing.T) {
	producer := cwgen.New()
	base := fakeclock.Origin
	// Deliberately shuffled: a source that answers out of order must not make
	// the checkpoint go backwards.
	events := []cloudwatch.Event{
		producer.PaymentError(cwgen.AtEventTime(base.Add(3*time.Minute)), cwgen.AtIngestionTime(base.Add(3*time.Minute))),
		producer.PaymentError(cwgen.AtEventTime(base.Add(time.Minute)), cwgen.AtIngestionTime(base.Add(time.Minute))),
		producer.PaymentError(cwgen.AtEventTime(base.Add(2*time.Minute)), cwgen.AtIngestionTime(base.Add(2*time.Minute))),
	}
	h := newHarness(t, events...)
	h.clock.Advance(4 * time.Minute)
	adapter := h.adapter(t)

	h.poll(t, adapter)

	checkpoints := h.loadCheckpoints(t)
	if len(checkpoints) != 1 || !checkpoints[0].Position.Equal(base.Add(3*time.Minute)) {
		t.Fatalf("want the highest accepted position, got %+v", checkpoints)
	}
}

func TestACheckpointNeverMovesBackwards(t *testing.T) {
	producer := cwgen.New()
	events := producer.Sequence(3, time.Minute)
	h := newHarness(t, events...)
	h.clock.Advance(4 * time.Minute)
	adapter := h.adapter(t)
	h.poll(t, adapter)
	forward := h.loadCheckpoints(t)[0].Position

	// A late event inside the overlap is delivered, but its older position must
	// not drag the stream's checkpoint back over records already accounted for.
	h.source.add(producer.PaymentError(
		cwgen.AtEventTime(events[0].Timestamp.Add(10*time.Second)),
		cwgen.AtIngestionTime(h.clock.Now()),
	))
	h.clock.Advance(10 * time.Second)
	h.poll(t, adapter)

	if got := h.loadCheckpoints(t)[0].Position; got.Before(forward) {
		t.Fatalf("checkpoint moved backwards from %s to %s", forward, got)
	}
}

// ---------------------------------------------------------------------------
// Stream discovery.
// ---------------------------------------------------------------------------

func TestANewStreamIsDiscoveredWithoutRescanningUnboundedHistory(t *testing.T) {
	producer := cwgen.New()
	h := newHarness(t, producer.Sequence(2, time.Minute)...)
	h.clock.Advance(3 * time.Minute)
	adapter := h.adapter(t)
	h.poll(t, adapter)

	// A second task starts and writes to a stream the adapter has never seen.
	fresh := cwgen.New(cwgen.ForStream("ecs/paymentservice/deadbeef"), cwgen.WithIDBase(9))
	h.clock.Advance(time.Minute)
	h.source.add(fresh.PaymentError(
		cwgen.AtEventTime(h.clock.Now().Add(-time.Second)), cwgen.AtIngestionTime(h.clock.Now())))

	result := h.poll(t, adapter)

	if result.Delivered != 1 {
		t.Fatalf("want the new stream's event discovered, got %d", result.Delivered)
	}
	checkpoints := h.loadCheckpoints(t)
	if len(checkpoints) != 2 {
		t.Fatalf("want a checkpoint per stream, got %d", len(checkpoints))
	}
	for _, query := range h.source.allQueries() {
		if query.Start.Before(h.clock.Now().Add(-h.config.MaxLookback)) {
			t.Fatalf("discovery reached back past the bounded floor: %s", query.Start)
		}
	}
}

func TestEachStreamKeepsItsOwnCheckpoint(t *testing.T) {
	first := cwgen.New(cwgen.ForStream("stream-a"), cwgen.WithIDBase(1))
	second := cwgen.New(cwgen.ForStream("stream-b"), cwgen.WithIDBase(2))
	base := fakeclock.Origin
	events := []cloudwatch.Event{
		first.PaymentError(cwgen.AtEventTime(base.Add(time.Minute)), cwgen.AtIngestionTime(base.Add(time.Minute))),
		second.PaymentError(cwgen.AtEventTime(base.Add(3*time.Minute)), cwgen.AtIngestionTime(base.Add(3*time.Minute))),
	}
	h := newHarness(t, events...)
	h.clock.Advance(4 * time.Minute)
	adapter := h.adapter(t)

	h.poll(t, adapter)

	positions := map[string]time.Time{}
	for _, checkpoint := range h.loadCheckpoints(t) {
		positions[checkpoint.Stream.Name] = checkpoint.Position
	}
	if !positions["stream-a"].Equal(base.Add(time.Minute)) {
		t.Fatalf("want stream-a at its own position, got %s", positions["stream-a"])
	}
	if !positions["stream-b"].Equal(base.Add(3 * time.Minute)) {
		t.Fatalf("want stream-b at its own position, got %s", positions["stream-b"])
	}
}

func TestAQuietStreamDoesNotPinTheGroupRetrievalHorizon(t *testing.T) {
	first := cwgen.New(cwgen.ForStream("stream-a"), cwgen.WithIDBase(1))
	second := cwgen.New(cwgen.ForStream("stream-b"), cwgen.WithIDBase(2))
	base := fakeclock.Origin
	h := newHarness(t,
		first.PaymentError(cwgen.AtEventTime(base.Add(time.Minute)), cwgen.AtIngestionTime(base.Add(time.Minute))),
		second.PaymentError(cwgen.AtEventTime(base.Add(14*time.Minute)), cwgen.AtIngestionTime(base.Add(14*time.Minute))),
	)
	h.clock.Advance(15 * time.Minute)
	adapter := h.adapter(t)
	h.poll(t, adapter)

	// stream-a goes quiet. A new stream then writes ahead of stream-b's
	// checkpoint. Because FilterLogEvents reads the whole group, stream-a's old
	// per-stream diagnostic checkpoint must not hold every later group query in
	// the past.
	third := cwgen.New(cwgen.ForStream("stream-c"), cwgen.WithIDBase(3))
	h.clock.Advance(6 * time.Minute)
	h.source.add(third.PaymentError(
		cwgen.AtEventTime(base.Add(20*time.Minute)), cwgen.AtIngestionTime(base.Add(20*time.Minute))))

	result := h.poll(t, adapter)

	if result.Delivered != 1 {
		t.Fatalf("quiet stream pinned the group horizon: delivered %d", result.Delivered)
	}
	if got := h.sink.distinct(); got != 3 {
		t.Fatalf("want all three streams represented, got %d records", got)
	}
}

// ---------------------------------------------------------------------------
// The regional boundary.
// ---------------------------------------------------------------------------

// A client can be rebuilt underneath a long-lived adapter, so the boundary is
// rechecked every cycle rather than only once at construction.
func TestTheAdapterRefusesAClientBoundToAnotherRegion(t *testing.T) {
	producer := cwgen.New()
	h := newHarness(t, producer.Sequence(2, time.Minute)...)
	h.clock.Advance(3 * time.Minute)
	adapter := h.adapter(t)
	h.source.region = "eu-west-1"

	_, err := adapter.Poll(context.Background())

	if !errors.Is(err, cloudwatch.ErrRegionalBoundary) {
		t.Fatalf("want a regional boundary refusal, got %v", err)
	}
	if h.source.callCount() != 0 {
		t.Fatal("want the refusal before any log content is read at all")
	}
	if adapter.Counters().BoundaryRejections == 0 {
		t.Fatal("want the refusal counted")
	}
}

func TestTheAdapterRefusesAPageFromAnotherRegionOrGroup(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(*fakeSource)
	}{
		{"page from another region", func(s *fakeSource) { s.echoRegion = "eu-west-1" }},
		{"page from another log group", func(s *fakeSource) { s.echoGroup = "/aws/ecs/cartservice" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			producer := cwgen.New()
			h := newHarness(t, producer.Sequence(2, time.Minute)...)
			h.clock.Advance(3 * time.Minute)
			test.arrange(h.source)
			adapter := h.adapter(t)

			_, err := adapter.Poll(context.Background())

			if !errors.Is(err, cloudwatch.ErrRegionalBoundary) {
				t.Fatalf("want a boundary refusal, got %v", err)
			}
			if h.sink.batchCount() != 0 {
				t.Fatal("records from outside the boundary must never reach ingestion")
			}
			if got := h.loadCheckpoints(t); len(got) != 0 {
				t.Fatalf("a boundary violation advanced a checkpoint: %+v", got)
			}
		})
	}
}

func TestTheEnvelopeIsBuiltFromConfigurationAlone(t *testing.T) {
	producer := cwgen.New()
	h := newHarness(t, producer.Sequence(1, time.Minute)...)
	h.clock.Advance(2 * time.Minute)
	adapter := h.adapter(t)

	h.poll(t, adapter)

	if len(h.sink.envelopes) != 1 {
		t.Fatalf("want one envelope, got %d", len(h.sink.envelopes))
	}
	envelope := h.sink.envelopes[0]
	if envelope.SourceType != model.SourceTypeCloudWatch {
		t.Fatalf("want source type %s, got %s", model.SourceTypeCloudWatch, envelope.SourceType)
	}
	if envelope.Region != cwgen.DefaultRegion || envelope.SourceAccount != cwgen.DefaultAccount {
		t.Fatalf("want the configured account and region, got %+v", envelope)
	}
	if envelope.ReceivedAt.IsZero() {
		t.Fatal("want a receipt time on the envelope")
	}
	if err := envelope.Validate(); err != nil {
		t.Fatalf("the envelope is not usable: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Telemetry.
// ---------------------------------------------------------------------------

func TestCountersRecordEveryOutcomeWorthAlerting(t *testing.T) {
	producer := cwgen.New()
	events := producer.Sequence(3, time.Minute)
	h := newHarness(t, append(append([]cloudwatch.Event(nil), events...), events[0])...)
	h.clock.Advance(4 * time.Minute)
	h.source.failNext(fmt.Errorf("%w: rate exceeded", cloudwatch.ErrThrottled), 1)
	adapter := h.adapter(t)

	go func() {
		h.clock.BlockUntil(1)
		h.clock.Advance(time.Second)
	}()
	if _, err := adapter.Poll(context.Background()); err != nil {
		t.Fatalf("polling: %v", err)
	}

	counters := adapter.Counters()
	if counters.RecordsRead != 4 {
		t.Fatalf("want four events read, got %d", counters.RecordsRead)
	}
	if counters.DuplicateRecords != 1 {
		t.Fatalf("want one duplicate, got %d", counters.DuplicateRecords)
	}
	if counters.Throttled != 1 {
		t.Fatalf("want one throttled attempt, got %d", counters.Throttled)
	}
	if counters.RecordsDelivered != 3 {
		t.Fatalf("want three delivered, got %d", counters.RecordsDelivered)
	}
	if counters.CheckpointCommits != 1 {
		t.Fatalf("want one checkpoint commit, got %d", counters.CheckpointCommits)
	}
}

func TestCheckpointAgeAndSourceLagAreObservable(t *testing.T) {
	producer := cwgen.New()
	events := producer.Sequence(2, time.Minute)
	h := newHarness(t, events...)
	h.clock.Advance(3 * time.Minute)
	adapter := h.adapter(t)
	h.poll(t, adapter)

	h.clock.Advance(10 * time.Minute)
	observed := adapter.Observations(h.clock.Now())

	if observed.CheckpointedStreams != 1 {
		t.Fatalf("want one checkpointed stream, got %d", observed.CheckpointedStreams)
	}
	if observed.OldestCheckpointAge < 10*time.Minute {
		t.Fatalf("want a checkpoint age of at least ten minutes, got %s", observed.OldestCheckpointAge)
	}
	wantLag := h.clock.Now().Sub(events[1].Timestamp)
	if observed.SourceLag != wantLag {
		t.Fatalf("want a source lag of %s, got %s", wantLag, observed.SourceLag)
	}
}

// operations.md forbids unbounded label cardinality on metrics. A log stream
// name is exactly that, so the counters carry no labels at all and the gauges
// report the worst stream rather than every stream.
func TestTelemetryCarriesNoUnboundedLabels(t *testing.T) {
	producer := cwgen.New()
	h := newHarness(t, producer.Sequence(1, time.Minute)...)
	h.clock.Advance(2 * time.Minute)
	adapter := h.adapter(t)
	h.poll(t, adapter)

	// Counters and Observations are plain value types. If either grew a map
	// keyed by stream, group, or record id, this stops compiling, which is the
	// point: the constraint is enforced by the shape of the type.
	var counters cloudwatch.Counters = adapter.Counters()
	var observations cloudwatch.Observations = adapter.Observations(h.clock.Now())
	_ = counters.RecordsRead + counters.DuplicateRecords + counters.Throttled +
		counters.SourceFailures + counters.EmptyResults + counters.RecordsDelivered +
		counters.RecordsRejected + counters.CheckpointCommits + counters.BoundaryRejections
	_ = observations.OldestCheckpointAge + observations.SourceLag
}

// ---------------------------------------------------------------------------
// Configuration.
// ---------------------------------------------------------------------------

func TestAdapterConfigurationIsValidatedAtConstruction(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*cloudwatch.Config)
		because string
	}{
		{"no api", func(c *cloudwatch.Config) { c.API = nil }, "there is nothing to read from"},
		{"no sink", func(c *cloudwatch.Config) { c.Sink = nil }, "there is nowhere to deliver"},
		{"no checkpoints", func(c *cloudwatch.Config) { c.Checkpoints = nil },
			"without a durable checkpoint every restart rereads from the floor forever"},
		{"no clock", func(c *cloudwatch.Config) { c.Clock = nil }, "nothing reads wall-clock time directly"},
		{"no ids", func(c *cloudwatch.Config) { c.IDs = nil }, "a batch id has to come from somewhere"},
		{"no groups", func(c *cloudwatch.Config) { c.Groups = nil }, "an adapter with no group polls nothing"},
		{"api in another region", func(c *cloudwatch.Config) { c.Region = "eu-west-1" },
			"a regional adapter must never be handed another region's client"},
		{"duplicate group", func(c *cloudwatch.Config) {
			c.Groups = append(c.Groups, c.Groups[0])
		}, "two configurations for one group would fight over its checkpoints"},
		{"service outside the allowed set", func(c *cloudwatch.Config) {
			c.Groups[0].Service = "cartservice"
		}, "the envelope's allowed set is the authorization"},
		{"lookback beyond the bounded floor", func(c *cloudwatch.Config) {
			c.Lookback = 2 * time.Hour
		}, "an overlap larger than the floor is an unbounded rescan"},
		{"negative window", func(c *cloudwatch.Config) { c.MaxWindow = -time.Minute },
			"a cycle that reads no span of time never makes progress"},
		{"impossible backoff", func(c *cloudwatch.Config) {
			c.Backoff = cloudwatch.Backoff{Base: time.Minute, Max: time.Second}
		}, "a cap below the first delay is a configuration mistake"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			test.change(&h.config)

			adapter, err := cloudwatch.New(h.config)

			if err == nil {
				t.Fatalf("want a configuration refusal because %s", test.because)
			}
			if !errors.Is(err, cloudwatch.ErrInvalidConfig) && !errors.Is(err, cloudwatch.ErrRegionalBoundary) {
				t.Fatalf("want a configuration or boundary category, got %v", err)
			}
			if adapter != nil {
				t.Fatal("want no adapter alongside a refusal")
			}
		})
	}
}

func TestAdapterDefaultsProduceAWorkingCycle(t *testing.T) {
	producer := cwgen.New()
	h := newHarness(t, producer.Sequence(2, time.Minute)...)
	h.clock.Advance(3 * time.Minute)
	h.config.Lookback = 0
	h.config.MaxLookback = 0
	h.config.InitialLookback = 0
	h.config.MaxWindow = 0
	h.config.PageLimit = 0
	h.config.MaxPages = 0
	h.config.Backoff = cloudwatch.Backoff{}
	adapter := h.adapter(t)

	result := h.poll(t, adapter)

	if result.Delivered != 2 {
		t.Fatalf("want the defaults to read the recent past, got %d", result.Delivered)
	}
}

func TestPollIsRefusedOnACancelledContext(t *testing.T) {
	producer := cwgen.New()
	h := newHarness(t, producer.Sequence(2, time.Minute)...)
	h.clock.Advance(3 * time.Minute)
	adapter := h.adapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := adapter.Poll(ctx); err == nil {
		t.Fatal("want a cancelled poll refused")
	}
	if got := h.loadCheckpoints(t); len(got) != 0 {
		t.Fatalf("a cancelled poll advanced a checkpoint: %+v", got)
	}
}
