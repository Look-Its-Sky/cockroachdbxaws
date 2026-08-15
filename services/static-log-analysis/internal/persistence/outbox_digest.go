package persistence

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"sort"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/queue"
)

const outboxContentDigestDomain = "static-log-analysis/outbox-content-digest:v1"

// outboxContentDigest binds every immutable byte or routing value published by
// the outbox. Each component is length-delimited and the attributes map has a
// canonical key order, so neither concatenation ambiguity nor JSONB map order
// can change the identity.
func outboxContentDigest(message queue.Message, aggregateType, aggregateID, payloadVersion string) [sha256.Size]byte {
	digest := sha256.New()
	writeDigestPart(digest, []byte(outboxContentDigestDomain))
	writeDigestPart(digest, []byte(message.MessageID))
	writeDigestPart(digest, []byte(message.DeduplicationKey))
	writeDigestPart(digest, []byte(message.Type))
	writeDigestPart(digest, []byte(aggregateType))
	writeDigestPart(digest, []byte(aggregateID))
	writeDigestPart(digest, []byte(payloadVersion))
	writeDigestPart(digest, message.Body)
	writeDigestPart(digest, canonicalOutboxAttributes(message.Attributes))
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result
}

func canonicalOutboxAttributes(attributes map[string]string) []byte {
	keys := make([]string, 0, len(attributes))
	for key := range attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var canonical bytes.Buffer
	writeUint64(&canonical, uint64(len(keys)))
	for _, key := range keys {
		writeBufferPart(&canonical, []byte(key))
		writeBufferPart(&canonical, []byte(attributes[key]))
	}
	return canonical.Bytes()
}

func validAssignmentAttributes(attributes map[string]string, region string) bool {
	if len(attributes) != 1 {
		return false
	}
	return attributes["region"] == region
}

func writeDigestPart(destination hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = destination.Write(size[:])
	_, _ = destination.Write(value)
}

func writeBufferPart(destination *bytes.Buffer, value []byte) {
	writeUint64(destination, uint64(len(value)))
	_, _ = destination.Write(value)
}

func writeUint64(destination *bytes.Buffer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = destination.Write(encoded[:])
}
