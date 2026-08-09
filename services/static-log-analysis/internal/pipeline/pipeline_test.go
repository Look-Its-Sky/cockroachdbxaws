package pipeline_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/admission"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/journal"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/pipeline"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/otlpgen"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/testids"
)

const (
	tenantID       = "tenant-a"
	classification = "SENSITIVE"
)

// capturingStore records what the coordinator prepared and hands back exactly
// what a Store reports. It deliberately does NOT implement the five-in-five
// election: the half-open window rule lives inside one CockroachDB transaction
// and is covered against a real database by
// persistence.TestCandidate* and the pipeline integration tests. A fake that
// re-derived the rule would only assert that the fake agrees with itself.
type capturingStore struct {
	failures int
	inputs   [][]persistence.ProcessInput
}

// electingStore models a Store which elected one candidate, so the coordinator's
// reporting and journal ordering can be exercised without a database. Which
// candidate it elects is arbitrary and is not the rule.
type electingStore struct {
	capturingStore
	elected int
}

func (s *electingStore) ProcessBatch(ctx context.Context, inputs []persistence.ProcessInput) ([]persistence.ProcessResult, error) {
	results, err := s.capturingStore.ProcessBatch(ctx, inputs)
	if err != nil {
		return results, err
	}
	for i, input := range inputs {
		if input.InvestigationCandidate == nil || s.elected > 0 {
			continue
		}
		results[i].InvestigationID = input.InvestigationCandidate.InvestigationID
		results[i].InvestigationCreated = true
		s.elected++
	}
	return results, nil
}

type poisonStore struct{}

// matchingBoundary is the Store boundary a correctly deployed coordinator sees.
func matchingBoundary() persistence.Boundary {
	return persistence.Boundary{Scope: persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID},
		Classification: classification, PolicyVersion: redact.MinimalPolicy().Version()}
}

func (*capturingStore) Boundary() persistence.Boundary { return matchingBoundary() }
func (*poisonStore) Boundary() persistence.Boundary    { return matchingBoundary() }

func (*poisonStore) ProcessBatch(_ context.Context, inputs []persistence.ProcessInput) ([]persistence.ProcessResult, error) {
	for _, input := range inputs {
		if input.Record.EventTime.Equal(fakeclock.Origin.Add(time.Second)) {
			return nil, persistence.ErrInvalidInput
		}
	}
	results := make([]persistence.ProcessResult, len(inputs))
	for i, input := range inputs {
		results[i] = persistence.ProcessResult{RecordID: input.Record.RecordID, IncidentID: input.IncidentID, Generation: input.Generation}
	}
	return results, nil
}

func (s *capturingStore) ProcessBatch(_ context.Context, inputs []persistence.ProcessInput) ([]persistence.ProcessResult, error) {
	copyOfInputs := append([]persistence.ProcessInput(nil), inputs...)
	s.inputs = append(s.inputs, copyOfInputs)
	if s.failures > 0 {
		s.failures--
		return nil, persistence.ErrUnavailable
	}
	results := make([]persistence.ProcessResult, len(inputs))
	for i, input := range inputs {
		results[i] = persistence.ProcessResult{RecordID: input.Record.RecordID, IncidentID: input.IncidentID, Generation: input.Generation}
		if input.Investigation != nil {
			results[i].InvestigationID = input.Investigation.InvestigationID
			results[i].InvestigationCreated = true
		}
	}
	return results, nil
}

func TestOTLPPaymentErrorsAreRedactedBeforeDurableAcknowledgementAndDeduplicated(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy, err := redact.MinimalPolicy().WithForbiddenValues("customer-secret")
	if err != nil {
		t.Fatal(err)
	}
	j := openJournal(t, clock, policy)
	store := &capturingStore{}
	service := newService(t, clock, policy, j, store)
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	record := producer.PaymentError(otlpgen.WithBody("card declined customer-secret"))
	payload := marshalRequest(t, producer.Request(record))

	first, err := service.Ingest(context.Background(), envelope(clock.Now()), payload, admission.EncodingIdentity)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	second, err := service.Ingest(context.Background(), envelope(clock.Now().Add(time.Second)), payload, admission.EncodingIdentity)
	if err != nil {
		t.Fatalf("duplicate ingest: %v", err)
	}
	if !first.Acknowledged || !second.Acknowledged || first.Accepted != 1 || second.Accepted != 1 {
		t.Fatalf("unexpected acknowledgements: first=%+v second=%+v", first, second)
	}
	stats, err := j.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 1 {
		t.Fatalf("duplicate delivery created %d pending records, want 1", stats.Pending)
	}
	claimed, err := j.Claim(1, "redaction-check")
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: records=%d err=%v", len(claimed), err)
	}
	if got := claimed[0].Record.Body.String; got == "card declined customer-secret" || policy.ValidateRecord(claimed[0].Record) != nil {
		t.Fatalf("unsafe content crossed journal boundary: %q", got)
	}
}

