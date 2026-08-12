package cloudwatch

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/clock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/ids"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
)

// Cycle defaults. Every one of them is a bound rather than a preference: an
// unbounded lookback is an unbounded rescan, and an unbounded page walk is a
// cycle that never ends.
const (
	// DefaultLookback is how far behind its own checkpoint a cycle restarts.
	// CloudWatch filters on event time, so an event written late lands behind a
	// position already passed; this overlap is the only thing that recovers it.
	DefaultLookback = 5 * time.Minute
	// DefaultMaxLookback is the floor a cycle never reads before, however far
	// behind the checkpoint is. Without it a stream quiet for a week would make
	// every cycle rescan a week.
	DefaultMaxLookback = time.Hour
	// DefaultInitialLookback is how much history a stream with no checkpoint
	// yet contributes. A new deployment does not import its own backlog.
	DefaultInitialLookback = 15 * time.Minute
	// DefaultMaxWindow bounds the span one cycle reads, so catching up after an
	// outage happens in bounded steps rather than in one unbounded read.
	DefaultMaxWindow = 15 * time.Minute
	DefaultPageLimit = 1000
	// DefaultMaxPages is a safety valve, not a normal limit. Truncating a
	// window silently would checkpoint past events that were never read, so
	// exhausting it is reported instead.
	DefaultMaxPages = 1000
)

// ErrPageBudgetExhausted means one window held more pages than the budget
// allows. It is reported rather than truncated: a truncated window that still
// checkpointed would skip every event it did not reach.
var ErrPageBudgetExhausted = fmt.Errorf("%w: page budget exhausted before the retrieval horizon", ErrSourceUnavailable)

// GroupConfig is one log group and the identity its records are attributed to.
type GroupConfig struct {
	LogGroup string
	// Service and Environment are the operator's declaration. CloudWatch events
	// carry no authenticated service identity, and a claim inside a message is
	// untrusted, so this is the only place the identity can come from.
	Service     string
	Environment string
}

// Config builds an Adapter.
type Config struct {
	Clock       clock.Clock
	IDs         ids.Source
	Policy      *redact.Policy
	API         LogsAPI
	Checkpoints CheckpointStore
	Sink        Sink

	Account             string
	Region              string
	SourceInstance      string
	CredentialIdentity  string
	AllowedServices     []string
	AllowedEnvironments []string
	Classification      string
	RawReferenceTTL     time.Duration

	Groups []GroupConfig

	Lookback        time.Duration
	MaxLookback     time.Duration
	InitialLookback time.Duration
	MaxWindow       time.Duration
	PageLimit       int
	MaxPages        int
	Backoff         Backoff
}

// Counters are cumulative, categorical, process-lifetime counts.
//
// They carry no labels. operations.md forbids unbounded label cardinality, and
// a log stream name is unbounded: an account can hold hundreds of thousands of
// them, and each one would become a time series that never retires.
type Counters struct {
	// RecordsRead counts native events returned by the source, including ones
	// an overlap returned again.
	RecordsRead uint64
	// DuplicateRecords counts events suppressed because this adapter had
	// already delivered them. Suppression is an optimization; the guarantee is
	// the stable record id.
	DuplicateRecords uint64
	// RecordsDelivered counts records handed to ingestion and acknowledged.
	RecordsDelivered uint64
	// RecordsRejected counts events that could not become records at all.
	RecordsRejected uint64
	// Throttled counts source API attempts refused for rate.
	Throttled uint64
	// SourceFailures counts source API attempts that failed for another reason.
	SourceFailures uint64
	// EmptyResults counts cycles where a group answered validly with nothing.
	// It is deliberately not a failure: telling them apart is a requirement.
	EmptyResults uint64
	// CheckpointCommits counts durable checkpoint writes.
	CheckpointCommits uint64
	// BoundaryRejections counts reads refused because they would have crossed a
	// regional or account boundary.
	BoundaryRejections uint64
}

