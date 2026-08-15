package pipeline_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/pipeline"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/cwgen"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
)

// IngestRecords is the second ingestion entry point. It exists because Ingest
// takes OTLP bytes and derives otlp:v1 identity from a producer-assigned record
// UID, which would discard the native CloudWatch event ID that cw:v1 is defined
// over - and that ID is the whole reason an overlapping replay is harmless.
//
// What it must NOT become is a way around the guarantees Ingest enforces. These
// tests pin that it enforces the same ones.

// cwRecords maps count CloudWatch events into normalized records under one
// batch, using the same mapper the adapter uses in production. Building them
// through the real mapper is the point: a hand-written NormalizedLog would
// prove only that this entry point accepts whatever the test made up.
func cwRecords(t *testing.T, batchID string, count int) []model.NormalizedLog {
	t.Helper()
	mapper, err := cloudwatch.NewMapper(cloudwatch.MapperConfig{
		Policy: redact.MinimalPolicy(), Account: cwgen.DefaultAccount, Region: cwgen.DefaultRegion,
		SourceInstance: "cw-adapter-a", CredentialIdentity: "workload-a",
		AllowedServices: []string{cwgen.DefaultService}, AllowedEnvironments: []string{cwgen.DefaultEnvironment},
		Service: cwgen.DefaultService, Environment: cwgen.DefaultEnvironment,
		Classification: classification,
	})
	if err != nil {
		t.Fatal(err)
	}
	group := cloudwatch.Group{Account: cwgen.DefaultAccount, Region: cwgen.DefaultRegion,
		LogGroup: cwgen.DefaultLogGroup}
	producer := cwgen.New(cwgen.WithClock(fakeclock.NewAtOrigin()))
	records := make([]model.NormalizedLog, 0, count)
	for i := 0; i < count; i++ {
		event := producer.PaymentError(cwgen.WithEventID(cwgen.EventID(uint64(i + 1))))
		record, err := mapper.Record(batchID, group, event, fakeclock.Origin)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func TestIngestRecordsAcknowledgesOnlyAfterTheJournalCommits(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	service := newService(t, clock, policy, j, &capturingStore{})

	batchID := "0194f0a0-0000-7000-8000-0000000000c1"
	records := cwRecords(t, batchID, 3)
	result, err := service.IngestRecords(context.Background(), cwEnvelope(), records)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Acknowledged || result.Accepted != 3 {
		t.Fatalf("result=%+v, want three accepted and acknowledged", result)
	}
	stats, err := j.Stats()
	if err != nil || stats.Pending != 3 {
		t.Fatalf("journal stats=%+v err=%v; an acknowledgement must mean a durable write", stats, err)
	}
}

// TestIngestRecordsRefusesAnEnvelopeForAnotherRegion pins that the second entry
// point cannot be used to move another region's records into this journal. The
// first entry point refuses this and so must this one.
func TestIngestRecordsRefusesAnEnvelopeForAnotherRegion(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	service := newService(t, clock, policy, openJournal(t, clock, policy), &capturingStore{})

	envelope := cwEnvelope()
	envelope.Region = "eu-central-1"
	_, err := service.IngestRecords(context.Background(), envelope,
		cwRecords(t, "0194f0a0-0000-7000-8000-0000000000c2", 1))
	if !errors.Is(err, pipeline.ErrScopeNotPermitted) {
		t.Fatalf("a foreign region was answered %v, want ErrScopeNotPermitted", err)
	}
}

// TestIngestRecordsRefusesRecordsThatDisagreeOnTheirBatch pins the journal's
// own precondition at the entry point instead of letting the whole append fail.
// The journal requires every record in one append to carry the batch identity
// it was appended under.
func TestIngestRecordsRefusesRecordsThatDisagreeOnTheirBatch(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	service := newService(t, clock, policy, openJournal(t, clock, policy), &capturingStore{})

	records := cwRecords(t, "0194f0a0-0000-7000-8000-0000000000c3", 2)
	records[1].BatchID = "0194f0a0-0000-7000-8000-0000000000c4"
	_, err := service.IngestRecords(context.Background(), cwEnvelope(), records)
	if !errors.Is(err, pipeline.ErrRequestRejected) {
		t.Fatalf("a mixed batch was answered %v, want ErrRequestRejected", err)
	}
}

// TestIngestRecordsRejectsAnOversizedRecordWithoutLosingItsSiblings pins that
// the record-local size bound is record-local here too. Failing the whole call
// would make a transport replay a batch whose one bad record can never shrink.
func TestIngestRecordsRejectsAnOversizedRecordWithoutLosingItsSiblings(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	service := newService(t, clock, policy, j, &capturingStore{})

	batchID := "0194f0a0-0000-7000-8000-0000000000c5"
	records := cwRecords(t, batchID, 3)
	records[1].Body = model.SafeString(strings.Repeat("a", 2<<20))

	result, err := service.IngestRecords(context.Background(), cwEnvelope(), records)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Acknowledged {
		t.Fatal("a batch with one oversized record was not acknowledged; its siblings were durable")
	}
	if result.Accepted != 2 || len(result.Rejected) != 1 {
		t.Fatalf("result=%+v, want two accepted and one rejected", result)
	}
	if result.Rejected[0].Index != 1 || result.Rejected[0].Reason != pipeline.RejectionRecordTooLarge {
		t.Fatalf("rejection=%+v, want index 1 rejected as too large", result.Rejected[0])
	}
	if got := service.Counters().RecordRejections.RecordTooLarge; got != 1 {
		t.Fatalf("record-too-large counter=%d, want 1", got)
	}
}

// TestIngestRecordsAcknowledgesAnAllRejectedBatchWithoutWriting pins the same
// acknowledgement boundary Ingest has: nothing durable, but nothing to retry
// either, so the source may move its checkpoint past them.
func TestIngestRecordsAcknowledgesAnAllRejectedBatchWithoutWriting(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	service := newService(t, clock, policy, j, &capturingStore{})

	records := cwRecords(t, "0194f0a0-0000-7000-8000-0000000000c6", 1)
	records[0].Body = model.SafeString(strings.Repeat("a", 2<<20))

	result, err := service.IngestRecords(context.Background(), cwEnvelope(), records)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Acknowledged || result.Accepted != 0 || len(result.Rejected) != 1 {
		t.Fatalf("result=%+v, want acknowledged with nothing accepted", result)
	}
	if stats, err := j.Stats(); err != nil || stats.Pending != 0 {
		t.Fatalf("journal stats=%+v err=%v; nothing should have been written", stats, err)
	}
}

func TestIngestRecordsWithNoRecordsIsAcknowledgedAndWritesNothing(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	service := newService(t, clock, policy, j, &capturingStore{})

	result, err := service.IngestRecords(context.Background(), cwEnvelope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Acknowledged || result.Accepted != 0 {
		t.Fatalf("result=%+v", result)
	}
	if stats, err := j.Stats(); err != nil || stats.Pending != 0 {
		t.Fatalf("journal stats=%+v err=%v", stats, err)
	}
}

// TestARepeatedCloudWatchEventContributesOneDurableRecord is the property the
// whole second entry point exists to preserve: the native event ID makes an
// overlapping replay harmless.
func TestARepeatedCloudWatchEventContributesOneDurableRecord(t *testing.T) {
	clock := fakeclock.NewAtOrigin()
	policy := redact.MinimalPolicy()
	j := openJournal(t, clock, policy)
	service := newService(t, clock, policy, j, &capturingStore{})

	first := cwRecords(t, "0194f0a0-0000-7000-8000-0000000000c7", 2)
	if _, err := service.IngestRecords(context.Background(), cwEnvelope(), first); err != nil {
		t.Fatal(err)
	}
	// The same events read again under a new batch, which is exactly what the
	// adapter's bounded lookback overlap produces after a lost checkpoint.
	second := cwRecords(t, "0194f0a0-0000-7000-8000-0000000000c8", 2)
	for i := range second {
		if second[i].RecordID != first[i].RecordID {
			t.Fatalf("the same event produced two record identities: %s and %s", first[i].RecordID, second[i].RecordID)
		}
	}
	result, err := service.IngestRecords(context.Background(), cwEnvelope(), second)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Acknowledged {
		t.Fatal("a replayed overlap was not acknowledged; the checkpoint could never advance")
	}
	stats, err := j.Stats()
	if err != nil || stats.Pending != 2 {
		t.Fatalf("journal stats=%+v err=%v; a replayed event must not become a second durable record", stats, err)
	}
}

// TestPipelineServiceSatisfiesTheCloudWatchSink is the wiring assertion: the
// coordinator is usable as the adapter's sink without an adapter in between.
func TestPipelineServiceSatisfiesTheCloudWatchSink(t *testing.T) {
	var _ interface {
		IngestRecords(context.Context, model.TrustedEnvelope, []model.NormalizedLog) (pipeline.IngestResult, error)
	} = (*pipeline.Service)(nil)
	// cloudwatch.Sink returns cloudwatch.Ack, so an adapter is still required to
	// convert the result. That adapter is cwsink; this test pins that the shapes
	// line up so the conversion is total.
	var sink cloudwatch.Sink
	_ = sink
}

func cwEnvelope() model.TrustedEnvelope {
	return model.TrustedEnvelope{
		SourceType: model.SourceTypeCloudWatch, SourceAccount: cwgen.DefaultAccount,
		Region: cwgen.DefaultRegion, AllowedEnvironments: []string{cwgen.DefaultEnvironment},
		AllowedServices: []string{cwgen.DefaultService}, SourceInstance: "cw-adapter-a",
		CredentialIdentity: "workload-a",
	}
}
