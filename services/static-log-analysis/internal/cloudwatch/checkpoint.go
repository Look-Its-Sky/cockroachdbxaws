package cloudwatch

import (
	"context"
	"fmt"
	"time"
)

// CheckpointSchemaVersion is the major.minor version of a persisted checkpoint.
// Readers accept unknown fields within the same major version; writers never
// change the meaning or type of an existing field.
const CheckpointSchemaVersion = "1.0"

// Checkpoint is the last source position for one stream that is known to have
// reached the analysis journal.
//
// It is deliberately not "the last position read". A checkpoint written before
// the journal acknowledged would silently drop records on the next restart,
// which is the one failure this whole design is arranged to prevent.
type Checkpoint struct {
	Stream Stream `json:"-"`
	// Position is the highest event time whose records reached the journal.
	// The next cycle starts before it, by the configured lookback, because
	// CloudWatch filters on event time and an event can be written late.
	Position time.Time `json:"position"`
	// EventID is the native identifier of the event at Position. It is
	// diagnostic: an operator comparing a checkpoint against the stream needs
	// the exact boundary, and a timestamp alone can name several events.
	EventID string `json:"event_id"`
	// CommittedAt is when this checkpoint was written, which is what checkpoint
	// age is measured from.
	CommittedAt time.Time `json:"committed_at"`
}

// CheckpointStore keeps checkpoints durably.
//
// Commit must return only once the write is durable. The whole overlap-reread
// design assumes that a checkpoint which Commit reported is one a restart will
// find, and that a checkpoint it did not report is one a restart will not.
type CheckpointStore interface {
	// LoadGroup returns every checkpoint held for a group. The adapter asks per
	// group rather than per stream because it does not know which streams exist
	// until it has read one.
	LoadGroup(ctx context.Context, group Group) ([]Checkpoint, error)
	Commit(ctx context.Context, checkpoints []Checkpoint) error
}

// Validate rejects a checkpoint that cannot be stored or reloaded meaningfully.
func (c Checkpoint) Validate() error {
	if !c.Stream.Group.valid() || !trimmed(c.Stream.Name) {
		return fmt.Errorf("%w: a checkpoint must name one account, region, group, and stream", ErrInvalidConfig)
	}
	if c.Position.IsZero() {
		return fmt.Errorf("%w: a checkpoint at the zero time would restart from the epoch", ErrInvalidConfig)
	}
	if c.CommittedAt.IsZero() {
		return fmt.Errorf("%w: checkpoint age cannot be measured without a commit time", ErrInvalidConfig)
	}
	return nil
}

// Age is how long ago a checkpoint was committed. A checkpoint that stops
// ageing is an adapter that has stopped making progress, which is the reason
// this value is a metric.
func (c Checkpoint) Age(now time.Time) time.Duration {
	if c.CommittedAt.IsZero() {
		return 0
	}
	return now.Sub(c.CommittedAt)
}
