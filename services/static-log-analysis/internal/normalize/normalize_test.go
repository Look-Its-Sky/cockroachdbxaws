package normalize_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	logs "go.opentelemetry.io/proto/otlp/logs/v1"
	resource "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/admission"
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

func TestDirectRequestUsesIdentityOriginalIndexMapping(t *testing.T) {
	s := newScenario(t)
	result, err := s.normalizer.Request(s.envelope, s.producer.Request(s.producer.Record(), s.producer.Record()))
	if err != nil || !reflect.DeepEqual(result.RecordOriginalIndexes, []int{0, 1}) {
		t.Fatalf("direct unfiltered request must retain identity mapping, result=%+v err=%v", result, err)
	}
}

func TestMissingProducerUIDUsesTheMeasuredDerivedIdentityFallback(t *testing.T) {
	s := newScenario(t)
	source := s.producer.PaymentError(otlpgen.WithoutRecordUID())

	first := s.admitOne(t, source)
	second := s.admitOne(t, otlpgen.Duplicate(source))
	if first.RecordID != second.RecordID {
		t.Fatalf("retry changed derived identity: %s != %s", first.RecordID, second.RecordID)
	}
	if first.RecordIDVersion != model.RecordIDVersionDerivedV1 {
		t.Fatalf("record id version=%q, want %q", first.RecordIDVersion, model.RecordIDVersionDerivedV1)
	}
	if first.IdentityQuality != model.IdentityQualityDerived {
		t.Fatalf("identity quality=%q, want derived", first.IdentityQuality)
	}
}

func TestMappedNormalizationPreservesOriginalIndexesThroughEveryPartialBoundary(t *testing.T) {
	s := newScenario(t)
	hostile := otlpgen.New(otlpgen.WithService("cartservice"))
	groups := []*logs.ResourceLogs{}
	for _, request := range []*otlpgen.ExportRequest{
		s.producer.Request(s.producer.DeepAttributes(17)),
		hostile.Request(hostile.Record()),
		s.producer.Request(s.producer.Record()),
		s.producer.Request(s.producer.DeepAttributes(17)),
		s.producer.Request(s.producer.Record(otlpgen.WithRecordUID("not-a-uuid"))),
		s.producer.Request(s.producer.Record()),
	} {
		groups = append(groups, request.ResourceLogs...)
	}
	raw := &collectorlogs.ExportLogsServiceRequest{ResourceLogs: groups}
	wire, err := otlpgen.Encode(raw)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := admission.Decode(wire, admission.EncodingIdentity, admission.Limits{
		MaxUncompressedBytes: int64(len(wire)), MaxRecords: 6, MaxNestingDepth: 16,
	})
	if err != nil || !reflect.DeepEqual(decoded.AcceptedOriginalIndexes, []int{1, 2, 4, 5}) {
		t.Fatalf("admission mapping mismatch: decoded=%+v err=%v", decoded, err)
	}
	normalized, err := s.normalizer.MappedRequest(s.envelope, decoded.Request, decoded.AcceptedOriginalIndexes)
	if err != nil {
		t.Fatalf("mapped normalize: %v", err)
	}
	if !reflect.DeepEqual(normalized.RecordOriginalIndexes, []int{2, 5}) {
		t.Fatalf("successful records lost original indexes: %+v", normalized)
	}
	if len(normalized.Rejected) != 2 || normalized.Rejected[0].Index != 1 || normalized.Rejected[1].Index != 4 {
		t.Fatalf("normalization rejections lost original indexes: %+v", normalized.Rejected)
	}
	sizeRejected, err := admission.LimitNormalized(
		[][]byte{bytes.Repeat([]byte{'a'}, 8), bytes.Repeat([]byte{'b'}, 9)},
		normalized.RecordOriginalIndexes, admission.Limits{MaxNormalizedBytes: 8},
	)
	if err != nil || len(sizeRejected) != 1 || sizeRejected[0].Index != 5 {
		t.Fatalf("size rejection lost post-normalization original index: %+v err=%v", sizeRejected, err)
	}
	allRejected := []int{decoded.Rejected[0].Index, decoded.Rejected[1].Index, normalized.Rejected[0].Index, normalized.Rejected[1].Index, sizeRejected[0].Index}
	if !reflect.DeepEqual(allRejected, []int{0, 3, 1, 4, 5}) {
		t.Fatalf("partial boundaries must identify five distinct original records, got %v", allRejected)
	}
}

