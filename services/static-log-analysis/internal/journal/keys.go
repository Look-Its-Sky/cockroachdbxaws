package journal

import (
	"encoding/binary"
	"time"
)

const keyVersion byte = 1

const (
	nsManifest byte = iota + 1
	nsBatch
	nsRecord
	nsPending
	nsClaim
	nsCommitted
	nsQuarantine
	nsBatchRef
	nsTransitionReserve
	nsBatchOriginalRef
)

var (
	keyFormat   = key(nsManifest, "format")
	keyOwner    = key(nsManifest, "owner")
	keyUsage    = key(nsManifest, "usage")
	keyPriority = key(nsManifest, "priority")
	keyBoundary = key(nsManifest, "boundary")
)

func prefix(namespace byte) []byte { return []byte{keyVersion, namespace} }

func key(namespace byte, components ...string) []byte {
	out := prefix(namespace)
	for _, component := range components {
		out = binary.AppendUvarint(out, uint64(len(component)))
		out = append(out, component...)
	}
	return out
}

func batchKey(batchID string) []byte              { return key(nsBatch, batchID) }
func recordKey(recordID string) []byte            { return key(nsRecord, recordID) }
func quarantineKey(recordID string) []byte        { return key(nsQuarantine, recordID) }
func batchRefKey(recordID, batchID string) []byte { return key(nsBatchRef, recordID, batchID) }
func batchRefPrefix(recordID string) []byte       { return key(nsBatchRef, recordID) }
func transitionReserveKey(recordID string) []byte { return key(nsTransitionReserve, recordID) }
func batchOriginalRefKey(recordID, batchID string) []byte {
	return key(nsBatchOriginalRef, recordID, batchID)
}
func batchOriginalRefPrefix(recordID string) []byte { return key(nsBatchOriginalRef, recordID) }

func pendingKey(priority Priority, received time.Time, recordID string) []byte {
	out := prefix(nsPending)
	out = append(out, byte(priority))
	out = appendOrderedTime(out, received)
	return appendComponent(out, recordID)
}

func claimKey(expiry time.Time, recordID string) []byte {
	out := appendOrderedTime(prefix(nsClaim), expiry)
	return appendComponent(out, recordID)
}

func committedKey(at time.Time, recordID string) []byte {
	out := appendOrderedTime(prefix(nsCommitted), at)
	return appendComponent(out, recordID)
}

func appendOrderedTime(out []byte, at time.Time) []byte {
	// Flip the sign bit so signed Unix nanos retain chronological byte order.
	return binary.BigEndian.AppendUint64(out, uint64(at.UnixNano())^(uint64(1)<<63))
}

func appendComponent(out []byte, value string) []byte {
	out = binary.AppendUvarint(out, uint64(len(value)))
	return append(out, value...)
}

func readComponent(in []byte) (string, []byte, bool) {
	size, n := binary.Uvarint(in)
	if n <= 0 || size > uint64(len(in[n:])) {
		return "", nil, false
	}
	return string(in[n : n+int(size)]), in[n+int(size):], true
}

func parsePendingKey(in []byte) (Priority, time.Time, string, bool) {
	if len(in) < 11 || in[0] != keyVersion || in[1] != nsPending {
		return 0, time.Time{}, "", false
	}
	priority := Priority(in[2])
	nanos := int64(binary.BigEndian.Uint64(in[3:11]) ^ (uint64(1) << 63))
	id, rest, ok := readComponent(in[11:])
	return priority, time.Unix(0, nanos).UTC(), id, ok && len(rest) == 0
}

func parseTimedKey(in []byte, namespace byte) (time.Time, string, bool) {
	if len(in) < 10 || in[0] != keyVersion || in[1] != namespace {
		return time.Time{}, "", false
	}
	nanos := int64(binary.BigEndian.Uint64(in[2:10]) ^ (uint64(1) << 63))
	id, rest, ok := readComponent(in[10:])
	return time.Unix(0, nanos).UTC(), id, ok && len(rest) == 0
}

func parseTwoComponentKey(in []byte, namespace byte) (string, string, bool) {
	if len(in) < 2 || in[0] != keyVersion || in[1] != namespace {
		return "", "", false
	}
	first, rest, ok := readComponent(in[2:])
	if !ok {
		return "", "", false
	}
	second, rest, ok := readComponent(rest)
	return first, second, ok && len(rest) == 0
}