func TestMissingProducerUIDIsAcceptedAndMeasured(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	service := newService(t, clock, policy, openJournal(t, clock, policy), &capturingStore{})
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	payload := marshalRequest(t, producer.Request(producer.Record(otlpgen.WithoutRecordUID())))

	result, err := service.Ingest(context.Background(), envelope(clock.Now()), payload, admission.EncodingIdentity)
	if err != nil || !result.Acknowledged || result.Accepted != 1 {
		t.Fatalf("derived record was not accepted: result=%+v err=%v", result, err)
	}
	if got := service.Counters().DerivedIdentity; got != 1 {
		t.Fatalf("derived identity counter=%d, want 1", got)
	}
}

// A database failure must leave every claim exactly where it was and re-prepare
// the identical transactional candidates on the retry. Whether any candidate is
// elected is the Store's decision, which this test does not simulate.
func TestDatabaseFailureKeepsClaimsAndRepreparesEveryCandidate(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	store := &electingStore{capturingStore: capturingStore{failures: 1}}
	service := newService(t, clock, policy, j, store)
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))

	for i := 0; i < 5; i++ {
		record := producer.PaymentError(otlpgen.AtEventTime(uint64(fakeclock.Origin.Add(time.Duration(i) * time.Second).UnixNano())))
		result, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, producer.Request(record)), admission.EncodingIdentity)
		if err != nil || !result.Acknowledged {
			t.Fatalf("ingest %d: result=%+v err=%v", i, result, err)
		}
	}
	clock.Advance(2*time.Minute + 5*time.Second)
	if _, err := service.Process(context.Background(), 100); !errors.Is(err, persistence.ErrUnavailable) {
		t.Fatalf("first process error = %v, want unavailable", err)
	}
	stats, _ := j.Stats()
	if stats.Claimed != 5 || stats.Committed != 0 {
		t.Fatalf("database failure lost work: %+v", stats)
	}
	result, err := service.Process(context.Background(), 100)
	if err != nil {
		t.Fatalf("retry process: %v", err)
	}
	if result.Processed != 5 || result.Investigations != 1 {
		t.Fatalf("unexpected processing result: %+v", result)
	}
	// Both attempts must present the same fully prepared cohort; the retry is not
	// allowed to arrive at the Store with less material than the first attempt.
	if len(store.inputs) != 2 {
		t.Fatalf("store saw %d batches, want the failed attempt and its retry", len(store.inputs))
	}
	for attempt, inputs := range store.inputs {
		if candidates := preparedCandidates(inputs); candidates != 5 {
			t.Fatalf("attempt %d prepared %d transactional candidates, want 5", attempt, candidates)
		}
	}
	stats, _ = j.Stats()
	if stats.Committed != 5 || stats.Pending != 0 || stats.Claimed != 0 {
		t.Fatalf("journal was not committed after database success: %+v", stats)
	}
}

// vertical-slice.md: every finalized, grouping-eligible ordinary error carries a
// fully prepared candidate, whatever the running count is. The coordinator never
// counts to five itself, and it reports only what the Store elected.
func TestEveryFinalizedOrdinaryErrorCarriesACandidateAndOnlyTheStoreElects(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	store := &capturingStore{}
	service := newService(t, clock, policy, j, store)
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	for i := 0; i < 4; i++ {
		record := producer.PaymentError(otlpgen.AtEventTime(uint64(fakeclock.Origin.Add(time.Duration(i) * time.Second).UnixNano())))
		if _, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, producer.Request(record)), admission.EncodingIdentity); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(2*time.Minute + 5*time.Second)
	result, err := service.Process(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if result.Processed != 4 {
		t.Fatalf("unexpected processing result: %+v", result)
	}
	if result.Investigations != 0 {
		t.Fatalf("coordinator reported %d investigations the store never created", result.Investigations)
	}
	if candidates := preparedCandidates(store.inputs[len(store.inputs)-1]); candidates != 4 {
		t.Fatalf("prepared %d candidates below the threshold, want one per finalized error", candidates)
	}
}

func preparedCandidates(inputs []persistence.ProcessInput) int {
	prepared := 0
	for _, input := range inputs {
		if input.InvestigationCandidate != nil {
			prepared++
		}
	}
	return prepared
}

func TestPartialClaimRejectionStillDurablyAcknowledgesSafeSibling(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	service := newService(t, clock, policy, j, &capturingStore{})
	ids := testids.New(testids.WithClock(clock))
	validProducer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(ids))
	invalidProducer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(ids), otlpgen.WithService("inventoryservice"))
	request := validProducer.Request(validProducer.PaymentError())
	request.ResourceLogs = append(request.ResourceLogs, invalidProducer.Request(invalidProducer.PaymentError()).ResourceLogs...)

	result, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, request), admission.EncodingIdentity)
	if err != nil || !result.Acknowledged || result.Accepted != 1 || len(result.Rejected) != 1 {
		t.Fatalf("partial ingest: result=%+v err=%v", result, err)
	}
	if result.Rejected[0].Index != 1 || result.Rejected[0].Reason != pipeline.RejectionClaimNotPermitted {
		t.Fatalf("unexpected rejection: %+v", result.Rejected)
	}
	if got := service.Counters().RecordRejections.ClaimNotPermitted; got != 1 {
		t.Fatalf("claim rejection counter=%d, want 1", got)
	}
	stats, err := j.Stats()
	if err != nil || stats.Pending != 1 {
		t.Fatalf("safe sibling was not durable: stats=%+v err=%v", stats, err)
	}
}

