package admission_test

import (
	"bytes"
	"compress/gzip"
	"errors"
	"reflect"
	"strings"
	"testing"

	common "go.opentelemetry.io/proto/otlp/common/v1"
	logs "go.opentelemetry.io/proto/otlp/logs/v1"
	resource "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/admission"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/otlpgen"
)

func TestCompressedAndUncompressedRequestLimitsUseExactBoundaries(t *testing.T) {
	producer := otlpgen.New()
	wire, err := otlpgen.Encode(producer.Request(producer.Record()))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	if _, err := admission.Decode(wire, admission.EncodingIdentity, admission.Limits{MaxCompressedBytes: 1, MaxUncompressedBytes: int64(len(wire)), MaxRecords: 1, MaxNestingDepth: 16}); err != nil {
		t.Fatalf("exact identity boundary must pass: %v", err)
	}
	if _, err := admission.Decode(wire, admission.EncodingIdentity, admission.Limits{MaxCompressedBytes: 1, MaxUncompressedBytes: int64(len(wire) - 1), MaxRecords: 1, MaxNestingDepth: 16}); !errors.Is(err, admission.ErrUncompressedRequestTooLarge) {
		t.Fatalf("just over uncompressed maximum must reject request, got %v", err)
	}

	compressed := gzipBytes(t, wire)
	if _, err := admission.Decode(compressed, admission.EncodingGZIP, admission.Limits{MaxCompressedBytes: int64(len(compressed)), MaxUncompressedBytes: int64(len(wire)), MaxRecords: 1, MaxNestingDepth: 16}); err != nil {
		t.Fatalf("exact gzip boundaries must pass: %v", err)
	}
	if _, err := admission.Decode(compressed, admission.EncodingGZIP, admission.Limits{MaxCompressedBytes: int64(len(compressed)), MaxUncompressedBytes: int64(len(wire) - 1), MaxRecords: 1, MaxNestingDepth: 16}); !errors.Is(err, admission.ErrUncompressedRequestTooLarge) {
		t.Fatalf("just over expanded maximum must reject request, got %v", err)
	}
	if _, err := admission.Decode(compressed, admission.EncodingGZIP, admission.Limits{MaxCompressedBytes: int64(len(compressed) - 1), MaxUncompressedBytes: int64(len(wire)), MaxRecords: 1, MaxNestingDepth: 16}); !errors.Is(err, admission.ErrCompressedRequestTooLarge) {
		t.Fatalf("just over compressed maximum must reject request, got %v", err)
	}
}

func TestAdversarialCompressedInputStopsAtExpandedLimit(t *testing.T) {
	bomb := gzipBytes(t, bytes.Repeat([]byte{'A'}, 8<<20))
	_, err := admission.Decode(bomb, admission.EncodingGZIP, admission.Limits{MaxCompressedBytes: int64(len(bomb)), MaxUncompressedBytes: 1024, MaxRecords: 1, MaxNestingDepth: 16})
	if !errors.Is(err, admission.ErrUncompressedRequestTooLarge) {
		t.Fatalf("decompression bomb must stop at the expanded limit, got %v", err)
	}
	if strings.Contains(err.Error(), strings.Repeat("A", 64)) {
		t.Fatal("limit errors must not retain request content")
	}
}

func TestRecordCountRejectsTheWholeRequest(t *testing.T) {
	producer := otlpgen.New()
	wire, err := otlpgen.Encode(producer.Request(producer.Record(), producer.Record()))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := admission.Decode(wire, admission.EncodingIdentity, admission.Limits{MaxCompressedBytes: 1, MaxUncompressedBytes: int64(len(wire)), MaxRecords: 2, MaxNestingDepth: 16}); err != nil {
		t.Fatalf("exact record-count boundary must pass: %v", err)
	}
	_, err = admission.Decode(wire, admission.EncodingIdentity, admission.Limits{MaxCompressedBytes: 1, MaxUncompressedBytes: int64(len(wire)), MaxRecords: 1, MaxNestingDepth: 16})
	if !errors.Is(err, admission.ErrTooManyRecords) {
		t.Fatalf("record count overflow must reject the request, got %v", err)
	}
}

