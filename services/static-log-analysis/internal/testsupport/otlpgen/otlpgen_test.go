package otlpgen_test

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	common "go.opentelemetry.io/proto/otlp/common/v1"
	logs "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/ids"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/golden"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/otlpgen"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/testids"
)

func TestTwoProducersEmitByteIdenticalPayloads(t *testing.T) {
	first := encode(t, otlpgen.New())
	second := encode(t, otlpgen.New())

	// Without this, a duplicate-delivery test could not tell a genuine
	// difference from generator noise.
	if !bytes.Equal(first, second) {
		t.Fatalf("producers with the same configuration disagree: %d and %d bytes", len(first), len(second))
	}
}

func encode(t *testing.T, producer *otlpgen.Producer) []byte {
	t.Helper()
	request := producer.Request(producer.PaymentError(), producer.Record())
	encoded, err := otlpgen.Encode(request)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	return encoded
}

func TestARecordCarriesAValidRecordUID(t *testing.T) {
	producer := otlpgen.New()

	record := producer.Record()

	uid, ok := otlpgen.RecordUID(record)
	if !ok {
		t.Fatal("want a record uid set before the first export attempt, got none")
	}
	// The uid is a uniqueness claim the service validates, so a generator that
	// emitted something unparseable would test the rejection path by accident.
	if err := ids.Validate(uid); err != nil {
		t.Fatalf("want a canonical UUIDv7, got %s: %v", uid, err)
	}
}

func TestEachRecordGetsItsOwnUID(t *testing.T) {
	producer := otlpgen.New()

	first, _ := otlpgen.RecordUID(producer.Record())
	second, _ := otlpgen.RecordUID(producer.Record())

	if first == second {
		t.Fatalf("want distinct record uids, both were %s", first)
	}
}

func TestADuplicateKeepsTheSameIdentityAndBytes(t *testing.T) {
	producer := otlpgen.New()
	original := producer.PaymentError()

	duplicate := otlpgen.Duplicate(original)

	// A Collector retrying after a lost acknowledgement resends the same
	// record. A fresh uid here would mean the duplicate could never be
	// recognised, and the retry test would pass for the wrong reason.
	originalUID, _ := otlpgen.RecordUID(original)
	duplicateUID, _ := otlpgen.RecordUID(duplicate)
	if originalUID != duplicateUID {
		t.Fatalf("want the same record uid, got %s and %s", originalUID, duplicateUID)
	}
	if !proto.Equal(original, duplicate) {
		t.Fatal("want a byte-identical duplicate")
	}
}

func TestADuplicateIsIndependentOfItsOriginal(t *testing.T) {
	producer := otlpgen.New()
	original := producer.Record()
	duplicate := otlpgen.Duplicate(original)

	original.SeverityText = "CHANGED"

	if duplicate.GetSeverityText() == "CHANGED" {
		t.Fatal("want the duplicate unaffected by changes to its original")
	}
}

func TestTwoRecordsCanClaimTheSameIdentity(t *testing.T) {
	producer := otlpgen.New()
	uid, _ := otlpgen.RecordUID(producer.Record())

	// Deduplication has to hold when the same identity arrives with different
	// content, not only when the bytes match.
	restated := producer.Record(otlpgen.WithRecordUID(uid), otlpgen.WithBody("different text"))

	restatedUID, _ := otlpgen.RecordUID(restated)
	if restatedUID != uid {
		t.Fatalf("want the given uid %s, got %s", uid, restatedUID)
	}
}

func TestReplaySequenceRepeatsTheSameRequest(t *testing.T) {
	producer := otlpgen.New()
	request := producer.Request(producer.PaymentError())

	replays := otlpgen.ReplaySequence(request, 3)

	if len(replays) != 3 {
		t.Fatalf("want 3 replays, got %d", len(replays))
	}
	first, err := otlpgen.Encode(replays[0])
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	for i, replay := range replays {
		encoded, err := otlpgen.Encode(replay)
		if err != nil {
			t.Fatalf("encoding replay %d: %v", i, err)
		}
		if !bytes.Equal(first, encoded) {
			t.Fatalf("replay %d differs from the first", i)
		}
	}
}

