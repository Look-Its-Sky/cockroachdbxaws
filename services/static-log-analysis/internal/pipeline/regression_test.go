package pipeline_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/admission"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/ids"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/incident"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/journal"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/pipeline"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/otlpgen"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/testids"
)

// errorStore returns one categorical persistence failure for every batch. It is
// how a terminal escalation is presented to the coordinator without a database.
type errorStore struct {
	err      error
	boundary *persistence.Boundary
}

func (s *errorStore) Boundary() persistence.Boundary {
	if s.boundary != nil {
		return *s.boundary
	}
	return matchingBoundary()
}

func (s *errorStore) ProcessBatch(context.Context, []persistence.ProcessInput) ([]persistence.ProcessResult, error) {
	return nil, s.err
}

// shortResultStore returns fewer results than inputs, which a correct Store
// never does. The coordinator must reject the shape rather than index past it.
type shortResultStore struct{}

func (*shortResultStore) Boundary() persistence.Boundary { return matchingBoundary() }

func (*shortResultStore) ProcessBatch(_ context.Context, inputs []persistence.ProcessInput) ([]persistence.ProcessResult, error) {
	return make([]persistence.ProcessResult, len(inputs)-1), nil
}

// toggleIDs models a transient UUIDv7 entropy or clock fault. The underlying
// source stays healthy so the same record can succeed on a later attempt.
type toggleIDs struct {
	inner ids.Source
	fail  bool
}

func (s *toggleIDs) New() (string, error) {
	if s.fail {
		return "", errors.New("ids: entropy temporarily unavailable")
	}
	return s.inner.New()
}

// recordingJournal captures the categorical quarantine reasons a real journal
// was asked for, which Stats deliberately does not expose.
type recordingJournal struct {
	*journal.Journal
	reasons []journal.QuarantineReason
}

func (j *recordingJournal) Quarantine(recordID, token string, reason journal.QuarantineReason) error {
	j.reasons = append(j.reasons, reason)
	return j.Journal.Quarantine(recordID, token, reason)
}

// stubJournal is the seam for journal failures and manifest boundaries a
// healthy Pebble directory cannot be asked for on demand.
type stubJournal struct {
	appendErr error
	manifest  journal.Manifest
}

func (j *stubJournal) AppendBatch(string, []journal.Admission) error { return j.appendErr }

// Capacity reports a journal with room. These tests are about append and claim
// outcomes; capacity shedding has its own tests and must not silently change
// what this stub is answering.
func (j *stubJournal) Capacity() journal.Capacity {
	return journal.Capacity{Used: 0, Total: 1 << 30, CanPersist: true}
}
func (j *stubJournal) AdoptClaims(string, int) ([]journal.ClaimedRecord, error) {
	return nil, nil
}
func (j *stubJournal) ClaimTTL() time.Duration { return journal.DefaultClaimTTL }
func (j *stubJournal) RenewClaims([]journal.CommitClaim) ([]journal.ClaimedRecord, error) {
	return nil, nil
}
func (j *stubJournal) Manifest() journal.Manifest { return j.manifest }
func (j *stubJournal) Claim(int, string) ([]journal.ClaimedRecord, error) {
	return nil, nil
}
func (j *stubJournal) MarkCommittedBatch([]journal.CommitClaim) error { return nil }
func (j *stubJournal) Quarantine(string, string, journal.QuarantineReason) error {
	return nil
}
func (j *stubJournal) RecoverRetained(string, int) ([]journal.RecoveredRecord, string, error) {
	return nil, "", nil
}
func (j *stubJournal) Retention() time.Duration { return journal.DefaultSafetyDelay }