type counters struct {
	recordsRead        atomic.Uint64
	duplicateRecords   atomic.Uint64
	recordsDelivered   atomic.Uint64
	recordsRejected    atomic.Uint64
	throttled          atomic.Uint64
	sourceFailures     atomic.Uint64
	emptyResults       atomic.Uint64
	checkpointCommits  atomic.Uint64
	boundaryRejections atomic.Uint64
}

// Observations are the gauges an operator alerts on.
//
// Both are the worst value across streams rather than a value per stream, for
// the same cardinality reason the counters carry no labels. The worst stream is
// the one an alert is about.
type Observations struct {
	// OldestCheckpointAge is how long ago the least recently committed
	// checkpoint was written. A checkpoint that stops ageing is an adapter that
	// has stopped making progress.
	OldestCheckpointAge time.Duration
	// SourceLag is how far behind the present the furthest-behind stream is.
	SourceLag           time.Duration
	CheckpointedStreams int
}

// PollResult is what one cycle did.
type PollResult struct {
	Groups     int
	Pages      int
	EventsRead int
	// Duplicates counts events this adapter had already delivered.
	Duplicates int
	Delivered  int
	Rejected   int
	// Checkpoints counts streams whose position advanced.
	Checkpoints int
	// Empty is true when every group answered validly with nothing new. It is
	// not an error and it advances nothing.
	Empty bool
}

// groupState is one configured group: its identity, its mapper, and what this
// process has already delivered from it.
type groupState struct {
	group  Group
	mapper *Mapper
	// delivered remembers record ids already handed to ingestion, so a steady
	// state cycle does not resend its whole overlap every time. It is an
	// optimization only: a replacement replica has none of it and redelivers,
	// which is exactly what makes the crash case safe.
	delivered map[string]time.Time
	// positions is the last committed position per stream, kept so the metrics
	// do not have to reread the store.
	positions   map[string]Checkpoint
	loadedOnce  bool
	commitCount int
}

// Adapter reads one region's CloudWatch log groups.
type Adapter struct {
	clock       clock.Clock
	ids         ids.Source
	api         LogsAPI
	checkpoints CheckpointStore
	sink        Sink

	region          string
	lookback        time.Duration
	maxLookback     time.Duration
	initialLookback time.Duration
	maxWindow       time.Duration
	pageLimit       int
	maxPages        int
	backoff         Backoff

	mu     sync.Mutex
	groups []*groupState

	counters counters
}

