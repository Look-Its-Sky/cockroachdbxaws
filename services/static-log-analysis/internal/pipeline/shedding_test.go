package pipeline_test

import (
	"context"
	"errors"
	"testing"

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

// operations.md's utilization table is only a table until something consults a
// live journal. These tests are about the wiring, not about the policy: what a
// policy decides is covered exhaustively in internal/admission, and repeating it
// here would pin the fixture rather than the behaviour.

// fullJournal reports whatever capacity a test wants, and records what it was
// asked to append. Capacity is the one thing a real journal cannot be made to
// report on demand without writing gigabytes.
type fullJournal struct {
	pipeline.Journal
	capacity journal.Capacity
	appended int
}

func (j *fullJournal) Capacity() journal.Capacity { return j.capacity }

func (j *fullJournal) AppendBatch(batchID string, admissions []journal.Admission) error {
	j.appended += len(admissions)
	return j.Journal.AppendBatch(batchID, admissions)
}

func sheddingService(t *testing.T, j pipeline.Journal, policy admission.CapacityPolicy) *pipeline.Service {
	t.Helper()
	classifier, err := admission.Compile(nil)
	if err != nil {
		t.Fatal(err)
	}
	service, err := pipeline.New(pipeline.Config{
		Clock: fakeclock.NewAtOrigin(), IDs: testids.New(), Journal: j, Store: &capturingStore{},
		Policy: redact.MinimalPolicy(), Scope: persistence.Scope{Region: otlpgen.DefaultRegion, TenantID: tenantID},
		Classification: classification, CapacityPolicy: policy, Classifier: classifier,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func sheddingEnvelope(clock *fakeclock.Clock) model.TrustedEnvelope {
	return model.TrustedEnvelope{SourceType: model.SourceTypeOTLP, SourceAccount: "aws-account-a",
		Region: otlpgen.DefaultRegion, AllowedEnvironments: []string{otlpgen.DefaultEnvironment},
		AllowedServices: []string{otlpgen.DefaultService}, SourceInstance: "collector-a",
		CredentialIdentity: "workload-a", ReceivedAt: clock.Now()}
}

func debugPayload(t *testing.T) []byte {
	t.Helper()
	producer := otlpgen.New(otlpgen.WithIDs(testids.New(testids.WithSeed(31))))
	payload, err := otlpgen.Encode(producer.Request(producer.Record(otlpgen.SeverityDebug())))
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func errorPayload(t *testing.T) []byte {
	t.Helper()
	producer := otlpgen.New(otlpgen.WithIDs(testids.New(testids.WithSeed(32))))
	payload, err := otlpgen.Encode(producer.Request(producer.PaymentError()))
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// TestAnEligibleDebugRecordIsShedNearCapacityAndStillAcknowledged pins the
// 85–95% row of operations.md's table. Shedding is acknowledged because the
// record was optional and the decision was deliberate; the caller must not
// retry it forever.
func TestAnEligibleDebugRecordIsShedNearCapacityAndStillAcknowledged(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	real := openJournal(t, clock, policy)
	j := &fullJournal{Journal: real, capacity: journal.Capacity{Used: 90, Total: 100, CanPersist: true}}
	service := sheddingService(t, j, admission.CapacityPolicy{
		NearCapacityPercent: 85, EligibleSources: []model.SourceType{model.SourceTypeOTLP},
	})

	result, err := service.Ingest(context.Background(), sheddingEnvelope(clock), debugPayload(t), admission.EncodingIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Acknowledged {
		t.Fatal("a deliberately shed record was not acknowledged; the caller would retry it forever")
	}
	if j.appended != 0 {
		t.Fatalf("%d records were written despite being shed", j.appended)
	}
	if counters := service.Counters(); counters.Shed == 0 {
		t.Fatal("a shed record was not counted; operations.md requires dropped records to be counted")
	}
}

// TestAProtectedRecordIsNeverShedHoweverFullTheJournalIs is the other half, and
// the one that matters. ERROR is protected, so at capacity it must backpressure
// rather than be dropped: "It never acknowledges a silently dropped mandatory
// record."
func TestAProtectedRecordIsNeverShedHoweverFullTheJournalIs(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	real := openJournal(t, clock, policy)
	j := &fullJournal{Journal: real, capacity: journal.Capacity{Used: 100, Total: 100, CanPersist: false}}
	service := sheddingService(t, j, admission.CapacityPolicy{
		NearCapacityPercent: 85, EligibleSources: []model.SourceType{model.SourceTypeOTLP},
	})

	_, err := service.Ingest(context.Background(), sheddingEnvelope(clock), errorPayload(t), admission.EncodingIdentity)
	if !errors.Is(err, pipeline.ErrJournalUnavailable) {
		t.Fatalf("a full journal answered %v for a protected record, want retryable unavailability", err)
	}
	if j.appended != 0 {
		t.Fatalf("%d records reached a journal that cannot persist", j.appended)
	}
	if counters := service.Counters(); counters.Backpressured == 0 {
		t.Fatal("backpressure was not counted")
	}
}

// TestWellUnderCapacityNothingIsShed keeps the policy from being a permanent
// filter rather than a capacity response.
func TestWellUnderCapacityNothingIsShed(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	real := openJournal(t, clock, policy)
	j := &fullJournal{Journal: real, capacity: journal.Capacity{Used: 10, Total: 100, CanPersist: true}}
	service := sheddingService(t, j, admission.CapacityPolicy{
		NearCapacityPercent: 85, EligibleSources: []model.SourceType{model.SourceTypeOTLP},
	})

	result, err := service.Ingest(context.Background(), sheddingEnvelope(clock), debugPayload(t), admission.EncodingIdentity)
	if err != nil || !result.Acknowledged {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if j.appended != 1 {
		t.Fatalf("%d records were written well under capacity, want 1", j.appended)
	}
	if counters := service.Counters(); counters.Shed != 0 {
		t.Fatal("a record was shed well under capacity")
	}
}

// TestWithoutAConfiguredPolicyNothingIsEverShed keeps shedding opt-in. An
// operator who configured no eligible sources has chosen that nothing may be
// dropped, and the default must honour that rather than invent a threshold.
func TestWithoutAConfiguredPolicyNothingIsEverShed(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	real := openJournal(t, clock, policy)
	j := &fullJournal{Journal: real, capacity: journal.Capacity{Used: 99, Total: 100, CanPersist: true}}
	service := newService(t, clock, policy, j, &capturingStore{})

	result, err := service.Ingest(context.Background(), sheddingEnvelope(clock), debugPayload(t), admission.EncodingIdentity)
	if err != nil || !result.Acknowledged {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if j.appended != 1 {
		t.Fatalf("%d records written with no shedding policy configured, want 1", j.appended)
	}
}
