// Package cwsink adapts the ingestion coordinator to the CloudWatch adapter's
// sink boundary.
//
// It is a separate package for the same reason cwaws is: internal/cloudwatch
// decides what to read and when a checkpoint may move, and must not depend on
// the coordinator to do it. The conversion here is small and total, which is
// the point — anything that needed judgement would belong on one side or the
// other, not in the seam.
package cwsink

import (
	"context"
	"errors"
	"fmt"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/pipeline"
)

// Coordinator is the ingestion entry point this sink delivers through.
// *pipeline.Service satisfies it.
type Coordinator interface {
	IngestRecords(context.Context, model.TrustedEnvelope, []model.NormalizedLog) (pipeline.IngestResult, error)
}

// Sink delivers mapped CloudWatch records into the coordinator.
type Sink struct{ coordinator Coordinator }

var _ cloudwatch.Sink = (*Sink)(nil)

func New(coordinator Coordinator) (*Sink, error) {
	if coordinator == nil {
		return nil, fmt.Errorf("%w: a sink needs a coordinator to ingest into", cloudwatch.ErrInvalidConfig)
	}
	return &Sink{coordinator: coordinator}, nil
}

// IngestRecords delivers one batch and reports whether it became durable.
//
// Acknowledged is passed through exactly as the coordinator reported it. The
// adapter moves no checkpoint without it, and inventing one here would be the
// single mistake the whole read-then-checkpoint ordering exists to prevent.
func (s *Sink) IngestRecords(ctx context.Context, envelope model.TrustedEnvelope, records []model.NormalizedLog) (cloudwatch.Ack, error) {
	if s == nil || s.coordinator == nil {
		return cloudwatch.Ack{}, fmt.Errorf("%w: sink is not built", cloudwatch.ErrInvalidConfig)
	}
	result, err := s.coordinator.IngestRecords(ctx, envelope, records)
	if err != nil {
		return cloudwatch.Ack{}, classify(err)
	}
	return cloudwatch.Ack{
		Acknowledged: result.Acknowledged,
		Accepted:     result.Accepted,
		Rejected:     len(result.Rejected),
	}, nil
}

// classify maps the coordinator's failures onto the adapter's vocabulary.
//
// Only one of them changes what an operator must do. A scope refusal means this
// adapter and this coordinator disagree about which region they serve, which is
// exactly the boundary crossing the adapter refuses on its own reads; naming it
// as such is what stops it being read as a transient ingestion problem.
//
// Everything else stays as it is and stops the cycle. The adapter advances no
// checkpoint on any error, so a permanent refusal parks the group for an
// operator instead of silently skipping records — the safe direction, because
// the alternative is a source that quietly moves past data nothing ever stored.
func classify(err error) error {
	switch {
	case errors.Is(err, pipeline.ErrScopeNotPermitted):
		return fmt.Errorf("%w: the coordinator does not serve this adapter's region: %w",
			cloudwatch.ErrRegionalBoundary, err)
	case errors.Is(err, pipeline.ErrJournalUnavailable):
		return fmt.Errorf("%w: %w", cloudwatch.ErrNotAcknowledged, err)
	default:
		return err
	}
}