// A record without service.name is admitted by design: only service-specific
// agent execution requires one. It must therefore never be able to make the
// process unstartable, and it must reach a terminal categorical outcome.
func TestAcknowledgedServicelessRecordStartsAndReachesTerminalQuarantine(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	dir := filepath.Join(t.TempDir(), "journal")
	config := journal.Config{Dir: dir, Owner: "serviceless-test", TenantID: tenantID, Region: otlpgen.DefaultRegion,
		Classification: classification, Clock: clock, Validator: policy, MaxBytes: 32 << 20, MinFreeBytes: 1,
		FreeSpace: func(string) (uint64, error) { return 1 << 40, nil }}
	base, err := journal.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	recorder := &recordingJournal{Journal: base}
	service := newServiceOn(t, clock, policy, recorder, &capturingStore{})
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	request := withoutServiceName(producer.Request(producer.PaymentError()))

	result, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, request), admission.EncodingIdentity)
	if err != nil || !result.Acknowledged || result.Accepted != 1 {
		t.Fatalf("serviceless ingest: result=%+v err=%v", result, err)
	}
	restarted, err := pipeline.New(pipeline.Config{Clock: clock, IDs: testids.New(testids.WithClock(clock)),
		Journal: recorder, Store: &capturingStore{}, Policy: policy,
		Scope:          persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID},
		Classification: classification})
	if err != nil {
		t.Fatalf("startup after acknowledging a serviceless record: %v", err)
	}
	if got := restarted.Counters().RecoverySkipped; got != 1 {
		t.Fatalf("recovery skipped %d unobservable records, want 1 counted", got)
	}
	clock.Advance(2*time.Minute + 5*time.Second)
	processed, err := service.Process(context.Background(), 100)
	if err != nil || processed.Quarantined != 1 || processed.Processed != 0 {
		t.Fatalf("serviceless process: result=%+v err=%v", processed, err)
	}
	if len(recorder.reasons) != 1 || recorder.reasons[0] != journal.QuarantineUnsupportedData {
		t.Fatalf("quarantine reasons=%v, want one unsupported-data reason", recorder.reasons)
	}
	stats, err := base.Stats()
	if err != nil || stats.Quarantined != 1 || stats.Pending != 0 || stats.Claimed != 0 {
		t.Fatalf("terminal outcome: stats=%+v err=%v", stats, err)
	}
}

// A generation-key collision and an immutable-value conflict are evidence an
// operator must investigate. Destroying the payload and reporting success would
// remove exactly what the investigation needs.
func TestTerminalPersistenceEscalationsRetainClaimsAndDestroyNothing(t *testing.T) {
	for name, sentinel := range map[string]error{
		"identity_conflict": persistence.ErrIdentityConflict,
		"immutable":         persistence.ErrImmutable,
	} {
		t.Run(name, func(t *testing.T) {
			clock := fakeclock.NewAtOrigin()
			policy := redact.MinimalPolicy()
			j := openJournal(t, clock, policy)
			service := newService(t, clock, policy, j, &errorStore{err: sentinel})
			producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
			for i := 0; i < 3; i++ {
				record := producer.PaymentError(otlpgen.AtEventTime(uint64(fakeclock.Origin.Add(time.Duration(i) * time.Second).UnixNano())))
				if _, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, producer.Request(record)), admission.EncodingIdentity); err != nil {
					t.Fatal(err)
				}
			}
			clock.Advance(2*time.Minute + 5*time.Second)
			result, err := service.Process(context.Background(), 100)
			if !errors.Is(err, sentinel) {
				t.Fatalf("escalation error=%v, want %v", err, sentinel)
			}
			if result.Quarantined != 0 {
				t.Fatalf("terminal escalation destroyed %d payloads", result.Quarantined)
			}
			stats, statsErr := j.Stats()
			if statsErr != nil || stats.Quarantined != 0 || stats.Claimed != 3 || stats.Committed != 0 {
				t.Fatalf("escalation lost evidence: stats=%+v err=%v", stats, statsErr)
			}
		})
	}
}

// A scope, policy-version, or classification disagreement is a deployment
// fault. Isolating it record by record would irreversibly destroy every
// acknowledged record in the journal and report success.
func TestStoreConfigurationMismatchQuarantinesNothingAndDestroysNothing(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	service := newService(t, clock, policy, j, &errorStore{err: persistence.ErrConfiguration})
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	for i := 0; i < 5; i++ {
		record := producer.PaymentError(otlpgen.AtEventTime(uint64(fakeclock.Origin.Add(time.Duration(i) * time.Second).UnixNano())))
		if _, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, producer.Request(record)), admission.EncodingIdentity); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(2*time.Minute + 5*time.Second)
	result, err := service.Process(context.Background(), 100)
	if !errors.Is(err, persistence.ErrConfiguration) {
		t.Fatalf("configuration mismatch error=%v, want it surfaced to the caller", err)
	}
	if result.Quarantined != 0 {
		t.Fatalf("configuration mismatch destroyed %d acknowledged records", result.Quarantined)
	}
	stats, statsErr := j.Stats()
	if statsErr != nil || stats.Quarantined != 0 || stats.Claimed != 5 || stats.Committed != 0 {
		t.Fatalf("configuration mismatch lost durable data: stats=%+v err=%v", stats, statsErr)
	}
}