// New validates everything before returning an adapter. A regional or identity
// misconfiguration caught here is one error; caught per record it would look
// like every record being individually invalid.
func New(config Config) (*Adapter, error) {
	if nilInterface(config.Clock) || nilInterface(config.IDs) || nilInterface(config.API) ||
		nilInterface(config.Checkpoints) || nilInterface(config.Sink) {
		return nil, fmt.Errorf("%w: a clock, id source, source api, checkpoint store, and sink are all required", ErrInvalidConfig)
	}
	if len(config.Groups) == 0 {
		return nil, fmt.Errorf("%w: an adapter with no log group polls nothing", ErrInvalidConfig)
	}
	if !trimmed(config.Region) {
		return nil, fmt.Errorf("%w: a regional adapter needs its region", ErrInvalidConfig)
	}
	// The client's own region is checked before anything else can use it. A
	// replica handed another region's client would otherwise read that region's
	// logs and store them under this region's boundary.
	if config.API.Region() != config.Region {
		return nil, fmt.Errorf("%w: the source client is bound to %q, not %q",
			ErrRegionalBoundary, config.API.Region(), config.Region)
	}
	if err := config.Backoff.Validate(); err != nil {
		return nil, err
	}

	lookback := orDefault(config.Lookback, DefaultLookback)
	maxLookback := orDefault(config.MaxLookback, DefaultMaxLookback)
	initialLookback := orDefault(config.InitialLookback, DefaultInitialLookback)
	maxWindow := orDefault(config.MaxWindow, DefaultMaxWindow)
	for name, value := range map[string]time.Duration{
		"lookback": config.Lookback, "max lookback": config.MaxLookback,
		"initial lookback": config.InitialLookback, "max window": config.MaxWindow,
	} {
		if value < 0 {
			return nil, fmt.Errorf("%w: %s cannot be negative", ErrInvalidConfig, name)
		}
	}
	if lookback > maxLookback {
		return nil, fmt.Errorf("%w: an overlap larger than the bounded floor is an unbounded rescan", ErrInvalidConfig)
	}
	if initialLookback > maxLookback {
		return nil, fmt.Errorf("%w: a first read further back than the bounded floor is an unbounded rescan", ErrInvalidConfig)
	}
	pageLimit := config.PageLimit
	if pageLimit <= 0 {
		pageLimit = DefaultPageLimit
	}
	maxPages := config.MaxPages
	if maxPages <= 0 {
		maxPages = DefaultMaxPages
	}

	adapter := &Adapter{
		clock: config.Clock, ids: config.IDs, api: config.API,
		checkpoints: config.Checkpoints, sink: config.Sink, region: config.Region,
		lookback: lookback, maxLookback: maxLookback, initialLookback: initialLookback,
		maxWindow: maxWindow, pageLimit: pageLimit, maxPages: maxPages, backoff: config.Backoff,
	}
	seen := map[string]bool{}
	for _, group := range config.Groups {
		if !trimmed(group.LogGroup) {
			return nil, fmt.Errorf("%w: a group configuration needs a log group name", ErrInvalidConfig)
		}
		if seen[group.LogGroup] {
			// Two configurations for one group would fight over its
			// checkpoints, each moving the other's position.
			return nil, fmt.Errorf("%w: log group %q is configured twice", ErrInvalidConfig, group.LogGroup)
		}
		seen[group.LogGroup] = true
		mapper, err := NewMapper(MapperConfig{
			Policy:              config.Policy,
			Account:             config.Account,
			Region:              config.Region,
			SourceInstance:      config.SourceInstance,
			CredentialIdentity:  config.CredentialIdentity,
			AllowedServices:     config.AllowedServices,
			AllowedEnvironments: config.AllowedEnvironments,
			Service:             group.Service,
			Environment:         group.Environment,
			Classification:      config.Classification,
			RawReferenceTTL:     config.RawReferenceTTL,
		})
		if err != nil {
			return nil, err
		}
		adapter.groups = append(adapter.groups, &groupState{
			group:     Group{Account: config.Account, Region: config.Region, LogGroup: group.LogGroup},
			mapper:    mapper,
			delivered: map[string]time.Time{},
			positions: map[string]Checkpoint{},
		})
	}
	return adapter, nil
}

// Counters returns a snapshot.
func (a *Adapter) Counters() Counters {
	if a == nil {
		return Counters{}
	}
	return Counters{
		RecordsRead:        a.counters.recordsRead.Load(),
		DuplicateRecords:   a.counters.duplicateRecords.Load(),
		RecordsDelivered:   a.counters.recordsDelivered.Load(),
		RecordsRejected:    a.counters.recordsRejected.Load(),
		Throttled:          a.counters.throttled.Load(),
		SourceFailures:     a.counters.sourceFailures.Load(),
		EmptyResults:       a.counters.emptyResults.Load(),
		CheckpointCommits:  a.counters.checkpointCommits.Load(),
		BoundaryRejections: a.counters.boundaryRejections.Load(),
	}
}