type countingIDSource struct{ calls int }

func (s *countingIDSource) New() (string, error) {
	s.calls++
	return "0194f0a0-0000-7000-8000-000000000001", nil
}

func TestMappedNormalizationRejectsMalformedMappingsBeforeIDGeneration(t *testing.T) {
	s := newScenario(t)
	request := s.producer.Request(s.producer.Record(), s.producer.Record())
	for name, mapping := range map[string][]int{
		"missing": nil, "short": {0}, "negative": {-1, 1}, "duplicate": {1, 1}, "nonmonotonic": {2, 1},
	} {
		t.Run(name, func(t *testing.T) {
			idSource := &countingIDSource{}
			normalizer := normalize.New(normalize.WithIDs(idSource))
			_, err := normalizer.MappedRequest(s.envelope, request, mapping)
			if !errors.Is(err, normalize.ErrInvalidRecordMapping) || idSource.calls != 0 {
				t.Fatalf("mapping must fail opaquely before ID generation: err=%v calls=%d", err, idSource.calls)
			}
		})
	}
}

func TestNilNormalizerDependenciesFailClosedWithoutPanicking(t *testing.T) {
	s := newScenario(t)
	request := s.producer.Request(s.producer.Record())
	var typedNil *countingIDSource
	for name, normalizer := range map[string]*normalize.Normalizer{
		"nil policy": normalize.New(normalize.WithRedactionPolicy(nil)),
		"nil IDs":    normalize.New(normalize.WithIDs(nil)),
		"typed nil":  normalize.New(normalize.WithIDs(typedNil)),
		"nil option": normalize.New((normalize.Option)(nil)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizer.Request(s.envelope, request); !errors.Is(err, normalize.ErrInvalidConfiguration) {
				t.Fatalf("invalid dependency must fail with stable category, got %v", err)
			}
		})
	}
}

func TestNormalizerClonesTrustedEnvelopeOwnership(t *testing.T) {
	s := newScenario(t)
	originalServices := append([]string(nil), s.envelope.AllowedServices...)
	originalEnvironments := append([]string(nil), s.envelope.AllowedEnvironments...)
	result, err := s.normalizer.Request(s.envelope, s.producer.Request(s.producer.Record(), s.producer.Record()))
	if err != nil || len(result.Records) != 2 {
		t.Fatalf("normalize: result=%+v err=%v", result, err)
	}
	s.envelope.AllowedServices[0] = "password=hunter2"
	s.envelope.AllowedEnvironments[0] = "password=hunter2"
	for index := range result.Records {
		if !reflect.DeepEqual(result.Records[index].Source.AllowedServices, originalServices) || !reflect.DeepEqual(result.Records[index].Source.AllowedEnvironments, originalEnvironments) {
			t.Fatalf("input mutation changed returned record %d source: %+v", index, result.Records[index].Source)
		}
	}
	result.Records[0].Source.AllowedServices[0] = "record-mutated"
	result.Records[0].Source.AllowedEnvironments[0] = "record-mutated"
	if s.envelope.AllowedServices[0] != "password=hunter2" || s.envelope.AllowedEnvironments[0] != "password=hunter2" {
		t.Fatalf("returned record mutation changed input envelope: %+v", s.envelope)
	}
	if !reflect.DeepEqual(result.Records[1].Source.AllowedServices, originalServices) || !reflect.DeepEqual(result.Records[1].Source.AllowedEnvironments, originalEnvironments) {
		t.Fatalf("sibling record sources alias: first=%+v second=%+v", result.Records[0].Source, result.Records[1].Source)
	}
	result.Records[1].Source.AllowedServices[0] = "sibling-mutated"
	result.Records[1].Source.AllowedEnvironments[0] = "sibling-mutated"
	if result.Records[0].Source.AllowedServices[0] != "record-mutated" || result.Records[0].Source.AllowedEnvironments[0] != "record-mutated" {
		t.Fatalf("second sibling mutation changed first: first=%+v second=%+v", result.Records[0].Source, result.Records[1].Source)
	}
}