func TestRecordLocalRejectionsAreCountedByClosedSafeCategory(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	service := newService(t, clock, policy, openJournal(t, clock, policy), &capturingStore{})
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	missingTimes := producer.Record(otlpgen.WithoutTimestamps())
	missingContent := producer.Record(otlpgen.WithoutBody(), otlpgen.WithEventName(""))

	result, err := service.Ingest(context.Background(), envelope(clock.Now()),
		marshalRequest(t, producer.Request(missingTimes, missingContent)), admission.EncodingIdentity)
	if err != nil || !result.Acknowledged || result.Accepted != 0 || len(result.Rejected) != 2 {
		t.Fatalf("unexpected rejection result=%+v err=%v", result, err)
	}
	if result.Rejected[0].Reason != pipeline.RejectionInvalidRecord ||
		result.Rejected[0].Subreason != pipeline.RejectionInvalidMissingTimestamps {
		t.Fatalf("want safe missing-timestamps subreason, got %+v", result.Rejected[0])
	}
	if result.Rejected[1].Reason != pipeline.RejectionInvalidRecord ||
		result.Rejected[1].Subreason != pipeline.RejectionInvalidMissingContent {
		t.Fatalf("want safe missing-content subreason, got %+v", result.Rejected[1])
	}

	counters := service.Counters().RecordRejections
	if counters.InvalidMissingTimestamps != 1 || counters.InvalidMissingContent != 1 || counters.Total() != 2 {
		t.Fatalf("each rejection must increment exactly one counter, got %+v", counters)
	}
}

func TestDeterministicPoisonIsIsolatedAndQuarantineDoesNotBreakRestart(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	dir := filepath.Join(t.TempDir(), "journal")
	config := journal.Config{Dir: dir, Owner: "poison-test", TenantID: tenantID, Region: otlpgen.DefaultRegion,
		Classification: classification, Clock: clock, Validator: policy, MaxBytes: 32 << 20, MinFreeBytes: 1,
		FreeSpace: func(string) (uint64, error) { return 1 << 40, nil }}
	j, err := journal.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	service := newService(t, clock, policy, j, &poisonStore{})
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	for i := 0; i < 2; i++ {
		record := producer.PaymentError(otlpgen.AtEventTime(uint64(fakeclock.Origin.Add(time.Duration(i) * time.Second).UnixNano())))
		if _, err := service.Ingest(context.Background(), envelope(clock.Now()), marshalRequest(t, producer.Request(record)), admission.EncodingIdentity); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(2*time.Minute + 5*time.Second)
	result, err := service.Process(context.Background(), 100)
	if err != nil || result.Processed != 1 || result.Quarantined != 1 {
		t.Fatalf("poison isolation: result=%+v err=%v", result, err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = journal.Open(config)
	if err != nil {
		t.Fatalf("reopen after quarantine: %v", err)
	}
	defer j.Close()
	if _, err := pipeline.New(pipeline.Config{Clock: clock, IDs: testids.New(testids.WithClock(clock)), Journal: j,
		Store: &capturingStore{}, Policy: policy, Scope: persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID}, Classification: classification}); err != nil {
		t.Fatalf("service restart after quarantine: %v", err)
	}
	stats, err := j.Stats()
	if err != nil || stats.Committed != 1 || stats.Quarantined != 1 {
		t.Fatalf("restart state: stats=%+v err=%v", stats, err)
	}
}

func newService(t *testing.T, clock *fakeclock.Clock, policy *redact.Policy, j pipeline.Journal, store pipeline.Store) *pipeline.Service {
	t.Helper()
	service, err := pipeline.New(pipeline.Config{
		Clock: clock, IDs: testids.New(testids.WithClock(clock)), Journal: j, Store: store,
		Policy: policy, Scope: persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID}, Classification: classification,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func openJournal(t *testing.T, clock *fakeclock.Clock, policy *redact.Policy) *journal.Journal {
	t.Helper()
	j, err := journal.Open(journal.Config{
		Dir: filepath.Join(t.TempDir(), "journal"), Owner: "pipeline-test", TenantID: tenantID,
		Region: otlpgen.DefaultRegion, Classification: classification, Clock: clock, Validator: policy,
		MaxBytes: 32 << 20, MinFreeBytes: 1, FreeSpace: func(string) (uint64, error) { return 1 << 40, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func envelope(receivedAt time.Time) model.TrustedEnvelope {
	return model.TrustedEnvelope{
		SourceType: model.SourceTypeOTLP, SourceAccount: "aws-account-a", Region: otlpgen.DefaultRegion,
		AllowedEnvironments: []string{otlpgen.DefaultEnvironment}, AllowedServices: []string{otlpgen.DefaultService},
		SourceInstance: "collector-a", CredentialIdentity: "workload-a", ReceivedAt: receivedAt.UTC(),
	}
}

func marshalRequest(t *testing.T, request proto.Message) []byte {
	t.Helper()
	payload, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
