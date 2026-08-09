// Package journal owns the durable, replica-local boundary between admission
// and downstream processing. It persists only structurally valid, finally
// scanned NormalizedLog values; raw OTLP never crosses this package boundary.
package journal

import (
	"errors"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/clock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

const (
	FormatMajor = 1
	FormatMinor = 0

	DefaultClaimTTL    = 5 * time.Minute
	DefaultSafetyDelay = 15 * time.Minute
	// DefaultEntryOverhead is charged for each logical key/value pair in
	// addition to its exact encoded bytes. It conservatively covers Pebble's
	// internal-key, WAL, index, and allocator amplification without pretending
	// to be a filesystem measurement.
	DefaultEntryOverhead uint64 = 64
	DefaultMaxBatchRefs         = 64
	MaxOwnerBytes               = 128
	MaxBoundaryBytes            = 128
	claimTokenBytes             = len("claim:") + 64
	// A maximum-sized claim owner adds 128 bytes and grows its length prefix
	// from one to two bytes. The fixed claim token adds 70 bytes without
	// growing its one-byte length prefix, while claim keys are one byte shorter
	// than pending keys. The reserve payload therefore covers the exact largest
	// dynamic transition delta (128 + 1 + 70 - 1), before the reserve key,
	// checksum, and entry overhead provide additional conservatism.
	transitionReserveBytes = MaxOwnerBytes + 1 + claimTokenBytes - 1
	MaxEncodedRecordBytes  = 256 << 10
	MaxAppendRecords       = 10_000
	MaxTransitionRecords   = 1_000
)

var (
	ErrInvalidConfig      = errors.New("journal: invalid configuration")
	ErrInvalidInput       = errors.New("journal: invalid input")
	ErrCapacity           = errors.New("journal: capacity unavailable")
	ErrOwnerMismatch      = errors.New("journal: owner mismatch")
	ErrBoundaryMismatch   = errors.New("journal: immutable boundary mismatch")
	ErrIncompatibleFormat = errors.New("journal: incompatible format")
	ErrLocked             = errors.New("journal: directory locked")
	ErrDuplicateConflict  = errors.New("journal: identity conflict")
	ErrNotHealthy         = errors.New("journal: unavailable")
	ErrCorruption         = errors.New("journal: corruption detected")
	ErrStaleClaim         = errors.New("journal: stale claim")
	ErrClosed             = errors.New("journal: closed")
)

// Validator is the mandatory final prohibited-content boundary. Implementations
// must return a categorical error and must not mutate the record. Version must
// identify the exact policy which produced every accepted record. The journal
// trusts this implementation; a no-op validator is suitable only for tests and
// cannot provide the production prohibited-content guarantee.
type Validator interface {
	ValidateRecord(model.NormalizedLog) error
	Version() string
}

// FreeSpace reports currently available bytes for a journal directory.
type FreeSpace func(path string) (uint64, error)

type Config struct {
	Dir      string
	Owner    string
	TenantID string
	Region   string
	// Classification is the highest classification permitted in this
	// single-tenant journal (for example, SENSITIVE).
	Classification string
	Clock          clock.Clock
	Validator      Validator

	MaxBytes      uint64
	MinFreeBytes  uint64
	FreeSpace     FreeSpace
	ClaimTTL      time.Duration
	SafetyDelay   time.Duration
	EntryOverhead uint64
	MaxBatchRefs  int
}

// Manifest is the immutable boundary fixed when the journal directory was
// created. A coordinator cross-checks it against its own configuration at
// startup, because a contradiction is a deployment error rather than something
// individual records should discover one at a time.
type Manifest struct {
	Region         string
	TenantID       string
	Classification string
	PolicyVersion  string
}

// Priority sorts lexicographically in pending indexes. Lower values are
// claimed first; received time and record ID break ties deterministically.
type Priority uint8

const (
	PriorityCritical Priority = iota
	PriorityHigh
	PriorityNormal
	PriorityLow
)

type Admission struct {
	Record   model.NormalizedLog
	Priority Priority
}

type State uint8

const (
	StatePending State = iota + 1
	StateClaimed
	StateCommitted
	StateQuarantined
)

type ClaimedRecord struct {
	Record    model.NormalizedLog
	Priority  Priority
	Replay    ReplayIdentity
	Token     string
	Owner     string
	Attempt   uint32
	ExpiresAt time.Time
}

const ReplayIdentityVersion = "journal:replay:v1"

// ReplayIdentity is the sealed M3 equivalence value handed to downstream
// persistence. It binds the exact durable envelope digest and admission
// priority used by journal replay-conflict detection.
type ReplayIdentity struct {
	version  string
	digest   [32]byte
	priority Priority
}

func (r ReplayIdentity) Version() string    { return r.version }
func (r ReplayIdentity) Digest() [32]byte   { return r.digest }
func (r ReplayIdentity) Priority() Priority { return r.priority }

type CommitClaim struct {
	RecordID string
	Token    string
}

// RecoveredRecord is the safe retained projection used to rebuild deterministic
// in-memory rule topology after a process restart. StateCommitted records warm
// rule state but are never eligible to be persisted again.
type RecoveredRecord struct {
	Record model.NormalizedLog
	State  State
}

// QuarantineReason is deliberately categorical. It is persisted and may be
// surfaced to operators, so arbitrary error text is not accepted.
type QuarantineReason uint8

const (
	QuarantineInvalidNormalizedRecord QuarantineReason = iota + 1
	QuarantineDeterministicProcessing
	QuarantineUnsupportedData
)

// Capacity is what an ingestion path needs in order to decide whether a record
// may be shed before it is written. CanPersist is false when the journal is at
// its configured limit, below its filesystem headroom, or unusable.
type Capacity struct {
	Used       uint64
	Total      uint64
	CanPersist bool
}

type Stats struct {
	AccountedBytes uint64
	ByPriority     [4]uint64
	Pending        uint64
	Claimed        uint64
	Committed      uint64
	Quarantined    uint64
}