func TestNormalizationMaterializationBudgetIsExactAndRunsBeforeIDGeneration(t *testing.T) {
	s := newScenario(t)
	request := s.producer.Request(s.producer.Record(), s.producer.Record())
	baseline, err := s.normalizer.Request(s.envelope, request)
	if err != nil || baseline.ProjectedMaterializedBytes < 2 {
		t.Fatalf("baseline projection: result=%+v err=%v", baseline, err)
	}
	exactIDs := &countingIDSource{}
	exact := normalize.New(normalize.WithIDs(exactIDs), normalize.WithMaxMaterializedBytes(baseline.ProjectedMaterializedBytes))
	if _, err := exact.Request(s.envelope, request); err != nil || exactIDs.calls != 1 {
		t.Fatalf("exact normalization materialization boundary must pass: err=%v calls=%d", err, exactIDs.calls)
	}
	overIDs := &countingIDSource{}
	over := normalize.New(normalize.WithIDs(overIDs), normalize.WithMaxMaterializedBytes(baseline.ProjectedMaterializedBytes-1))
	if _, err := over.Request(s.envelope, request); !errors.Is(err, normalize.ErrMaterializationTooLarge) || overIDs.calls != 0 {
		t.Fatalf("over-budget normalization must fail before ID generation: err=%v calls=%d", err, overIDs.calls)
	}
}

func TestNormalizationMaterializationBudgetCoversSharedAttributesAndEnvelopeClones(t *testing.T) {
	s := newScenario(t)
	records := make([]*logs.LogRecord, 100)
	for index := range records {
		records[index] = s.producer.Record()
	}
	request := s.producer.Request(records...)
	request.ResourceLogs[0].Resource = &resource.Resource{Attributes: []*common.KeyValue{
		{Key: "shared", Value: otlpgen.StringValue(strings.Repeat("x", 64<<10))},
	}}
	ids := &countingIDSource{}
	normalizer := normalize.New(normalize.WithIDs(ids))
	if _, err := normalizer.Request(s.envelope, request); !errors.Is(err, normalize.ErrMaterializationTooLarge) || ids.calls != 0 {
		t.Fatalf("shared decoded attributes must fail before IDs: err=%v calls=%d", err, ids.calls)
	}

	envelope := s.envelope
	envelope.AllowedServices = append([]string{otlpgen.DefaultService}, make([]string, 128)...)
	for index := 1; index < len(envelope.AllowedServices); index++ {
		envelope.AllowedServices[index] = "service-" + strconv.Itoa(index) + strings.Repeat("x", 1024)
	}
	request = s.producer.Request(records...)
	ids = &countingIDSource{}
	normalizer = normalize.New(normalize.WithIDs(ids))
	if _, err := normalizer.Request(envelope, request); !errors.Is(err, normalize.ErrMaterializationTooLarge) || ids.calls != 0 {
		t.Fatalf("repeated trusted-envelope clones must be budgeted before IDs: err=%v calls=%d", err, ids.calls)
	}
}

func TestNormalizationMaterializationBudgetCoversEmbeddedJSONExpansion(t *testing.T) {
	s := newScenario(t)
	compactJSON := "[" + strings.Repeat("0,", 4_999) + "0]"
	makeRecords := func(body string) []*logs.LogRecord {
		records := make([]*logs.LogRecord, 100)
		for index := range records {
			records[index] = s.producer.Record(otlpgen.WithBody(body))
		}
		return records
	}
	tests := map[string]func() *collectorlogs.ExportLogsServiceRequest{
		"record bodies": func() *collectorlogs.ExportLogsServiceRequest {
			return s.producer.Request(makeRecords(compactJSON)...)
		},
		"shared resource attribute": func() *collectorlogs.ExportLogsServiceRequest {
			request := s.producer.Request(makeRecords("")...)
			request.ResourceLogs[0].Resource = &resource.Resource{Attributes: []*common.KeyValue{
				{Key: "structured", Value: otlpgen.StringValue(compactJSON)},
			}}
			return request
		},
		"shared scope attribute": func() *collectorlogs.ExportLogsServiceRequest {
			request := s.producer.Request(makeRecords("")...)
			request.ResourceLogs[0].ScopeLogs[0].Scope.Attributes = []*common.KeyValue{
				{Key: "structured", Value: otlpgen.StringValue(compactJSON)},
			}
			return request
		},
	}
	for name, makeRequest := range tests {
		t.Run(name, func(t *testing.T) {
			idSource := &countingIDSource{}
			normalizer := normalize.New(normalize.WithIDs(idSource))
			if _, err := normalizer.Request(s.envelope, makeRequest()); !errors.Is(err, normalize.ErrMaterializationTooLarge) || idSource.calls != 0 {
				t.Fatalf("embedded JSON expansion must fail before IDs: err=%v calls=%d", err, idSource.calls)
			}
		})
	}
}