func TestNestingDepthIsAPerRecordPartialRejection(t *testing.T) {
	producer := otlpgen.New()
	request := producer.Request(producer.DeepAttributes(16), producer.DeepAttributes(17))
	wire, err := otlpgen.Encode(request)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := admission.Decode(wire, admission.EncodingIdentity, admission.Limits{MaxCompressedBytes: int64(len(wire)), MaxUncompressedBytes: int64(len(wire)), MaxRecords: 2, MaxNestingDepth: 16})
	if err != nil {
		t.Fatalf("record-local overflow must not reject the request: %v", err)
	}
	if len(got.Rejected) != 1 || got.Rejected[0].Index != 1 || got.Rejected[0].Reason != admission.ReasonNestingTooDeep {
		t.Fatalf("want only record 1 rejected for depth, got %+v", got.Rejected)
	}
	if countDecodedRecords(got.Request) != 1 {
		t.Fatalf("returned request must contain only the accepted record, got %+v", got.Request)
	}
}

func TestAcceptedRecordsRetainOriginalWireIndexes(t *testing.T) {
	producer := otlpgen.New()
	deep := func() *logs.LogRecord { return producer.DeepAttributes(17) }
	safe := func() *logs.LogRecord { return producer.Record() }
	tests := []struct {
		name         string
		records      []*logs.LogRecord
		wantAccepted []int
		wantRejected []int
	}{
		{name: "all accepted", records: []*logs.LogRecord{safe(), safe(), safe()}, wantAccepted: []int{0, 1, 2}},
		{name: "leading rejection", records: []*logs.LogRecord{deep(), safe()}, wantAccepted: []int{1}, wantRejected: []int{0}},
		{name: "middle rejection", records: []*logs.LogRecord{safe(), deep(), safe()}, wantAccepted: []int{0, 2}, wantRejected: []int{1}},
		{name: "multiple rejections", records: []*logs.LogRecord{deep(), safe(), deep(), safe()}, wantAccepted: []int{1, 3}, wantRejected: []int{0, 2}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := producer.Request(test.records...)
			wire, err := otlpgen.Encode(request)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := admission.Decode(wire, admission.EncodingIdentity, admission.Limits{
				MaxUncompressedBytes: int64(len(wire)), MaxRecords: len(test.records), MaxNestingDepth: 16,
			})
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(got.AcceptedOriginalIndexes, test.wantAccepted) {
				t.Fatalf("want accepted original indexes %v, got %v", test.wantAccepted, got.AcceptedOriginalIndexes)
			}
			if countDecodedRecords(got.Request) != len(test.wantAccepted) {
				t.Fatalf("mapping must align with flattened accepted request: indexes=%v request=%+v", got.AcceptedOriginalIndexes, got.Request)
			}
			var rejected []int
			for _, rejection := range got.Rejected {
				rejected = append(rejected, rejection.Index)
			}
			if !reflect.DeepEqual(rejected, test.wantRejected) {
				t.Fatalf("want rejected original indexes %v, got %v", test.wantRejected, rejected)
			}
		})
	}
}

func TestExtremeNestingRemainsAPerRecordPartialRejection(t *testing.T) {
	producer := otlpgen.New()
	request := producer.Request(producer.DeepAttributes(2), producer.DeepAttributes(300))
	wire, err := otlpgen.Encode(request)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := admission.Decode(wire, admission.EncodingIdentity, admission.Limits{MaxUncompressedBytes: int64(len(wire)), MaxRecords: 2, MaxNestingDepth: 16})
	if err != nil {
		t.Fatalf("extreme record-local nesting must not reject its safe sibling: %v", err)
	}
	if len(got.Rejected) != 1 || got.Rejected[0].Index != 1 || got.Rejected[0].Reason != admission.ReasonNestingTooDeep {
		t.Fatalf("want only extreme record rejected, got %+v", got.Rejected)
	}
}