// Observations returns checkpoint age and source lag as of now.
func (a *Adapter) Observations(now time.Time) Observations {
	if a == nil {
		return Observations{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	observed := Observations{}
	for _, state := range a.groups {
		for _, checkpoint := range state.positions {
			observed.CheckpointedStreams++
			if age := checkpoint.Age(now); age > observed.OldestCheckpointAge {
				observed.OldestCheckpointAge = age
			}
			if lag := now.Sub(checkpoint.Position); lag > observed.SourceLag {
				observed.SourceLag = lag
			}
		}
	}
	return observed
}

// Poll runs one cycle over every configured group.
//
// The ordering inside a group is the contract: read, map, deliver, wait for a
// durable journal acknowledgement, and only then move the checkpoint. A failure
// anywhere before the acknowledgement leaves every position exactly where it
// was, and the next cycle rereads.
//
// The first group that fails ends the cycle. The result returned alongside the
// error describes the work that did complete, so counters stay truthful.
func (a *Adapter) Poll(ctx context.Context) (PollResult, error) {
	if a == nil {
		return PollResult{}, fmt.Errorf("%w: adapter is not built", ErrInvalidConfig)
	}
	if ctx == nil {
		return PollResult{}, fmt.Errorf("%w: a poll needs a context", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return PollResult{}, err
	}
	// Rechecked every cycle rather than only at construction, because a client
	// can be rebuilt underneath a long-lived adapter.
	if a.api.Region() != a.region {
		a.counters.boundaryRejections.Add(1)
		return PollResult{}, fmt.Errorf("%w: the source client is bound to %q, not %q",
			ErrRegionalBoundary, a.api.Region(), a.region)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	result := PollResult{Empty: true}
	for _, state := range a.groups {
		groupResult, err := a.pollGroup(ctx, state)
		result.Groups++
		result.Pages += groupResult.Pages
		result.EventsRead += groupResult.EventsRead
		result.Duplicates += groupResult.Duplicates
		result.Delivered += groupResult.Delivered
		result.Rejected += groupResult.Rejected
		result.Checkpoints += groupResult.Checkpoints
		if groupResult.EventsRead > 0 {
			result.Empty = false
		}
		if err != nil {
			result.Empty = false
			return result, err
		}
	}
	return result, nil
}

// readEvent is one event together with the identity it was given, kept
// together so that the checkpoint and the record never disagree about which
// stream an event belonged to.
type readEvent struct {
	event    Event
	recordID string
}

func (a *Adapter) pollGroup(ctx context.Context, state *groupState) (PollResult, error) {
	result := PollResult{}
	now := a.clock.Now().UTC()

	checkpoints, err := a.loadCheckpoints(ctx, state)
	if err != nil {
		return result, err
	}
	start, end := a.window(checkpoints, now)
	a.forget(state, start)

	events, pages, err := a.read(ctx, state.group, start, end)
	result.Pages = pages
	if err != nil {
		return result, err
	}
	result.EventsRead = len(events)
	a.counters.recordsRead.Add(uint64(len(events)))
	if len(events) == 0 {
		// A valid empty result. It advances nothing, produces no ingestion
		// batch, and is emphatically not a failure.
		a.counters.emptyResults.Add(1)
		return result, nil
	}

	// positions is the furthest each stream was read to in this window. Every
	// event contributes, whether it was delivered now, delivered before, or
	// permanently refused: all of them were read, and none of them will be
	// produced by this window again.
	positions := map[string]readEvent{}
	records := make([]model.NormalizedLog, 0, len(events))
	batchID, err := a.ids.New()
	if err != nil {
		return result, fmt.Errorf("cloudwatch: generating a batch id: %w", err)
	}
	seen := map[string]bool{}
	for _, event := range events {
		record, mapErr := state.mapper.Record(batchID, state.group, event, now)
		if mapErr != nil {
			if errors.Is(mapErr, ErrRegionalBoundary) {
				a.counters.boundaryRejections.Add(1)
				return result, mapErr
			}
			// Record-local. It will never become a record, so the window moves
			// past it rather than replaying it forever.
			result.Rejected++
			a.counters.recordsRejected.Add(1)
			advance(positions, event, "")
			continue
		}
		if seen[record.RecordID] {
			// The same event on two overlapping pages. One read, one record.
			result.Duplicates++
			a.counters.duplicateRecords.Add(1)
			advance(positions, event, record.RecordID)
			continue
		}
		seen[record.RecordID] = true
		advance(positions, event, record.RecordID)
		if _, alreadySent := state.delivered[record.RecordID]; alreadySent {
			result.Duplicates++
			a.counters.duplicateRecords.Add(1)
			continue
		}
		records = append(records, record)
	}

	if len(records) > 0 {
		ack, ingestErr := a.sink.IngestRecords(ctx, state.mapper.Envelope(now), records)
		if ingestErr != nil {
			return result, ingestErr
		}
		if !ack.Acknowledged {
			// Without a durable journal write there is nothing to checkpoint.
			// Declaring delivery complete here is the one mistake this whole
			// ordering exists to prevent.
			return result, ErrNotAcknowledged
		}
		result.Delivered = len(records)
		a.counters.recordsDelivered.Add(uint64(len(records)))
		for _, record := range records {
			state.delivered[record.RecordID] = record.EventTime
		}
	}

	committed, err := a.commit(ctx, state, positions, now)
	if err != nil {
		return result, err
	}
	result.Checkpoints = committed
	return result, nil
}

// advance records the furthest position reached in a stream. A source may
// answer out of order, so the maximum is taken rather than the last seen.
func advance(positions map[string]readEvent, event Event, recordID string) {
	if event.Timestamp.IsZero() {
		// A position is only meaningful in the clock the query filters on. An
		// event without one cannot move the checkpoint; the overlap will read
		// it again, and its stable identity makes that harmless.
		return
	}
	current, present := positions[event.StreamName]
	if present && !event.Timestamp.After(current.event.Timestamp) {
		return
	}
	positions[event.StreamName] = readEvent{event: event, recordID: recordID}
}

// commit writes the new positions. A position never moves backwards: a late
// event inside the overlap is delivered, but its older timestamp must not drag
// the stream back over records already accounted for.
func (a *Adapter) commit(ctx context.Context, state *groupState, positions map[string]readEvent, now time.Time) (int, error) {
	pending := make([]Checkpoint, 0, len(positions))
	for _, streamName := range sortedReadKeys(positions) {
		read := positions[streamName]
		if existing, present := state.positions[streamName]; present && !read.event.Timestamp.After(existing.Position) {
			continue
		}
		pending = append(pending, Checkpoint{
			Stream:      Stream{Group: state.group, Name: streamName},
			Position:    read.event.Timestamp.UTC(),
			EventID:     read.event.EventID,
			CommittedAt: now,
		})
	}
	if len(pending) == 0 {
		return 0, nil
	}
	if err := a.checkpoints.Commit(ctx, pending); err != nil {
		return 0, err
	}
	a.counters.checkpointCommits.Add(1)
	for _, checkpoint := range pending {
		state.positions[checkpoint.Stream.Name] = checkpoint
	}
	return len(pending), nil
}

// loadCheckpoints reads the durable positions once per process and keeps them
// in step afterwards. Rereading every cycle would make the store the hot path
// of a loop whose whole purpose is to avoid unnecessary reads.
func (a *Adapter) loadCheckpoints(ctx context.Context, state *groupState) ([]Checkpoint, error) {
	if state.loadedOnce {
		loaded := make([]Checkpoint, 0, len(state.positions))
		for _, checkpoint := range state.positions {
			loaded = append(loaded, checkpoint)
		}
		return loaded, nil
	}
	loaded, err := a.checkpoints.LoadGroup(ctx, state.group)
	if err != nil {
		return nil, err
	}
	for _, checkpoint := range loaded {
		state.positions[checkpoint.Stream.Name] = checkpoint
	}
	state.loadedOnce = true
	return loaded, nil
}

// window returns the half-open event-time range this cycle reads.
//
// The start is one lookback behind the group retrieval horizon: the greatest
// position reached by any stream in the last exhaustive group-wide query. A
// quiet stream's older diagnostic checkpoint cannot pin FilterLogEvents in the
// past, because that API already searched every stream in the queried group.
// The lookback still recovers bounded late delivery on any stream. The end is
// bounded so that catching up after an outage happens in bounded steps.
func (a *Adapter) window(checkpoints []Checkpoint, now time.Time) (time.Time, time.Time) {
	start := now.Add(-a.initialLookback)
	if len(checkpoints) > 0 {
		latest := checkpoints[0].Position
		for _, checkpoint := range checkpoints[1:] {
			if checkpoint.Position.After(latest) {
				latest = checkpoint.Position
			}
		}
		start = latest.Add(-a.lookback)
	}
	if floor := now.Add(-a.maxLookback); start.Before(floor) {
		start = floor
	}
	end := start.Add(a.maxWindow)
	if end.After(now) {
		end = now
	}
	if !end.After(start) {
		// A clock that has not moved since the last cycle still asks a
		// well-formed question; it simply finds nothing.
		end = start
	}
	return start.UTC(), end.UTC()
}

// forget drops remembered record ids below the floor. Without it the
// suppression set grows for the lifetime of the process.
func (a *Adapter) forget(state *groupState, before time.Time) {
	for recordID, eventTime := range state.delivered {
		if eventTime.Before(before) {
			delete(state.delivered, recordID)
		}
	}
}

// read walks pages until the retrieval horizon, retrying throttled attempts.
func (a *Adapter) read(ctx context.Context, group Group, start, end time.Time) ([]Event, int, error) {
	var events []Event
	token := ""
	pages := 0
	for {
		page, err := a.readPage(ctx, Query{
			LogGroup: group.LogGroup, Start: start, End: end, NextToken: token, Limit: a.pageLimit,
		})
		if err != nil {
			return events, pages, err
		}
		pages++
		// The echoed region and group are checked before a single event is
		// mapped. A misconfigured endpoint would otherwise have another
		// region's logs stored under this region's account and identity.
		if page.Region != group.Region || page.LogGroup != group.LogGroup {
			a.counters.boundaryRejections.Add(1)
			return nil, pages, fmt.Errorf("%w: a page claimed region %q and group %q",
				ErrRegionalBoundary, page.Region, page.LogGroup)
		}
		events = append(events, page.Events...)
		if page.NextToken == "" {
			return events, pages, nil
		}
		if pages >= a.maxPages {
			// Truncating here and checkpointing anyway would skip every event
			// past this point with nothing reporting it.
			return nil, pages, ErrPageBudgetExhausted
		}
		token = page.NextToken
	}
}

// readPage retries throttling with exponential backoff and full jitter, and
// gives up once the attempt budget is spent. The caller's next cycle is itself
// the retry, so holding the worker here indefinitely would starve every other
// group behind this one.
func (a *Adapter) readPage(ctx context.Context, query Query) (Page, error) {
	attempts := a.backoff.Attempts()
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			if err := a.clock.Sleep(ctx, a.backoff.Delay(attempt-1)); err != nil {
				return Page{}, err
			}
		}
		page, err := a.api.FilterEvents(ctx, query)
		if err == nil {
			return page, nil
		}
		lastErr = err
		if errors.Is(err, ErrThrottled) {
			a.counters.throttled.Add(1)
			continue
		}
		// Anything else is not a rate problem, so waiting cannot help. Reporting
		// it as retryable here would hide it behind an attempt budget.
		a.counters.sourceFailures.Add(1)
		return Page{}, err
	}
	return Page{}, lastErr
}

func sortedReadKeys(values map[string]readEvent) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func orDefault(value, fallback time.Duration) time.Duration {
	if value == 0 {
		return fallback
	}
	return value
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