func TestNormalizationMaterializationJSONBoundaryIsExact(t *testing.T) {
	s := newScenario(t)
	request := s.producer.Request(s.producer.Record(otlpgen.WithBody(`{"items":[0,1,2],"ok":true}`)))
	baseline, err := s.normalizer.Request(s.envelope, request)
	if err != nil {
		t.Fatalf("baseline JSON projection: %v", err)
	}
	exactIDs := &countingIDSource{}
	exact := normalize.New(normalize.WithIDs(exactIDs), normalize.WithMaxMaterializedBytes(baseline.ProjectedMaterializedBytes))
	if _, err := exact.Request(s.envelope, request); err != nil || exactIDs.calls != 1 {
		t.Fatalf("exact JSON projection must pass: err=%v calls=%d", err, exactIDs.calls)
	}
	overIDs := &countingIDSource{}
	over := normalize.New(normalize.WithIDs(overIDs), normalize.WithMaxMaterializedBytes(baseline.ProjectedMaterializedBytes-1))
	if _, err := over.Request(s.envelope, request); !errors.Is(err, normalize.ErrMaterializationTooLarge) || overIDs.calls != 0 {
		t.Fatalf("one byte under JSON projection must fail before IDs: err=%v calls=%d", err, overIDs.calls)
	}
}

func TestNormalizationDuplicateJSONProjectionIsOpaqueAndExact(t *testing.T) {
	s := newScenario(t)
	duplicate := `{"duplicate":"first-secret","duplicate":"second-secret"}`
	duplicateRequest := s.producer.Request(s.producer.Record(otlpgen.WithBody(duplicate)))
	baseline, err := s.normalizer.Request(s.envelope, duplicateRequest)
	if err != nil {
		t.Fatalf("baseline duplicate JSON projection: %v", err)
	}
	opaque, err := s.normalizer.Request(s.envelope, s.producer.Request(s.producer.Record(otlpgen.WithBody(strings.Repeat("x", len(duplicate))))))
	if err != nil {
		t.Fatalf("same-length opaque projection: %v", err)
	}
	if baseline.ProjectedMaterializedBytes != opaque.ProjectedMaterializedBytes {
		t.Fatalf("duplicate JSON must project as one opaque/withheld value: duplicate=%d opaque=%d", baseline.ProjectedMaterializedBytes, opaque.ProjectedMaterializedBytes)
	}
	exactIDs := &countingIDSource{}
	exact := normalize.New(normalize.WithIDs(exactIDs), normalize.WithMaxMaterializedBytes(baseline.ProjectedMaterializedBytes))
	if _, err := exact.Request(s.envelope, duplicateRequest); err != nil || exactIDs.calls != 1 {
		t.Fatalf("exact duplicate-JSON projection must pass: err=%v calls=%d", err, exactIDs.calls)
	}
	overIDs := &countingIDSource{}
	over := normalize.New(normalize.WithIDs(overIDs), normalize.WithMaxMaterializedBytes(baseline.ProjectedMaterializedBytes-1))
	if _, err := over.Request(s.envelope, duplicateRequest); !errors.Is(err, normalize.ErrMaterializationTooLarge) || overIDs.calls != 0 {
		t.Fatalf("one byte under duplicate-JSON projection must fail before IDs: err=%v calls=%d", err, overIDs.calls)
	}
}

