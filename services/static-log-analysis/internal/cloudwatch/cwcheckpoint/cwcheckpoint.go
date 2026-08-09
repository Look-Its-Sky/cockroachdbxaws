// Package cwcheckpoint stores CloudWatch source checkpoints in Pebble.
//
// It is a separate package so that internal/cloudwatch, which decides when a
// checkpoint may move, does not import a storage engine. The decision and the
// durability are different concerns and fail in different ways.
package cwcheckpoint

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cockroachdb/pebble/v2"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

// keyPrefix namespaces checkpoints inside a database that may hold other
// things. It is part of the on-disk contract.
var keyPrefix = []byte("cw/ck/1/")

// ErrUnreadable means a stored checkpoint cannot be interpreted by this build.
// It is never repaired by guesswork: a checkpoint read wrongly moves a source
// position, and a source position moved wrongly loses records.
var ErrUnreadable = errors.New("cwcheckpoint: stored checkpoint is unreadable")

// Store is a Pebble-backed cloudwatch.CheckpointStore.
type Store struct {
	db *pebble.DB
}

var _ cloudwatch.CheckpointStore = (*Store)(nil)

// New wraps an open database. The caller owns the database's lifetime, because
// a replica shares one Pebble instance across its durable state.
func New(db *pebble.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: a checkpoint store needs an open database", cloudwatch.ErrInvalidConfig)
	}
	return &Store{db: db}, nil
}

// storedCheckpoint is the on-disk form. The stream is stored alongside the
// position as well as being encoded in the key, so a checkpoint found by a
// range scan can be returned without decoding its key back into components.
type storedCheckpoint struct {
	SchemaVersion string    `json:"schema_version"`
	Account       string    `json:"account"`
	Region        string    `json:"region"`
	LogGroup      string    `json:"log_group"`
	LogStream     string    `json:"log_stream"`
	Position      time.Time `json:"position"`
	EventID       string    `json:"event_id"`
	CommittedAt   time.Time `json:"committed_at"`
}

// Key returns the durable key one stream's checkpoint is stored under.
//
// Components are length-delimited rather than joined by a separator. A log
// group and a log stream are free-form operator-chosen names, so a separator
// would let one operator's naming choice move a boundary and make two distinct
// streams share a checkpoint. One of them would then skip records with nothing
// anywhere reporting it.
func Key(stream cloudwatch.Stream) []byte {
	key := append([]byte(nil), keyPrefix...)
	for _, component := range []string{
		stream.Group.Account, stream.Group.Region, stream.Group.LogGroup, stream.Name,
	} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(component)))
		key = append(key, length[:]...)
		key = append(key, component...)
	}
	return key
}

// groupPrefix is the key range every stream in one group lives under.
//
// It is Key without the stream component, which is a genuine prefix because the
// group's components come first and each carries its own length. One group's
// prefix can never be another's, however the two are named.
func groupPrefix(group cloudwatch.Group) []byte {
	key := Key(cloudwatch.Stream{Group: group})
	// Key appended a four-byte zero length for the absent stream name.
	return key[:len(key)-4]
}

// Commit writes every checkpoint in one synchronized batch.
//
// The batch is all or nothing and is synced before returning. Half a cycle's
// checkpoints would leave the other half rereading an overlap the adapter
// believed it had already passed, and an unsynced write would make the whole
// acknowledge-then-checkpoint ordering meaningless.
func (s *Store) Commit(ctx context.Context, checkpoints []cloudwatch.Checkpoint) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("%w: checkpoint store is not open", cloudwatch.ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(checkpoints) == 0 {
		return nil
	}
	// Every checkpoint is validated before anything is written, so a refusal
	// leaves the previous positions exactly as they were.
	encoded := make([][]byte, len(checkpoints))
	for i, checkpoint := range checkpoints {
		if err := checkpoint.Validate(); err != nil {
			return err
		}
		value, err := json.Marshal(storedCheckpoint{
			SchemaVersion: cloudwatch.CheckpointSchemaVersion,
			Account:       checkpoint.Stream.Group.Account,
			Region:        checkpoint.Stream.Group.Region,
			LogGroup:      checkpoint.Stream.Group.LogGroup,
			LogStream:     checkpoint.Stream.Name,
			Position:      checkpoint.Position.UTC(),
			EventID:       checkpoint.EventID,
			CommittedAt:   checkpoint.CommittedAt.UTC(),
		})
		if err != nil {
			return fmt.Errorf("cwcheckpoint: encoding a checkpoint: %w", err)
		}
		encoded[i] = value
	}

	batch := s.db.NewBatch()
	defer func() { _ = batch.Close() }()
	for i, checkpoint := range checkpoints {
		if err := batch.Set(Key(checkpoint.Stream), encoded[i], nil); err != nil {
			return fmt.Errorf("cwcheckpoint: staging a checkpoint: %w", err)
		}
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		return fmt.Errorf("cwcheckpoint: committing checkpoints: %w", err)
	}
	return nil
}

// LoadGroup returns every checkpoint in one group.
func (s *Store) LoadGroup(ctx context.Context, group cloudwatch.Group) ([]cloudwatch.Checkpoint, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("%w: checkpoint store is not open", cloudwatch.ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prefix := groupPrefix(group)
	iterator, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: upperBound(prefix),
	})
	if err != nil {
		return nil, fmt.Errorf("cwcheckpoint: reading checkpoints: %w", err)
	}
	defer func() { _ = iterator.Close() }()

	var checkpoints []cloudwatch.Checkpoint
	for iterator.First(); iterator.Valid(); iterator.Next() {
		var stored storedCheckpoint
		if err := json.Unmarshal(iterator.Value(), &stored); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnreadable, err)
		}
		version, err := model.ParseSchemaVersion(stored.SchemaVersion)
		if err != nil {
			return nil, fmt.Errorf("%w: schema version %q", ErrUnreadable, stored.SchemaVersion)
		}
		reader, err := model.ParseSchemaVersion(cloudwatch.CheckpointSchemaVersion)
		if err != nil {
			return nil, fmt.Errorf("%w: this build declares an invalid reader version", ErrUnreadable)
		}
		if !reader.CompatibleWith(version) {
			// A different major version means a field changed meaning or type.
			// Interpreting it under the old rules would move a source position
			// by a rule that no longer holds.
			return nil, fmt.Errorf("%w: schema version %s is not readable by %s",
				ErrUnreadable, version, reader)
		}
		checkpoint := cloudwatch.Checkpoint{
			Stream: cloudwatch.Stream{
				Group: cloudwatch.Group{
					Account: stored.Account, Region: stored.Region, LogGroup: stored.LogGroup,
				},
				Name: stored.LogStream,
			},
			Position:    stored.Position.UTC(),
			EventID:     stored.EventID,
			CommittedAt: stored.CommittedAt.UTC(),
		}
		if checkpoint.Stream.Group != group {
			// The key range already restricts the answer to this group. A value
			// disagreeing with its own key is corruption, not a stale write.
			return nil, fmt.Errorf("%w: a checkpoint names a group its key does not", ErrUnreadable)
		}
		checkpoints = append(checkpoints, checkpoint)
	}
	if err := iterator.Error(); err != nil {
		return nil, fmt.Errorf("cwcheckpoint: iterating checkpoints: %w", err)
	}
	return checkpoints, nil
}

// upperBound returns the exclusive end of a prefix scan.
func upperBound(prefix []byte) []byte {
	bound := append([]byte(nil), prefix...)
	for i := len(bound) - 1; i >= 0; i-- {
		if bound[i] < 0xff {
			bound[i]++
			return bound[:i+1]
		}
	}
	return nil
}
