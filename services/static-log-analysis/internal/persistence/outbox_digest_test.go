package persistence

import (
	"encoding/hex"
	"reflect"
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/queue"
)

func TestOutboxContentDigestIsVersionedCanonicalAndCoversEveryField(t *testing.T) {
	base := queue.Message{
		MessageID: "message-1", DeduplicationKey: "dedup-1", Type: "type-1", Body: []byte("payload-1"),
		Attributes: map[string]string{"z": "last", "a": "first"},
	}
	digest := outboxContentDigest(base, "aggregate-type", "aggregate-id", "1.0")
	const wantHex = "2db0c65ff24a6776a91e8bc9bf0853f5e76268cd976127dd403ba8c10046e0a7"
	if got := hex.EncodeToString(digest[:]); got != wantHex {
		t.Fatalf("canonical digest vector changed: want %s got %s", wantHex, got)
	}
	reordered := base
	reordered.Attributes = map[string]string{"a": "first", "z": "last"}
	if got := outboxContentDigest(reordered, "aggregate-type", "aggregate-id", "1.0"); got != digest {
		t.Fatal("attribute insertion order changed canonical digest")
	}

	tests := []struct {
		name   string
		mutate func(*queue.Message, *string, *string, *string)
	}{
		{name: "message id", mutate: func(m *queue.Message, _, _, _ *string) { m.MessageID += "x" }},
		{name: "deduplication key", mutate: func(m *queue.Message, _, _, _ *string) { m.DeduplicationKey += "x" }},
		{name: "message type", mutate: func(m *queue.Message, _, _, _ *string) { m.Type += "x" }},
		{name: "aggregate type", mutate: func(_ *queue.Message, value, _, _ *string) { *value += "x" }},
		{name: "aggregate id", mutate: func(_ *queue.Message, _, value, _ *string) { *value += "x" }},
		{name: "payload version", mutate: func(_ *queue.Message, _, _, value *string) { *value = "1.1" }},
		{name: "exact payload bytes", mutate: func(m *queue.Message, _, _, _ *string) { m.Body = append(m.Body, ' ') }},
		{name: "attribute key", mutate: func(m *queue.Message, _, _, _ *string) {
			delete(m.Attributes, "a")
			m.Attributes["b"] = "first"
		}},
		{name: "attribute value", mutate: func(m *queue.Message, _, _, _ *string) { m.Attributes["a"] = "other" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message := base
			message.Body = append([]byte(nil), base.Body...)
			message.Attributes = map[string]string{"z": "last", "a": "first"}
			aggregateType, aggregateID, payloadVersion := "aggregate-type", "aggregate-id", "1.0"
			test.mutate(&message, &aggregateType, &aggregateID, &payloadVersion)
			if got := outboxContentDigest(message, aggregateType, aggregateID, payloadVersion); reflect.DeepEqual(got, digest) {
				t.Fatalf("%s was not covered by digest", test.name)
			}
		})
	}
}

func TestOutboxDigestLengthDelimitingPreventsFieldBoundaryAmbiguity(t *testing.T) {
	left := queue.Message{MessageID: "a", DeduplicationKey: "bc"}
	right := queue.Message{MessageID: "ab", DeduplicationKey: "c"}
	if outboxContentDigest(left, "", "", "") == outboxContentDigest(right, "", "", "") {
		t.Fatal("length-delimited digest collapsed different field boundaries")
	}
}