func TestDepthChecksBodyResourceAndScopeWithSharedAttribution(t *testing.T) {
	producer := otlpgen.New()
	deepRecord := producer.DeepAttributes(17)
	deep, ok := otlpgen.AttributeValue(deepRecord.GetAttributes(), "nested")
	if !ok {
		t.Fatal("deep fixture is missing nested attribute")
	}
	tests := []struct {
		name string
		make func() *otlpgen.ExportRequest
		want []int
	}{
		{
			name: "body map",
			make: func() *otlpgen.ExportRequest {
				return producer.Request(producer.Record(), producer.Record(otlpgen.WithBodyValue(deep)))
			},
			want: []int{1},
		},
		{
			name: "body array",
			make: func() *otlpgen.ExportRequest {
				array := &common.AnyValue{Value: &common.AnyValue_ArrayValue{ArrayValue: &common.ArrayValue{Values: []*common.AnyValue{deep}}}}
				return producer.Request(producer.Record(), producer.Record(otlpgen.WithBodyValue(array)))
			},
			want: []int{1},
		},
		{
			name: "resource shared",
			make: func() *otlpgen.ExportRequest {
				request := producer.Request(producer.Record(), producer.Record())
				request.ResourceLogs[0].Resource.Attributes = append(request.ResourceLogs[0].Resource.Attributes, &common.KeyValue{Key: "nested", Value: deep})
				return request
			},
			want: []int{0, 1},
		},
		{
			name: "scope shared",
			make: func() *otlpgen.ExportRequest {
				request := producer.Request(producer.Record(), producer.Record())
				request.ResourceLogs[0].ScopeLogs[0].Scope.Attributes = append(request.ResourceLogs[0].ScopeLogs[0].Scope.Attributes, &common.KeyValue{Key: "nested", Value: deep})
				return request
			},
			want: []int{0, 1},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wire, err := otlpgen.Encode(test.make())
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := admission.Decode(wire, admission.EncodingIdentity, admission.Limits{MaxUncompressedBytes: int64(len(wire)), MaxRecords: 2, MaxNestingDepth: 16})
			if err != nil {
				t.Fatalf("depth must remain record-local: %v", err)
			}
			if len(got.Rejected) != len(test.want) {
				t.Fatalf("want indexes %v, got %+v", test.want, got.Rejected)
			}
			for i, index := range test.want {
				if got.Rejected[i].Index != index || got.Rejected[i].Reason != admission.ReasonNestingTooDeep {
					t.Fatalf("want indexes %v, got %+v", test.want, got.Rejected)
				}
			}
			wantAccepted := 2 - len(test.want)
			if countDecodedRecords(got.Request) != wantAccepted {
				t.Fatalf("want %d accepted records and no placeholders, got %+v", wantAccepted, got.Request)
			}
		})
	}
}

func countDecodedRecords(request *otlpgen.ExportRequest) int {
	count := 0
	for _, resourceLogs := range request.GetResourceLogs() {
		for _, scopeLogs := range resourceLogs.GetScopeLogs() {
			count += len(scopeLogs.GetLogRecords())
		}
	}
	return count
}

func TestNilAndEmptyRecordsStillCountWithoutPanicking(t *testing.T) {
	producer := otlpgen.New()
	request := producer.Request(nil, &logs.LogRecord{}, producer.Record())
	wire, err := otlpgen.Encode(request)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := admission.Decode(wire, admission.EncodingIdentity, admission.Limits{MaxUncompressedBytes: int64(len(wire)), MaxRecords: 3}); err != nil {
		t.Fatalf("exact count containing nil/empty records must decode safely: %v", err)
	}
	if _, err := admission.Decode(wire, admission.EncodingIdentity, admission.Limits{MaxUncompressedBytes: int64(len(wire)), MaxRecords: 2}); !errors.Is(err, admission.ErrTooManyRecords) {
		t.Fatalf("nil/empty records must count toward the request limit, got %v", err)
	}
}

func TestInvalidAndDefaultLimitMatrix(t *testing.T) {
	if _, err := admission.Decode(nil, admission.EncodingIdentity, admission.Limits{}); err != nil {
		t.Fatalf("zero-value limits must use safe defaults: %v", err)
	}
	invalid := []admission.Limits{
		{MaxCompressedBytes: -1},
		{MaxCompressedBytes: admission.DefaultMaxCompressedBytes + 1},
		{MaxUncompressedBytes: -1},
		{MaxUncompressedBytes: admission.DefaultMaxUncompressedBytes + 1},
		{MaxRecords: -1},
		{MaxRecords: admission.DefaultMaxRecords + 1},
		{MaxNestingDepth: -1},
		{MaxNestingDepth: admission.DefaultMaxNestingDepth + 1},
		{MaxNormalizedBytes: -1},
		{MaxNormalizedBytes: admission.DefaultMaxNormalizedBytes + 1},
		{MaxStructuralNodes: -1},
		{MaxStructuralNodes: admission.DefaultMaxStructuralNodes + 1},
		{MaxMaterializedBytes: -1},
		{MaxMaterializedBytes: admission.DefaultMaxMaterializedBytes + 1},
	}
	for _, limits := range invalid {
		if _, err := admission.Decode(nil, admission.EncodingIdentity, limits); !errors.Is(err, admission.ErrInvalidLimits) {
			t.Errorf("invalid limits %+v must fail explicitly, got %v", limits, err)
		}
	}
}