// The journal manifest is immutable. A coordinator that contradicts it must
// refuse to run rather than discovering the contradiction one record at a time.
func TestJournalManifestMismatchRefusesStartup(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	for name, manifest := range map[string]journal.Manifest{
		"region":         {Region: "eu-west-1", TenantID: tenantID, Classification: classification, PolicyVersion: policy.Version()},
		"tenant":         {Region: otlpgen.DefaultRegion, TenantID: "tenant-b", Classification: classification, PolicyVersion: policy.Version()},
		"classification": {Region: otlpgen.DefaultRegion, TenantID: tenantID, Classification: "INTERNAL", PolicyVersion: policy.Version()},
		"policy_version": {Region: otlpgen.DefaultRegion, TenantID: tenantID, Classification: classification, PolicyVersion: "redact:other"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := pipeline.New(pipeline.Config{Clock: clock, IDs: testids.New(testids.WithClock(clock)),
				Journal: &stubJournal{manifest: manifest}, Store: &capturingStore{}, Policy: policy,
				Scope:          persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID},
				Classification: classification})
			if !errors.Is(err, pipeline.ErrInvalidConfig) {
				t.Fatalf("manifest %s mismatch error=%v, want a configuration error", name, err)
			}
		})
	}
}

// The Store's classification and policy version reach records only through the
// sealed replay digest, where a mismatch is indistinguishable from a forged
// record and must fail closed. Startup is therefore the only place a boundary
// disagreement can be caught before it destroys every acknowledged record.
func TestStoreBoundaryMismatchRefusesStartup(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	scope := persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID}
	for name, boundary := range map[string]persistence.Boundary{
		"scope":          {Scope: persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: "tenant-b"}, Classification: classification, PolicyVersion: policy.Version()},
		"classification": {Scope: scope, Classification: "INTERNAL", PolicyVersion: policy.Version()},
		"policy_version": {Scope: scope, Classification: classification, PolicyVersion: "redact:other"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := pipeline.New(pipeline.Config{Clock: clock, IDs: testids.New(testids.WithClock(clock)),
				Journal: &stubJournal{manifest: matchingManifest(policy)},
				Store:   &errorStore{err: persistence.ErrUnavailable, boundary: &boundary},
				Policy:  policy, Scope: scope, Classification: classification})
			if !errors.Is(err, pipeline.ErrInvalidConfig) {
				t.Fatalf("store %s mismatch error=%v, want a configuration error", name, err)
			}
		})
	}
}

// UUIDv7 generation can fail transiently. Treating that as poison deletes a
// valid record's payload, so the incident it belonged to never reaches five.
func TestTransientIdentifierFailureRetainsClaimInsteadOfDestroyingPayload(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	source := &toggleIDs{inner: testids.New(testids.WithClock(clock))}
	store := &capturingStore{}
	service, err := pipeline.New(pipeline.Config{Clock: clock, IDs: source, Journal: j, Store: store, Policy: policy,
		Scope: persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID}, Classification: classification})
	if err != nil {
		t.Fatal(err)
	}
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	if _, err := service.Ingest(context.Background(), envelope(clock.Now()),
		marshalRequest(t, producer.Request(producer.PaymentError())), admission.EncodingIdentity); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2*time.Minute + 5*time.Second)
	source.fail = true
	result, err := service.Process(context.Background(), 100)
	if err == nil {
		t.Fatalf("transient identifier failure was swallowed: %+v", result)
	}
	if result.Quarantined != 0 {
		t.Fatalf("transient identifier failure destroyed %d payloads", result.Quarantined)
	}
	stats, statsErr := j.Stats()
	if statsErr != nil || stats.Quarantined != 0 || stats.Claimed != 1 {
		t.Fatalf("claim was not retained: stats=%+v err=%v", stats, statsErr)
	}
	source.fail = false
	retried, err := service.Process(context.Background(), 100)
	if err != nil || retried.Processed != 1 {
		t.Fatalf("retry after transient failure: result=%+v err=%v", retried, err)
	}
}