func TestTimesFollowTheScenarioClock(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	producer := otlpgen.New(otlpgen.WithClock(c), otlpgen.WithIDs(testids.New(testids.WithClock(c))))

	first := producer.Record()
	c.Advance(5 * time.Minute)
	second := producer.Record()

	if want := uint64(fakeclock.Origin.UnixNano()); first.GetObservedTimeUnixNano() != want {
		t.Fatalf("want the first record observed at %d, got %d", want, first.GetObservedTimeUnixNano())
	}
	want := uint64(fakeclock.Origin.Add(5 * time.Minute).UnixNano())
	if second.GetObservedTimeUnixNano() != want {
		t.Fatalf("want the second record observed at %d, got %d", want, second.GetObservedTimeUnixNano())
	}
	if second.GetTimeUnixNano() >= second.GetObservedTimeUnixNano() {
		t.Fatalf("want event time before observed time, got %d and %d",
			second.GetTimeUnixNano(), second.GetObservedTimeUnixNano())
	}
}

func TestARecordCanBePlacedAtAnExactInstant(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	producer := otlpgen.New(otlpgen.WithClock(c))
	boundary := uint64(fakeclock.Origin.Add(5 * time.Minute).UnixNano())

	at := producer.Record(otlpgen.AtEventTime(boundary))
	before := producer.Record(otlpgen.AtEventTime(boundary - 1))
	after := producer.Record(otlpgen.AtEventTime(boundary + 1))

	// Windows are half-open, so a scenario has to be able to place a record on
	// the boundary itself and one nanosecond either side of it.
	if at.GetTimeUnixNano() != boundary {
		t.Errorf("want a record exactly at %d, got %d", boundary, at.GetTimeUnixNano())
	}
	if before.GetTimeUnixNano() != boundary-1 || after.GetTimeUnixNano() != boundary+1 {
		t.Errorf("want records either side of the boundary, got %d and %d",
			before.GetTimeUnixNano(), after.GetTimeUnixNano())
	}
}

func TestThePaymentErrorCarriesAStackTraceWithApplicationAndLibraryFrames(t *testing.T) {
	record := otlpgen.New().PaymentError()

	stack, ok := otlpgen.AttributeValue(record.GetAttributes(), "exception.stacktrace")
	if !ok {
		t.Fatal("want a stack trace on the representative payment error, got none")
	}
	text := stack.GetStringValue()
	// A fingerprint keeps in-application frames and excludes library frames, so
	// the representative record has to contain both.
	if !contains(text, "payment/charge.go") {
		t.Errorf("want an in-application frame, got:\n%s", text)
	}
	if !contains(text, "net/http/server.go") {
		t.Errorf("want a library frame, got:\n%s", text)
	}
	if _, ok := otlpgen.AttributeValue(record.GetAttributes(), "exception.type"); !ok {
		t.Error("want an exception type")
	}
}

func TestThePaymentErrorCarriesDynamicValuesAFingerprintMustIgnore(t *testing.T) {
	record := otlpgen.New().PaymentError()

	// Two occurrences differing only in request id must fingerprint the same,
	// so the representative record has to carry values of that shape.
	if _, ok := otlpgen.AttributeValue(record.GetAttributes(), "request.id"); !ok {
		t.Error("want a request identifier")
	}
	message, ok := otlpgen.AttributeValue(record.GetAttributes(), "exception.message")
	if !ok {
		t.Fatal("want an exception message")
	}
	if !contains(message.GetStringValue(), "customer") {
		t.Errorf("want a customer identifier embedded in the message, got %q", message.GetStringValue())
	}
}

func TestTracesCanBeSharedOrDistinct(t *testing.T) {
	producer := otlpgen.New()
	sharedTrace, sharedSpan := producer.NextTrace()
	otherTrace, _ := producer.NextTrace()

	first := producer.Record(otlpgen.WithTrace(sharedTrace, sharedSpan))
	second := producer.Record(otlpgen.WithTrace(sharedTrace, sharedSpan))

	if !bytes.Equal(first.GetTraceId(), second.GetTraceId()) {
		t.Error("want two records to be able to share a trace")
	}
	if bytes.Equal(sharedTrace, otherTrace) {
		t.Error("want successive traces to be distinct")
	}
	if len(sharedTrace) != 16 || len(sharedSpan) != 8 {
		t.Fatalf("want a 16-byte trace and an 8-byte span, got %d and %d", len(sharedTrace), len(sharedSpan))
	}
}