func TestAggregateMaterializationBudgetCountsSharedResourceAndScopePerRecord(t *testing.T) {
	producer := otlpgen.New()
	large := strings.Repeat("x", 64<<10)
	for _, level := range []string{"resource", "scope"} {
		t.Run(level, func(t *testing.T) {
			records := make([]*logs.LogRecord, 100)
			for index := range records {
				records[index] = &logs.LogRecord{}
			}
			request := producer.Request(records...)
			attribute := &common.KeyValue{Key: "shared", Value: otlpgen.StringValue(large)}
			if level == "resource" {
				request.ResourceLogs[0].Resource = &resource.Resource{Attributes: []*common.KeyValue{attribute}}
			} else {
				request.ResourceLogs[0].ScopeLogs[0].Scope.Attributes = []*common.KeyValue{attribute}
			}
			wire, err := otlpgen.Encode(request)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := admission.Decode(wire, admission.EncodingIdentity, admission.Limits{
				MaxUncompressedBytes: int64(len(wire)), MaxRecords: 100,
			})
			if !errors.Is(err, admission.ErrMaterializationTooLarge) {
				t.Fatalf("shared %s amplification must fail before materialization, got=%+v err=%v", level, got, err)
			}
			if got.Request != nil || len(got.Rejected) != 0 || len(got.AcceptedOriginalIndexes) != 0 {
				t.Fatalf("request-wide materialization failure returned misleading partial state: %+v", got)
			}
		})
	}
}

func TestAggregateMaterializationBudgetExactBoundaryAndCompression(t *testing.T) {
	producer := otlpgen.New()
	request := producer.Request(producer.Record(), producer.Record())
	wire, err := otlpgen.Encode(request)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	baseline, err := admission.Decode(wire, admission.EncodingIdentity, admission.Limits{MaxUncompressedBytes: int64(len(wire)), MaxRecords: 2})
	if err != nil || baseline.ProjectedMaterializedBytes < 2 {
		t.Fatalf("baseline projection: %+v err=%v", baseline, err)
	}
	limits := admission.Limits{
		MaxUncompressedBytes: int64(len(wire)), MaxRecords: 2,
		MaxMaterializedBytes: baseline.ProjectedMaterializedBytes,
	}
	if _, err := admission.Decode(wire, admission.EncodingIdentity, limits); err != nil {
		t.Fatalf("exact materialization boundary must pass: %v", err)
	}
	limits.MaxMaterializedBytes--
	if _, err := admission.Decode(wire, admission.EncodingIdentity, limits); !errors.Is(err, admission.ErrMaterializationTooLarge) {
		t.Fatalf("just-over materialization boundary must fail, got %v", err)
	}

	compressed := gzipBytes(t, wire)
	limits.MaxCompressedBytes = int64(len(compressed))
	limits.MaxMaterializedBytes = baseline.ProjectedMaterializedBytes
	if got, err := admission.Decode(compressed, admission.EncodingGZIP, limits); err != nil || got.ProjectedMaterializedBytes != baseline.ProjectedMaterializedBytes {
		t.Fatalf("compression must not change projection: got=%+v err=%v", got, err)
	}
}

func TestAggregateMaterializationBudgetAllowsTenThousandTinySharedAttributes(t *testing.T) {
	records := make([]*logs.LogRecord, admission.DefaultMaxRecords)
	for index := range records {
		records[index] = &logs.LogRecord{}
	}
	request := &otlpgen.ExportRequest{ResourceLogs: []*logs.ResourceLogs{{
		Resource:  &resource.Resource{Attributes: []*common.KeyValue{{Key: "a", Value: otlpgen.StringValue("b")}}},
		ScopeLogs: []*logs.ScopeLogs{{LogRecords: records}},
	}}}
	wire, err := otlpgen.Encode(request)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got, err := admission.Decode(wire, admission.EncodingIdentity, admission.Limits{}); err != nil {
		t.Fatalf("tiny shared metadata at max records should fit conservative budget: projection=%d err=%v", got.ProjectedMaterializedBytes, err)
	}
}