// The engine already classifies lateness against its own watermark. Subtracting
// allowed lateness a second time makes occurrences.late false for records the
// rule engine itself considers late.
func TestOccurrenceLatenessComesFromTheEngineWatermark(t *testing.T) {
	origin := fakeclock.Origin
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	store := &capturingStore{}
	service := newService(t, clock, policy, j, store)
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	// The episode opens at the origin and its watermark is later carried forward
	// by a record observed ten minutes in. Observed time advances with the clock
	// because an event time far ahead of it would be inferred away.
	for _, offset := range []time.Duration{0, time.Second} {
		record := producer.PaymentError(otlpgen.AtEventTime(uint64(origin.Add(offset).UnixNano())))
		if _, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, producer.Request(record)), admission.EncodingIdentity); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(10 * time.Minute)
	frontier := producer.PaymentError(otlpgen.AtEventTime(uint64(origin.Add(10 * time.Minute).UnixNano())))
	if _, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, producer.Request(frontier)), admission.EncodingIdentity); err != nil {
		t.Fatal(err)
	}
	// now - allowed_lateness is behind the maximum event time, so the
	// finalization frontier stays strictly behind the event-time watermark plus
	// allowed lateness and the two thresholds cannot be confused.
	clock.Advance(time.Minute)
	if _, err := service.Process(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	late := producer.PaymentError(otlpgen.AtEventTime(uint64(origin.Add(7*time.Minute + 30*time.Second).UnixNano())))
	if _, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, producer.Request(late)), admission.EncodingIdentity); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Process(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	want := origin.Add(7*time.Minute + 30*time.Second)
	found := false
	for _, batch := range store.inputs {
		for _, input := range batch {
			if !input.Record.EventTime.Equal(want) {
				continue
			}
			found = true
			if !input.EvidenceOnly {
				t.Fatalf("record behind the watermark was not evidence-only: %+v", input.Decision)
			}
			if !input.Late {
				t.Fatal("record strictly behind the engine watermark was not marked late")
			}
		}
	}
	if !found {
		t.Fatal("the late record never reached persistence")
	}
}

// Quiet time is measured from bounded activity, min(event_time, observed_time),
// and detection status is the engine's, not a hardcoded constant.
func TestGenerationLifecycleFieldsComeFromTheEngineBoundedByObservedTime(t *testing.T) {
	origin := fakeclock.Origin
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	store := &capturingStore{}
	service := newService(t, clock, policy, j, store)
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	// A future-skewed record inside the accepted window must not be able to hold
	// the incident open past its observed-time bound.
	skewed := producer.PaymentError(otlpgen.AtEventTime(uint64(origin.Add(4 * time.Minute).UnixNano())))
	if _, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, producer.Request(skewed)), admission.EncodingIdentity); err != nil {
		t.Fatal(err)
	}
	clock.Advance(20 * time.Minute)
	if _, err := service.Process(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	if len(store.inputs) != 1 || len(store.inputs[0]) != 1 {
		t.Fatalf("unexpected persistence batches: %+v", store.inputs)
	}
	input := store.inputs[0][0]
	if input.DetectionStatus != string(incident.DetectionQuiet) {
		t.Fatalf("detection_status=%q, want the engine's %q", input.DetectionStatus, incident.DetectionQuiet)
	}
	if want := origin.Add(incident.DefaultQuietPeriod); !input.QuietAt.Equal(want) {
		t.Fatalf("quiet_at=%s, want observed-time bounded %s", input.QuietAt, want)
	}
	if want := origin.Add(incident.DefaultQuietPeriod + incident.DefaultReopenWindow); !input.ReopenUntil.Equal(want) {
		t.Fatalf("reopen_until=%s, want %s", input.ReopenUntil, want)
	}
}

// A misrouted regional envelope is permanently wrong. Reporting it as journal
// unavailability makes the transport retry a poisoned batch forever.
func TestForeignRegionEnvelopeIsRejectedPermanentlyBeforeTheJournal(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	service := newService(t, clock, policy, j, &capturingStore{})
	foreign := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))),
		otlpgen.WithRegion("eu-west-1"))
	foreignEnvelope := envelope(clock.Now())
	foreignEnvelope.Region = "eu-west-1"

	_, err := service.Ingest(context.Background(), foreignEnvelope,
		marshalRequest(t, foreign.Request(foreign.PaymentError())), admission.EncodingIdentity)
	if errors.Is(err, pipeline.ErrJournalUnavailable) {
		t.Fatalf("misrouted regional envelope was reported as retryable: %v", err)
	}
	if !errors.Is(err, pipeline.ErrRequestRejected) {
		t.Fatalf("misrouted regional envelope error=%v, want a permanent rejection", err)
	}
	stats, statsErr := j.Stats()
	if statsErr != nil || stats.Pending != 0 {
		t.Fatalf("foreign envelope reached the journal: stats=%+v err=%v", stats, statsErr)
	}
	if got := service.Counters().ScopeRejections; got != 1 {
		t.Fatalf("scope rejections=%d, want the rejection counted", got)
	}
}