func TestMalformedRecords(t *testing.T) {
	producer := otlpgen.New()

	tests := []struct {
		kind   otlpgen.MalformedKind
		assert func(*testing.T, *logs.LogRecord)
	}{
		{
			kind: otlpgen.MalformedNoIdentity,
			assert: func(t *testing.T, r *logs.LogRecord) {
				if _, ok := otlpgen.RecordUID(r); ok {
					t.Error("want no record uid")
				}
				if r.GetBody() != nil || r.GetObservedTimeUnixNano() != 0 {
					t.Error("want nothing left to derive an identity from")
				}
			},
		},
		{
			kind: otlpgen.MalformedInvalidUID,
			assert: func(t *testing.T, r *logs.LogRecord) {
				uid, ok := otlpgen.RecordUID(r)
				if !ok {
					t.Fatal("want a record uid present but invalid")
				}
				if err := ids.Validate(uid); err == nil {
					t.Errorf("want %s to fail validation", uid)
				}
			},
		},
		{
			kind: otlpgen.MalformedNoTimestamps,
			assert: func(t *testing.T, r *logs.LogRecord) {
				if r.GetTimeUnixNano() != 0 || r.GetObservedTimeUnixNano() != 0 {
					t.Error("want both timestamps absent")
				}
			},
		},
		{
			kind: otlpgen.MalformedNoContent,
			assert: func(t *testing.T, r *logs.LogRecord) {
				if r.GetBody() != nil || r.GetEventName() != "" {
					t.Error("want neither a body nor an event name")
				}
			},
		},
		{
			kind: otlpgen.MalformedShortTraceID,
			assert: func(t *testing.T, r *logs.LogRecord) {
				if len(r.GetTraceId()) == 16 {
					t.Error("want a trace id of the wrong length")
				}
			},
		},
		{
			kind: otlpgen.MalformedEmptyAttributeKey,
			assert: func(t *testing.T, r *logs.LogRecord) {
				if _, ok := otlpgen.AttributeValue(r.GetAttributes(), ""); !ok {
					t.Error("want an attribute with no name")
				}
			},
		},
		{
			kind: otlpgen.MalformedDuplicateAttributeKey,
			assert: func(t *testing.T, r *logs.LogRecord) {
				if count := countKey(r.GetAttributes(), "payment.provider"); count < 2 {
					t.Errorf("want the key repeated, got %d occurrences", count)
				}
			},
		},
		{
			kind: otlpgen.MalformedFutureEventTime,
			assert: func(t *testing.T, r *logs.LogRecord) {
				if r.GetTimeUnixNano() <= r.GetObservedTimeUnixNano() {
					t.Error("want an event time ahead of observed time")
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(string(test.kind), func(t *testing.T) {
			test.assert(t, producer.Malformed(test.kind))
		})
	}
}

func TestAMalformedRecordCanTravelWithValidSiblings(t *testing.T) {
	producer := otlpgen.New()

	request := producer.Request(
		producer.PaymentError(),
		producer.Malformed(otlpgen.MalformedNoContent),
		producer.PaymentError(),
	)

	// Valid attributes survive malformed siblings, so a batch has to be able to
	// mix them.
	records := request.GetResourceLogs()[0].GetScopeLogs()[0].GetLogRecords()
	if len(records) != 3 {
		t.Fatalf("want 3 records in the batch, got %d", len(records))
	}
}

func TestOversizedRecordsExceedTheRequestedSize(t *testing.T) {
	producer := otlpgen.New()

	for _, size := range []int{1 << 10, 256 << 10, 1 << 20} {
		record := producer.Oversized(size)
		if got := proto.Size(record); got < size {
			t.Errorf("want at least %d bytes, got %d", size, got)
		}
	}
}

func TestDeepAttributesReachTheRequestedDepth(t *testing.T) {
	producer := otlpgen.New()

	for _, depth := range []int{1, 2, 16, 17} {
		record := producer.DeepAttributes(depth)
		value, ok := otlpgen.AttributeValue(record.GetAttributes(), "nested")
		if !ok {
			t.Fatalf("depth %d: want a nested attribute", depth)
		}
		if got := valueDepth(value); got != depth {
			t.Errorf("want depth %d, got %d", depth, got)
		}
	}
}

func TestARecordCanOmitItsUIDToForceDerivedIdentity(t *testing.T) {
	record := otlpgen.New().Record(otlpgen.WithoutRecordUID())

	if _, ok := otlpgen.RecordUID(record); ok {
		t.Fatal("want no record uid, got one")
	}
	// The derived fallback still needs stable inputs, so the rest of the record
	// must remain intact.
	if record.GetBody() == nil || record.GetObservedTimeUnixNano() == 0 {
		t.Fatal("want the remaining identity inputs preserved")
	}
}

func TestTheRequestCarriesResourceAndScopeIdentity(t *testing.T) {
	producer := otlpgen.New(otlpgen.WithService("cartservice"), otlpgen.WithRegion("eu-west-1"))

	request := producer.Request(producer.Record())

	resourceLogs := request.GetResourceLogs()[0]
	attributes := resourceLogs.GetResource().GetAttributes()
	if value, _ := otlpgen.AttributeValue(attributes, "service.name"); value.GetStringValue() != "cartservice" {
		t.Errorf("want the configured service, got %q", value.GetStringValue())
	}
	if value, _ := otlpgen.AttributeValue(attributes, "cloud.region"); value.GetStringValue() != "eu-west-1" {
		t.Errorf("want the configured region, got %q", value.GetStringValue())
	}
	if scope := resourceLogs.GetScopeLogs()[0].GetScope(); scope.GetName() == "" {
		t.Error("want an instrumentation scope")
	}
}

func TestTheRepresentativePaymentErrorMatchesItsFixture(t *testing.T) {
	producer := otlpgen.New()

	// The payment error is the input to the first normalization, fingerprint,
	// and vertical slice tests. A change to it has to be reviewed rather than
	// discovered later as a rule that stopped matching.
	golden.JSON(t, "payment_error.json", summarize(producer.PaymentError()))
}

// summary is a readable projection of a record. The protobuf JSON encoder
// deliberately varies its whitespace, so it cannot be compared against a
// fixture directly.
type summary struct {
	SeverityNumber int32             `json:"severity_number"`
	SeverityText   string            `json:"severity_text"`
	Body           string            `json:"body"`
	EventTime      uint64            `json:"event_time_unix_nano"`
	ObservedTime   uint64            `json:"observed_time_unix_nano"`
	TraceID        string            `json:"trace_id"`
	SpanID         string            `json:"span_id"`
	Attributes     map[string]string `json:"attributes"`
}

func summarize(record *logs.LogRecord) summary {
	attributes := map[string]string{}
	for _, attribute := range record.GetAttributes() {
		attributes[attribute.GetKey()] = renderValue(attribute.GetValue())
	}
	return summary{
		SeverityNumber: int32(record.GetSeverityNumber()),
		SeverityText:   record.GetSeverityText(),
		Body:           record.GetBody().GetStringValue(),
		EventTime:      record.GetTimeUnixNano(),
		ObservedTime:   record.GetObservedTimeUnixNano(),
		TraceID:        hexText(record.GetTraceId()),
		SpanID:         hexText(record.GetSpanId()),
		Attributes:     attributes,
	}
}

// renderValue prints any attribute value, so a fixture never shows a non-string
// attribute as an empty string.
func renderValue(value *common.AnyValue) string {
	switch value.GetValue().(type) {
	case *common.AnyValue_StringValue:
		return value.GetStringValue()
	case *common.AnyValue_IntValue:
		return fmt.Sprintf("%d", value.GetIntValue())
	case *common.AnyValue_DoubleValue:
		return fmt.Sprintf("%g", value.GetDoubleValue())
	case *common.AnyValue_BoolValue:
		return fmt.Sprintf("%t", value.GetBoolValue())
	default:
		// Structured values are summarized by shape rather than content: the
		// protobuf text encoder varies its own output, which a fixture cannot
		// be compared against.
		return fmt.Sprintf("<%T>", value.GetValue())
	}
}

func hexText(value []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(value)*2)
	for _, b := range value {
		out = append(out, digits[b>>4], digits[b&0x0F])
	}
	return string(out)
}

func countKey(attributes []*common.KeyValue, key string) int {
	count := 0
	for _, attribute := range attributes {
		if attribute.GetKey() == key {
			count++
		}
	}
	return count
}

func valueDepth(value *common.AnyValue) int {
	list := value.GetKvlistValue()
	if list == nil {
		return 1
	}
	deepest := 0
	for _, entry := range list.GetValues() {
		if d := valueDepth(entry.GetValue()); d > deepest {
			deepest = d
		}
	}
	return deepest + 1
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