func TestDefaultBudgetsAdmitTenThousandSeparatelyWrappedRecords(t *testing.T) {
	request := &otlpgen.ExportRequest{ResourceLogs: make([]*logs.ResourceLogs, admission.DefaultMaxRecords)}
	for index := range request.ResourceLogs {
		request.ResourceLogs[index] = &logs.ResourceLogs{ScopeLogs: []*logs.ScopeLogs{{LogRecords: []*logs.LogRecord{{}}}}}
	}
	wire, err := otlpgen.Encode(request)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := admission.Decode(wire, admission.EncodingIdentity, admission.Limits{}); err != nil {
		t.Fatalf("default budgets must admit the documented 10,000 records even with separate wrappers: %v", err)
	}

	request.ResourceLogs = append(request.ResourceLogs, &logs.ResourceLogs{ScopeLogs: []*logs.ScopeLogs{{LogRecords: []*logs.LogRecord{{}}}}})
	wire, err = otlpgen.Encode(request)
	if err != nil {
		t.Fatalf("encode over-limit request: %v", err)
	}
	if _, err := admission.Decode(wire, admission.EncodingIdentity, admission.Limits{}); !errors.Is(err, admission.ErrTooManyRecords) {
		t.Fatalf("10,001st record must reject the request before decode, got %v", err)
	}
}

func TestNormalizedRecordSizeIsAPerRecordPartialRejectionAtJustOver(t *testing.T) {
	records := [][]byte{bytes.Repeat([]byte{'a'}, 8), bytes.Repeat([]byte{'b'}, 9)}
	got, err := admission.LimitNormalized(records, []int{4, 9}, admission.Limits{MaxNormalizedBytes: 8})
	if err != nil {
		t.Fatalf("limit safe records: %v", err)
	}
	if len(got) != 1 || got[0].Index != 9 || got[0].Reason != admission.ReasonNormalizedRecordTooLarge {
		t.Fatalf("want exact boundary accepted and just-over rejected, got %+v", got)
	}
}

func TestDepthAndNormalizedSizeRejectionsKeepDistinctOriginalIndexes(t *testing.T) {
	producer := otlpgen.New()
	request := producer.Request(producer.DeepAttributes(17), producer.Record())
	wire, err := otlpgen.Encode(request)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := admission.Decode(wire, admission.EncodingIdentity, admission.Limits{
		MaxUncompressedBytes: int64(len(wire)), MaxRecords: 2, MaxNestingDepth: 16,
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(decoded.AcceptedOriginalIndexes, []int{1}) {
		t.Fatalf("want accepted record mapped to original index 1, got %v", decoded.AcceptedOriginalIndexes)
	}
	sizeRejected, err := admission.LimitNormalized(
		[][]byte{bytes.Repeat([]byte{'x'}, 9)}, decoded.AcceptedOriginalIndexes,
		admission.Limits{MaxNormalizedBytes: 8},
	)
	if err != nil {
		t.Fatalf("limit normalized: %v", err)
	}
	combined := append(append([]admission.RecordRejection(nil), decoded.Rejected...), sizeRejected...)
	if len(combined) != 2 || combined[0].Index != 0 || combined[1].Index != 1 {
		t.Fatalf("depth and size rejections must identify distinct original records, got %+v", combined)
	}
}

func TestNormalizedRecordMappingFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		records [][]byte
		mapping []int
	}{
		{name: "missing", records: [][]byte{{'a'}}},
		{name: "short", records: [][]byte{{'a'}, {'b'}}, mapping: []int{0}},
		{name: "long", records: [][]byte{{'a'}}, mapping: []int{0, 1}},
		{name: "negative", records: [][]byte{{'a'}}, mapping: []int{-1}},
		{name: "duplicate", records: [][]byte{{'a'}, {'b'}}, mapping: []int{2, 2}},
		{name: "nonmonotonic", records: [][]byte{{'a'}, {'b'}}, mapping: []int{3, 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := admission.LimitNormalized(test.records, test.mapping, admission.Limits{MaxNormalizedBytes: 8})
			if !errors.Is(err, admission.ErrInvalidRecordMapping) {
				t.Fatalf("malformed mapping must fail closed, got %v", err)
			}
			if strings.Contains(err.Error(), "3") || strings.Contains(err.Error(), "2") {
				t.Fatalf("mapping error must remain opaque: %v", err)
			}
		})
	}
	if got, err := admission.LimitNormalized(nil, nil, admission.Limits{MaxNormalizedBytes: 8}); err != nil || len(got) != 0 {
		t.Fatalf("empty records and mapping must remain valid, got=%+v err=%v", got, err)
	}
}

