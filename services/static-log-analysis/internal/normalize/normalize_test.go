package normalize_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/normalize"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/builders"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/otlpgen"
)

// scenario is the setup the first behaviour is defined against: a
// representative OTLP payment error arriving from an authenticated regional
// Collector.
type scenario struct {
	envelope   model.TrustedEnvelope
	producer   *otlpgen.Producer
	normalizer *normalize.Normalizer
}

func newScenario(t *testing.T) scenario {
	t.Helper()
	factory := builders.NewFactory()
	return scenario{
		envelope:   factory.Envelope(t),
		producer:   otlpgen.New(),
		normalizer: normalize.New(),
	}
}

// admitOne normalizes a request carrying a single record and returns it,
// failing the test if the record was rejected.
func (s scenario) admitOne(t *testing.T, record *otlpgen.Record) model.NormalizedLog {
	t.Helper()
	result, err := s.normalizer.Request(s.envelope, s.producer.Request(record))
	if err != nil {
		t.Fatalf("request refused: %v", err)
	}
	for _, rejection := range result.Rejected {
		t.Fatalf("record %d rejected: %v", rejection.Index, rejection.Err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("want 1 normalized record, got %d", len(result.Records))
	}
	return result.Records[0]
}

func TestARepresentativePaymentErrorIsAdmittedAndNormalized(t *testing.T) {
	s := newScenario(t)
	source := s.producer.PaymentError()

	record := s.admitOne(t, source)

	t.Run("the record is structurally valid", func(t *testing.T) {
		// Everything below describes one field. This says the whole record is
		// something the rest of the service is allowed to see.
		if err := record.Validate(); err != nil {
			t.Fatalf("normalization produced an invalid record: %v", err)
		}
		if record.SchemaVersion != model.NormalizedLogSchemaVersion {
			t.Errorf("want schema version %s, got %s",
				model.NormalizedLogSchemaVersion, record.SchemaVersion)
		}
	})

	t.Run("identity is derived from the source instance and the producer uid", func(t *testing.T) {
		uid, ok := otlpgen.RecordUID(source)
		if !ok {
			t.Fatal("the representative record carries no producer uid")
		}
		// Restating the documented derivation rather than calling the
		// implementation: the test is what pins the algorithm down.
		digest := sha256.Sum256([]byte("otlp:v1" + s.envelope.SourceInstance + uid))
		want := hex.EncodeToString(digest[:])

		if record.RecordID != want {
			t.Errorf("want record id %s, got %s", want, record.RecordID)
		}
		if record.RecordIDVersion != model.RecordIDVersionOTLPV1 {
			t.Errorf("want version %s, got %s", model.RecordIDVersionOTLPV1, record.RecordIDVersion)
		}
		if record.IdentityQuality != model.IdentityQualityNative {
			t.Errorf("want native identity, got %s", record.IdentityQuality)
		}
	})

	t.Run("attribution follows the trusted envelope", func(t *testing.T) {
		if record.Service.Name != otlpgen.DefaultService {
			t.Errorf("want service %s, got %s", otlpgen.DefaultService, record.Service.Name)
		}
		if record.Service.Environment != builders.DefaultEnvironment {
			t.Errorf("want environment %s, got %s", builders.DefaultEnvironment, record.Service.Environment)
		}
		// Region comes from the envelope, never from a claim, because the
		// claim is the thing being checked.
		if record.Region != s.envelope.Region {
			t.Errorf("want region %s, got %s", s.envelope.Region, record.Region)
		}
		if record.Source.SourceInstance != s.envelope.SourceInstance {
			t.Errorf("want the envelope retained on the record, got %+v", record.Source)
		}
	})

	t.Run("severity maps to its class", func(t *testing.T) {
		if record.SeverityClass != model.SeverityClassError {
			t.Errorf("want severity class error, got %s", record.SeverityClass)
		}
		if record.SeverityNumber != int32(source.GetSeverityNumber()) {
			t.Errorf("want severity number %d, got %d", source.GetSeverityNumber(), record.SeverityNumber)
		}
		if record.SeverityText != source.GetSeverityText() {
			t.Errorf("want severity text %q, got %q", source.GetSeverityText(), record.SeverityText)
		}
	})

	t.Run("timestamps are preserved", func(t *testing.T) {
		wantEvent := time.Unix(0, int64(source.GetTimeUnixNano())).UTC()
		wantObserved := time.Unix(0, int64(source.GetObservedTimeUnixNano())).UTC()

		if !record.EventTime.Equal(wantEvent) {
			t.Errorf("want event time %s, got %s", wantEvent, record.EventTime)
		}
		if !record.ObservedTime.Equal(wantObserved) {
			t.Errorf("want observed time %s, got %s", wantObserved, record.ObservedTime)
		}
		if record.TimestampInferred {
			t.Error("want the event time used as given, it was marked inferred")
		}
	})

	t.Run("trace correlation becomes canonical lowercase hex", func(t *testing.T) {
		wantTrace := hex.EncodeToString(source.GetTraceId())
		wantSpan := hex.EncodeToString(source.GetSpanId())

		if record.Correlation.TraceID != wantTrace {
			t.Errorf("want trace %s, got %s", wantTrace, record.Correlation.TraceID)
		}
		if record.Correlation.SpanID != wantSpan {
			t.Errorf("want span %s, got %s", wantSpan, record.Correlation.SpanID)
		}
	})

	t.Run("exception data is carried", func(t *testing.T) {
		if record.Exception == nil {
			t.Fatal("want the exception carried, got none")
		}
		if record.Exception.Type != "PaymentDeclined" {
			t.Errorf("want exception type PaymentDeclined, got %s", record.Exception.Type)
		}
		if record.Exception.SafeMessage == "" {
			t.Error("want a safe exception message")
		}
	})

	t.Run("dynamic identifiers do not survive redaction", func(t *testing.T) {
		// These are the values the source record carried. Two occurrences that
		// differ only in them are the same failure, so they must not reach a
		// stored record at all.
		prohibited := map[string]string{
			"the request identifier":  "8f2c1d",
			"the customer identifier": "90210",
			"the order identifier":    "4711",
		}
		haystack := searchableText(record)
		for what, value := range prohibited {
			if strings.Contains(haystack, value) {
				t.Errorf("%s (%s) survived into the normalized record:\n%s", what, value, haystack)
			}
		}

		// Absence alone would also be satisfied by dropping the text, which
		// would pass this test while destroying the record. The surrounding
		// message has to survive with a typed placeholder in place of the
		// identifier.
		if !strings.Contains(record.Body.String, "charge failed for order <number>") {
			t.Errorf("want the body kept with a placeholder, got %q", record.Body.String)
		}
		if !strings.Contains(record.Exception.SafeMessage, "card declined by acme for request <hex>") {
			t.Errorf("want the exception message kept with a placeholder, got %q",
				record.Exception.SafeMessage)
		}
		if got := record.Attributes["request.id"]; got.String != "<hex>" {
			t.Errorf("want the request attribute replaced, got %q", got.String)
		}
	})

	t.Run("the producer uid is not left behind as a placeholder", func(t *testing.T) {
		// Its purpose was to establish record_id, which now carries that
		// identity. A redacted copy would name nothing.
		if _, present := record.Attributes[otlpgen.RecordUIDAttribute]; present {
			t.Errorf("want the record uid dropped once identity was derived, got %+v",
				record.Attributes[otlpgen.RecordUIDAttribute])
		}
	})

	t.Run("the redaction policy that made it safe is recorded", func(t *testing.T) {
		if record.Redaction.PolicyVersion == "" {
			t.Error("want the redaction policy version recorded")
		}
		if len(record.Redaction.RuleIDs) == 0 {
			t.Error("want the rules that matched recorded, so a policy change can be re-evaluated")
		}
	})
}

func TestARetriedRecordKeepsItsIdentity(t *testing.T) {
	s := newScenario(t)
	source := s.producer.PaymentError()

	first := s.admitOne(t, source)
	// A Collector whose acknowledgement was lost resends the same bytes. If the
	// second delivery produced a different identity, deduplication downstream
	// could never recognise it and the occurrence would be counted twice.
	second := s.admitOne(t, otlpgen.Duplicate(source))

	if first.RecordID != second.RecordID {
		t.Fatalf("want the retry to keep record id %s, got %s", first.RecordID, second.RecordID)
	}
	if first.BatchID == second.BatchID {
		t.Errorf("want each transport attempt to have its own batch id, both were %s", first.BatchID)
	}
}

func TestAServiceIdentityConflictingWithTheEnvelopeIsRejected(t *testing.T) {
	s := newScenario(t)
	// The envelope authenticated a Collector allowed to speak for
	// paymentservice. A record claiming another service is either misrouted or
	// forged, and either way it must not be attributed.
	impostor := otlpgen.New(otlpgen.WithService("cartservice"))

	result, err := s.normalizer.Request(s.envelope, impostor.Request(impostor.PaymentError()))

	if err != nil {
		t.Fatalf("want the batch admitted with the record rejected, got %v", err)
	}
	if len(result.Records) != 0 {
		t.Fatalf("want no normalized record, got %d", len(result.Records))
	}
	if len(result.Rejected) != 1 {
		t.Fatalf("want 1 rejection, got %d", len(result.Rejected))
	}
	if !errors.Is(result.Rejected[0].Err, normalize.ErrClaimNotPermitted) {
		t.Fatalf("want a claim conflict, got %v", result.Rejected[0].Err)
	}
	if !strings.Contains(result.Rejected[0].Err.Error(), "cartservice") {
		t.Errorf("want the rejected claim named, got %v", result.Rejected[0].Err)
	}
}

// searchableText returns every place a prohibited value could hide in a
// normalized record.
func searchableText(record model.NormalizedLog) string {
	var out strings.Builder
	out.WriteString(record.Body.String)
	out.WriteString("\n")
	out.WriteString(record.EventName)
	out.WriteString("\n")
	if record.Exception != nil {
		out.WriteString(record.Exception.Type)
		out.WriteString("\n")
		out.WriteString(record.Exception.SafeMessage)
		out.WriteString("\n")
		for _, frame := range record.Exception.StackFrames {
			out.WriteString(frame.Function + " " + frame.Module + " " + frame.File + "\n")
		}
	}
	for _, attributes := range []map[string]model.SafeValue{
		record.Attributes, record.ResourceAttributes, record.ScopeAttributes,
	} {
		for key, value := range attributes {
			out.WriteString(key + "=" + flatten(value) + "\n")
		}
	}
	return out.String()
}

func flatten(value model.SafeValue) string {
	switch value.Kind {
	case model.SafeKindString:
		return value.String
	case model.SafeKindMap:
		var parts []string
		for key, child := range value.Map {
			parts = append(parts, key+":"+flatten(child))
		}
		return strings.Join(parts, ",")
	case model.SafeKindSlice:
		var parts []string
		for _, child := range value.Slice {
			parts = append(parts, flatten(child))
		}
		return strings.Join(parts, ",")
	default:
		return ""
	}
}