func TestUnsafeTrustedEnvelopeFailsOnceBeforeIDsAndRecordWork(t *testing.T) {
	s := newScenario(t)
	records := make([]*logs.LogRecord, 100)
	for index := range records {
		records[index] = s.producer.Record()
	}
	for name, mutate := range map[string]func(*model.TrustedEnvelope){
		"allowed service": func(envelope *model.TrustedEnvelope) {
			envelope.AllowedServices = append(envelope.AllowedServices, "password=hunter2")
		},
		"source identity": func(envelope *model.TrustedEnvelope) { envelope.SourceInstance = "password=hunter2" },
	} {
		t.Run(name, func(t *testing.T) {
			envelope := s.envelope
			envelope.AllowedServices = append([]string(nil), envelope.AllowedServices...)
			mutate(&envelope)
			ids := &countingIDSource{}
			normalizer := normalize.New(normalize.WithIDs(ids))
			_, err := normalizer.Request(envelope, s.producer.Request(records...))
			if !errors.Is(err, normalize.ErrUnsafeEnvelope) || ids.calls != 0 {
				t.Fatalf("unsafe envelope must fail once before IDs: err=%v calls=%d", err, ids.calls)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("unsafe envelope error echoed content: %v", err)
			}
		})
	}
}

func TestInvalidNormalizationMaterializationConfigurationFailsClosed(t *testing.T) {
	s := newScenario(t)
	for _, maximum := range []int64{-1, normalize.DefaultMaxMaterializedBytes + 1} {
		normalizer := normalize.New(normalize.WithMaxMaterializedBytes(maximum))
		if _, err := normalizer.Request(s.envelope, s.producer.Request(s.producer.Record())); !errors.Is(err, normalize.ErrInvalidConfiguration) {
			t.Fatalf("invalid materialization maximum %d must fail categorically, got %v", maximum, err)
		}
	}
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

func TestEventTimestampNormalizationUsesTheDocumentedBounds(t *testing.T) {
	s := newScenario(t)
	observed := time.Date(2026, 8, 6, 12, 0, 0, 123, time.UTC)

	for _, test := range []struct {
		name       string
		event      time.Time
		inferred   bool
		reasonPart string
	}{
		{name: "missing event time", inferred: true, reasonPart: "missing"},
		{name: "one nanosecond too old", event: observed.Add(-24*time.Hour - time.Nanosecond), inferred: true, reasonPart: "outside"},
		{name: "exactly 24 hours old", event: observed.Add(-24 * time.Hour)},
		{name: "exactly five minutes ahead", event: observed.Add(5 * time.Minute)},
		{name: "one nanosecond too far ahead", event: observed.Add(5*time.Minute + time.Nanosecond), inferred: true, reasonPart: "outside"},
	} {
		t.Run(test.name, func(t *testing.T) {
			eventUnixNano := uint64(0)
			if !test.event.IsZero() {
				eventUnixNano = uint64(test.event.UnixNano())
			}
			record := s.producer.Record(
				otlpgen.AtObservedTime(uint64(observed.UnixNano())),
				otlpgen.AtEventTime(eventUnixNano),
			)
			normalized := s.admitOne(t, record)

			if normalized.TimestampInferred != test.inferred {
				t.Fatalf("want inferred=%v, got %+v", test.inferred, normalized)
			}
			if test.inferred {
				if !normalized.EventTime.Equal(observed) {
					t.Fatalf("want processing time to use observed %s, got %s", observed, normalized.EventTime)
				}
				if !strings.Contains(normalized.TimestampInferenceReason, test.reasonPart) {
					t.Fatalf("want reason containing %q, got %q", test.reasonPart, normalized.TimestampInferenceReason)
				}
			} else {
				if !normalized.EventTime.Equal(test.event) {
					t.Fatalf("want valid event time %s retained, got %s", test.event, normalized.EventTime)
				}
				if normalized.TimestampInferenceReason != "" {
					t.Fatalf("want no inference reason, got %q", normalized.TimestampInferenceReason)
				}
			}
		})
	}
}

func TestMissingObservedTimeUsesEventTimeWithReplayStableProvenance(t *testing.T) {
	s := newScenario(t)
	event := time.Date(2026, 8, 6, 12, 0, 0, 123, time.UTC)
	source := s.producer.Record(
		otlpgen.AtEventTime(uint64(event.UnixNano())),
		otlpgen.AtObservedTime(0),
		otlpgen.WithoutRecordUID(),
	)

	first := s.admitOne(t, source)
	second := s.admitOne(t, otlpgen.Duplicate(source))

	if !first.ObservedTime.Equal(event) {
		t.Fatalf("want observed time to fall back to event time %s, got %s", event, first.ObservedTime)
	}
	if !first.EventTime.Equal(event) || first.TimestampInferred {
		t.Fatalf("a supplied event time must stay authoritative, got %+v", first)
	}
	if !first.ObservedTimeInferred || first.ObservedTimeInferenceReason != "observed_time_missing_event_time_used" {
		t.Fatalf("want explicit observed-time provenance, got %+v", first)
	}
	if first.RecordID != second.RecordID {
		t.Fatalf("a replay changed derived identity: %s became %s", first.RecordID, second.RecordID)
	}
}

func TestMissingEventAndObservedTimesAreCategoricallyRejected(t *testing.T) {
	s := newScenario(t)
	source := s.producer.Record(otlpgen.AtEventTime(0), otlpgen.AtObservedTime(0))

	result, err := s.normalizer.Request(s.envelope, s.producer.Request(source))
	if err != nil {
		t.Fatalf("a record-local timestamp defect rejected its whole batch: %v", err)
	}
	if len(result.Records) != 0 || len(result.Rejected) != 1 {
		t.Fatalf("want one record-local rejection, got %+v", result)
	}
	if !errors.Is(result.Rejected[0].Err, normalize.ErrMissingTimestamps) {
		t.Fatalf("want missing-timestamps category, got %v", result.Rejected[0].Err)
	}
}

func TestIdentityClaimsConflictingWithTheTrustedEnvelopeAreRejected(t *testing.T) {
	for _, test := range []struct {
		name     string
		producer *otlpgen.Producer
		claim    string
	}{
		{name: "service", producer: otlpgen.New(otlpgen.WithService("cartservice")), claim: "cartservice"},
		{name: "environment", producer: otlpgen.New(otlpgen.WithEnvironment("staging")), claim: "staging"},
		{name: "region", producer: otlpgen.New(otlpgen.WithRegion("eu-west-1")), claim: "eu-west-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newScenario(t)
			result, err := s.normalizer.Request(s.envelope, test.producer.Request(test.producer.PaymentError()))

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
			if strings.Contains(result.Rejected[0].Err.Error(), test.claim) {
				t.Errorf("rejection must not echo the untrusted claim %q: %v", test.claim, result.Rejected[0].Err)
			}
		})
	}
}