func TestPermanentJournalRejectionIsNotReportedAsUnavailable(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	service := newServiceOn(t, clock, policy,
		&stubJournal{appendErr: journal.ErrInvalidInput, manifest: matchingManifest(policy)}, &capturingStore{})
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))

	_, err := service.Ingest(context.Background(), envelope(clock.Now()),
		marshalRequest(t, producer.Request(producer.PaymentError())), admission.EncodingIdentity)
	if errors.Is(err, pipeline.ErrJournalUnavailable) {
		t.Fatalf("permanent journal rejection was reported as retryable: %v", err)
	}
	if !errors.Is(err, pipeline.ErrRequestRejected) {
		t.Fatalf("permanent journal rejection error=%v, want a permanent rejection", err)
	}
	if got := service.Counters().JournalRejections; got != 1 {
		t.Fatalf("journal rejections=%d, want the rejection counted", got)
	}
}

// A worker loop's error handling must not depend on how many records happened
// to be claimed together.
func TestSingleRecordPoisonIsReportedAsQuarantineWithoutError(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	service := newService(t, clock, policy, j, &errorStore{err: persistence.ErrInvalidInput})
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	if _, err := service.Ingest(context.Background(), envelope(clock.Now()),
		marshalRequest(t, producer.Request(producer.PaymentError())), admission.EncodingIdentity); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2*time.Minute + 5*time.Second)
	result, err := service.Process(context.Background(), 100)
	if err != nil {
		t.Fatalf("single-record poison error=%v, want the same nil error a bisected batch reports", err)
	}
	if result.Quarantined != 1 {
		t.Fatalf("single-record poison result=%+v, want one quarantine", result)
	}
}

func TestShortStoreResultDoesNotCommitOrPanic(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	service := newService(t, clock, policy, j, &shortResultStore{})
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	for i := 0; i < 2; i++ {
		record := producer.PaymentError(otlpgen.AtEventTime(uint64(fakeclock.Origin.Add(time.Duration(i) * time.Second).UnixNano())))
		if _, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, producer.Request(record)), admission.EncodingIdentity); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(2*time.Minute + 5*time.Second)
	if _, err := service.Process(context.Background(), 100); err == nil {
		t.Fatal("a short store response was accepted")
	}
	stats, err := j.Stats()
	if err != nil || stats.Committed != 0 || stats.Claimed != 2 {
		t.Fatalf("short store response committed work: stats=%+v err=%v", stats, err)
	}
}

func TestInvalidAdmissionLimitsAreAConfigurationError(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	_, err := pipeline.New(pipeline.Config{Clock: clock, IDs: testids.New(testids.WithClock(clock)), Journal: j,
		Store: &capturingStore{}, Policy: policy,
		Scope:          persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID},
		Classification: classification, Limits: admission.Limits{MaxRecords: admission.DefaultMaxRecords + 1}})
	if !errors.Is(err, pipeline.ErrInvalidConfig) {
		t.Fatalf("invalid limits error=%v, want a configuration error", err)
	}
}

func newServiceOn(t *testing.T, clock *fakeclock.Clock, policy *redact.Policy, j pipeline.Journal, store pipeline.Store) *pipeline.Service {
	t.Helper()
	service, err := pipeline.New(pipeline.Config{Clock: clock, IDs: testids.New(testids.WithClock(clock)),
		Journal: j, Store: store, Policy: policy,
		Scope:          persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID},
		Classification: classification})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func matchingManifest(policy *redact.Policy) journal.Manifest {
	return journal.Manifest{Region: otlpgen.DefaultRegion, TenantID: tenantID,
		Classification: classification, PolicyVersion: policy.Version()}
}

// withoutServiceName removes the resource attribute that carries the service
// claim, producing the record data-model.md admits without service identity.
func withoutServiceName(request *otlpgen.ExportRequest) *otlpgen.ExportRequest {
	for _, resourceLogs := range request.GetResourceLogs() {
		kept := resourceLogs.GetResource().GetAttributes()[:0]
		for _, attribute := range resourceLogs.GetResource().GetAttributes() {
			if attribute.GetKey() == "service.name" {
				continue
			}
			kept = append(kept, attribute)
		}
		resourceLogs.Resource.Attributes = kept
	}
	return request
}