func TestNormalizedRecordSizeConfigurationFailsExplicitly(t *testing.T) {
	for _, maximum := range []int{-1, admission.DefaultMaxNormalizedBytes + 1} {
		if _, err := admission.LimitNormalized(nil, nil, admission.Limits{MaxNormalizedBytes: maximum}); !errors.Is(err, admission.ErrInvalidLimits) {
			t.Fatalf("maximum %d must fail explicitly, got %v", maximum, err)
		}
	}
}

func TestEmptyWrapperFloodsAreRejectedBeforeDecodeAtExactBoundary(t *testing.T) {
	tests := []struct {
		name    string
		request *otlpgen.ExportRequest
	}{
		{
			name: "resource logs",
			request: &otlpgen.ExportRequest{ResourceLogs: []*logs.ResourceLogs{
				{}, {}, {},
			}},
		},
		{
			name: "scope logs",
			request: &otlpgen.ExportRequest{ResourceLogs: []*logs.ResourceLogs{{
				ScopeLogs: []*logs.ScopeLogs{{}, {}},
			}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wire, err := otlpgen.Encode(test.request)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			limits := admission.Limits{MaxUncompressedBytes: int64(len(wire)), MaxRecords: 1, MaxNestingDepth: 16, MaxStructuralNodes: 3}
			if _, err := admission.Decode(wire, admission.EncodingIdentity, limits); err != nil {
				t.Fatalf("exact structural boundary must pass: %v", err)
			}
			limits.MaxStructuralNodes = 2
			if _, err := admission.Decode(wire, admission.EncodingIdentity, limits); !errors.Is(err, admission.ErrTooManyStructuralNodes) {
				t.Fatalf("just-over structural boundary must fail opaquely, got %v", err)
			}
		})
	}
}

func TestUnsupportedEncodingErrorDoesNotEchoHostileValue(t *testing.T) {
	secret := admission.Encoding("password=hunter2")
	_, err := admission.Decode(nil, secret, admission.Limits{})
	if !errors.Is(err, admission.ErrUnsupportedEncoding) {
		t.Fatalf("want unsupported encoding category, got %v", err)
	}
	if strings.Contains(err.Error(), string(secret)) || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("unsupported encoding error echoed untrusted value: %v", err)
	}
}

func TestGZIPMembersAndCorruptionAreBoundedAndOpaque(t *testing.T) {
	member := gzipBytes(t, bytes.Repeat([]byte{'A'}, 700))
	concatenated := append(append([]byte(nil), member...), member...)
	_, err := admission.Decode(concatenated, admission.EncodingGZIP, admission.Limits{MaxCompressedBytes: int64(len(concatenated)), MaxUncompressedBytes: 1024})
	if !errors.Is(err, admission.ErrUncompressedRequestTooLarge) {
		t.Fatalf("aggregate concatenated members must obey expanded limit, got %v", err)
	}

	validWire, encodeErr := otlpgen.Encode(otlpgen.New().Request())
	if encodeErr != nil {
		t.Fatalf("encode: %v", encodeErr)
	}
	validGZIP := gzipBytes(t, validWire)
	cases := map[string][]byte{
		"trailing garbage": append(append([]byte(nil), validGZIP...), []byte("password=hunter2")...),
		"zero padding":     append(append([]byte(nil), validGZIP...), 0, 0, 0),
		"truncated":        append([]byte(nil), validGZIP[:len(validGZIP)-2]...),
		"bad checksum":     append([]byte(nil), validGZIP...),
	}
	cases["bad checksum"][len(cases["bad checksum"])-5] ^= 0xff
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := admission.Decode(payload, admission.EncodingGZIP, admission.Limits{MaxCompressedBytes: int64(len(payload)), MaxUncompressedBytes: 1024})
			if !errors.Is(err, admission.ErrMalformedRequest) {
				t.Fatalf("invalid gzip must fail as malformed, got %v", err)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("gzip error echoed trailing content: %v", err)
			}
		})
	}
}

func gzipBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return out.Bytes()
}
