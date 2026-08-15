// Package testids provides a deterministic ids.Source.
//
// Every identifier is a structurally valid UUIDv7 and the sequence is strictly
// increasing, so tests that assert on ordering, on golden output, or on a
// specific identifier get the same answer on every run and on every machine.
package testids

import (
	"encoding/binary"
	"sync"

	"github.com/google/uuid"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/clock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/ids"
)

// maxSequence is the largest value the 12-bit rand_a field can hold. UUIDv7
// uses it as a within-millisecond counter.
const maxSequence = 0xFFF

// Source is a deterministic ids.Source.
//
// The millisecond field follows an injected clock when one is given, so
// identifiers sort consistently with the scenario's timeline. Within a
// millisecond a counter keeps them ordered, and once that counter is exhausted
// the millisecond field borrows from the future rather than repeating.
type Source struct {
	mu sync.Mutex
	// clock supplies the timestamp field. When nil, timestamps come from
	// startMillis plus the number of identifiers issued.
	clock clock.Clock
	// seed varies the pseudo-random field between sources so that two sources
	// in one test are distinguishable while each stays reproducible.
	seed uint64

	startMillis int64
	lastMillis  int64
	sequence    uint32
	issued      uint64
}

// Option configures a Source.
type Option func(*Source)

// WithClock ties identifier timestamps to a clock, normally the same fake clock
// the rest of the scenario uses.
func WithClock(c clock.Clock) Option { return func(s *Source) { s.clock = c } }

// WithSeed distinguishes one source from another. Two sources with the same
// seed and the same clock produce the same sequence.
func WithSeed(seed uint64) Option { return func(s *Source) { s.seed = seed } }

// WithStartMillis sets the timestamp the first identifier uses when no clock is
// injected.
func WithStartMillis(millis int64) Option { return func(s *Source) { s.startMillis = millis } }

// defaultStartMillis is 2026-01-01T00:00:00Z, matching fakeclock.Origin, so a
// clockless source and a clock-driven one produce identifiers from the same era.
const defaultStartMillis int64 = 1767225600000

// New returns a deterministic source.
func New(opts ...Option) *Source {
	s := &Source{startMillis: defaultStartMillis}
	for _, opt := range opts {
		opt(s)
	}
	s.lastMillis = -1
	return s
}

var _ ids.Source = (*Source)(nil)

// New returns the next identifier. It never fails; the error exists to satisfy
// ids.Source, whose production implementation can fail to read entropy.
func (s *Source) New() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	millis := s.nextMillisLocked()

	// Keep identifiers strictly increasing. Within one millisecond the sequence
	// counter orders them; when it overflows, move into the next millisecond
	// rather than emitting a duplicate or an out-of-order value.
	switch {
	case millis > s.lastMillis:
		s.lastMillis = millis
		s.sequence = 0
	case s.sequence < maxSequence:
		millis = s.lastMillis
		s.sequence++
	default:
		s.lastMillis++
		millis = s.lastMillis
		s.sequence = 0
	}

	id := uuid.UUID{}
	// Bytes 0-5: 48-bit big-endian millisecond timestamp.
	id[0] = byte(millis >> 40)
	id[1] = byte(millis >> 32)
	id[2] = byte(millis >> 24)
	id[3] = byte(millis >> 16)
	id[4] = byte(millis >> 8)
	id[5] = byte(millis)
	// Byte 6 high nibble: version 7. Remaining 12 bits: the sequence counter.
	id[6] = 0x70 | byte(s.sequence>>8&0x0F)
	id[7] = byte(s.sequence)
	// Byte 8 high bits: RFC 4122 variant. Remaining 62 bits: deterministic
	// filler that stands in for randomness without breaking ordering, since
	// (millis, sequence) already increases on every call.
	filler := mix(s.seed, s.issued)
	var tail [8]byte
	binary.BigEndian.PutUint64(tail[:], filler)
	id[8] = 0x80 | tail[0]&0x3F
	copy(id[9:], tail[1:])

	s.issued++
	return id.String(), nil
}

// nextMillisLocked returns the timestamp field for the next identifier.
func (s *Source) nextMillisLocked() int64 {
	if s.clock != nil {
		return s.clock.Now().UnixMilli()
	}
	return s.startMillis + int64(s.issued)
}

// Issued returns how many identifiers this source has produced.
func (s *Source) Issued() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issued
}

// mix is splitmix64, used only to spread the filler bits so that identifiers
// do not look like counters. It carries no security requirement.
func mix(seed, counter uint64) uint64 {
	z := seed + (counter+1)*0x9E3779B97F4A7C15
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}