func TestMissingEnvironmentFromMultiEnvironmentSourceIsNotFullyAttributed(t *testing.T) {
	s := newScenario(t)
	s.envelope.AllowedEnvironments = []string{"production", "staging"}
	request := s.producer.Request(s.producer.PaymentError())
	request.ResourceLogs[0].Resource.Attributes = withoutResourceAttribute(
		request.ResourceLogs[0].Resource.Attributes,
		"deployment.environment.name",
	)

	result, err := s.normalizer.Request(s.envelope, request)
	if err != nil || len(result.Rejected) != 0 || len(result.Records) != 1 {
		t.Fatalf("want safe evidence admitted without full attribution, result=%+v err=%v", result, err)
	}
	service := result.Records[0].Service
	if service.Environment != "" || service.Status == model.EnrichmentAvailable {
		t.Fatalf("want unresolved environment to keep service identity incomplete, got %+v", service)
	}
}

func TestUnvalidatedNamespaceDoesNotEnterTypedServiceIdentity(t *testing.T) {
	s := newScenario(t)
	record := s.admitOne(t, s.producer.PaymentError())
	if record.Service.Namespace != "" {
		t.Fatalf("untrusted service.namespace entered grouping identity: %+v", record.Service)
	}
}

func TestConflictingDuplicateTrustedClaimsAreRejected(t *testing.T) {
	for _, test := range []struct {
		name  string
		key   string
		value string
	}{
		{name: "service", key: "service.name", value: "cartservice"},
		{name: "environment", key: "deployment.environment.name", value: "staging"},
		{name: "region", key: "cloud.region", value: "eu-west-1"},
	} {
		for _, position := range []string{"before", "after"} {
			t.Run(test.name+"_"+position, func(t *testing.T) {
				s := newScenario(t)
				request := s.producer.Request(s.producer.PaymentError())
				conflict := &common.KeyValue{Key: test.key, Value: otlpgen.StringValue(test.value)}
				if position == "before" {
					request.ResourceLogs[0].Resource.Attributes = append(
						[]*common.KeyValue{conflict}, request.ResourceLogs[0].Resource.Attributes...,
					)
				} else {
					request.ResourceLogs[0].Resource.Attributes = append(
						request.ResourceLogs[0].Resource.Attributes, conflict,
					)
				}

				result, err := s.normalizer.Request(s.envelope, request)
				if err != nil {
					t.Fatalf("want per-record rejection, got batch error %v", err)
				}
				if len(result.Records) != 0 || len(result.Rejected) != 1 ||
					!errors.Is(result.Rejected[0].Err, normalize.ErrClaimNotPermitted) {
					t.Fatalf("conflicting duplicate claim was not rejected: %+v", result)
				}
			})
		}
	}
}

