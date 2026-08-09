package cwsink_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch/cwsink"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/pipeline"
)

// fakeCoordinator answers whatever the test wants the coordinator to have
// answered. What the coordinator actually does with records is covered against
// a real journal in internal/pipeline; what this pins is the conversion.
type fakeCoordinator struct {
	result pipeline.IngestResult
	err    error
	calls  int
}

func (f *fakeCoordinator) IngestRecords(context.Context, model.TrustedEnvelope, []model.NormalizedLog) (pipeline.IngestResult, error) {
	f.calls++
	return f.result, f.err
}

func TestAnAcknowledgedBatchIsReportedAcknowledgedWithItsCounts(t *testing.T) {
	coordinator := &fakeCoordinator{result: pipeline.IngestResult{
		Acknowledged: true, Accepted: 4,
		Rejected: []pipeline.RecordRejection{{Index: 1}, {Index: 2}},
	}}
	sink, err := cwsink.New(coordinator)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := sink.IngestRecords(context.Background(), model.TrustedEnvelope{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ack.Acknowledged || ack.Accepted != 4 || ack.Rejected != 2 {
		t.Fatalf("ack=%+v, want acknowledged with 4 accepted and 2 rejected", ack)
	}
}

// TestAnUnacknowledgedResultIsNeverUpgraded is the load-bearing one. The
// adapter moves its checkpoint on Acknowledged alone, so a sink that invented
// one would let a source advance past records nothing ever stored.
func TestAnUnacknowledgedResultIsNeverUpgraded(t *testing.T) {
	coordinator := &fakeCoordinator{result: pipeline.IngestResult{Acknowledged: false, Accepted: 3}}
	sink, _ := cwsink.New(coordinator)

	ack, err := sink.IngestRecords(context.Background(), model.TrustedEnvelope{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Acknowledged {
		t.Fatal("an unacknowledged ingest was reported acknowledged; the checkpoint would advance past records that were never durable")
	}
}

func TestAFailureIsNeverReportedAsAnAcknowledgement(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		want error
	}{
		{"scope refused", pipeline.ErrScopeNotPermitted, cloudwatch.ErrRegionalBoundary},
		{"journal unavailable", pipeline.ErrJournalUnavailable, cloudwatch.ErrNotAcknowledged},
		{"journal refused permanently", pipeline.ErrJournalRejected, pipeline.ErrJournalRejected},
		{"request refused", pipeline.ErrRequestRejected, pipeline.ErrRequestRejected},
		{"unrecognized", errors.New("something new"), nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			sink, _ := cwsink.New(&fakeCoordinator{err: testCase.err})
			ack, err := sink.IngestRecords(context.Background(), model.TrustedEnvelope{}, nil)
			if err == nil {
				t.Fatal("a failed ingest returned no error")
			}
			if ack.Acknowledged {
				t.Fatal("a failed ingest was reported acknowledged")
			}
			if testCase.want != nil && !errors.Is(err, testCase.want) {
				t.Fatalf("error %v does not carry %v", err, testCase.want)
			}
		})
	}
}

func TestASinkWithoutACoordinatorIsRefusedRatherThanBuilt(t *testing.T) {
	if _, err := cwsink.New(nil); !errors.Is(err, cloudwatch.ErrInvalidConfig) {
		t.Fatalf("a sink with nowhere to deliver was built: %v", err)
	}
}