func withoutResourceAttribute(attributes []*common.KeyValue, key string) []*common.KeyValue {
	filtered := make([]*common.KeyValue, 0, len(attributes))
	for _, attribute := range attributes {
		if attribute.GetKey() != key {
			filtered = append(filtered, attribute)
		}
	}
	return filtered
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

// answeringResolver answers exactly what a test tells it to, including shapes a
// careless resolver might really produce.
type answeringResolver struct{ answer model.DeploymentIdentity }

func (r answeringResolver) Deployment(model.ServiceIdentity, string) model.DeploymentIdentity {
	return r.answer
}

// TestAnIncompleteResolverAnswerStillProducesAValidRecord pins that a resolver
// outside this package cannot make records unpersistable.
//
// Normalization is the last place a record can be made valid. A deployment
// answered as "available" with no identity, or with no status at all, would
// otherwise fail the final validation and be rejected record-locally, which an
// operator would have to trace back through the coordinator to a resolver.
func TestAnIncompleteResolverAnswerStillProducesAValidRecord(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		answer     model.DeploymentIdentity
		wantID     string
		wantStatus model.EnrichmentStatus
	}{
		{"zero value", model.DeploymentIdentity{}, model.UnknownDeployment, model.EnrichmentPending},
		{"no status", model.DeploymentIdentity{ID: "deploy-1"}, "deploy-1", model.EnrichmentPending},
		{"available with no id", model.DeploymentIdentity{Status: model.EnrichmentAvailable},
			model.UnknownDeployment, model.EnrichmentPending},
		{"complete", model.DeploymentIdentity{ID: "deploy-1", Version: "1.0.0", Status: model.EnrichmentAvailable},
			"deploy-1", model.EnrichmentAvailable},
		{"definite absence", model.DeploymentIdentity{Status: model.EnrichmentNotApplicable},
			model.UnknownDeployment, model.EnrichmentNotApplicable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			scenario := newScenario(t)
			scenario.normalizer = normalize.New(
				normalize.WithDeploymentResolver(answeringResolver{answer: testCase.answer}))
			result, err := scenario.normalizer.Request(scenario.envelope,
				scenario.producer.Request(scenario.producer.PaymentError()))
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Records) != 1 {
				t.Fatalf("records=%d rejected=%+v", len(result.Records), result.Rejected)
			}
			deployment := result.Records[0].Deployment
			if deployment.ID != testCase.wantID || deployment.Status != testCase.wantStatus {
				t.Fatalf("deployment=%+v, want id %q status %q", deployment, testCase.wantID, testCase.wantStatus)
			}
			if err := result.Records[0].Validate(); err != nil {
				t.Fatalf("a resolver answer produced an invalid record: %v", err)
			}
		})
	}
}
