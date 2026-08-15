package admission

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"

	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	logs "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const (
	DefaultMaxCompressedBytes   int64 = 4 << 20
	DefaultMaxUncompressedBytes int64 = 16 << 20
	DefaultMaxRecords                 = 10_000
	DefaultMaxNestingDepth            = 16
	DefaultMaxNormalizedBytes         = 256 << 10
	DefaultMaxStructuralNodes         = 32 * DefaultMaxRecords
	DefaultMaxMaterializedBytes int64 = 16 << 20
)

var (
	ErrInvalidLimits               = errors.New("admission: invalid limits")
	ErrUnsupportedEncoding         = errors.New("admission: unsupported content encoding")
	ErrCompressedRequestTooLarge   = errors.New("admission: compressed request exceeds limit")
	ErrUncompressedRequestTooLarge = errors.New("admission: uncompressed request exceeds limit")
	ErrMalformedRequest            = errors.New("admission: malformed OTLP request")
	ErrTooManyRecords              = errors.New("admission: record count exceeds limit")
	ErrTooManyStructuralNodes      = errors.New("admission: structural node count exceeds limit")
	ErrInvalidRecordMapping        = errors.New("admission: invalid accepted-record index mapping")
	ErrMaterializationTooLarge     = errors.New("admission: projected materialization exceeds limit")
)

type Encoding string

const (
	EncodingIdentity Encoding = "identity"
	EncodingGZIP     Encoding = "gzip"
)

type Limits struct {
	MaxCompressedBytes   int64
	MaxUncompressedBytes int64
	MaxRecords           int
	MaxNestingDepth      int
	MaxNormalizedBytes   int
	MaxMaterializedBytes int64
	// MaxStructuralNodes bounds resource/scope/record wrappers, attributes,
	// and recursive AnyValue nodes walked before protobuf materialization.
	MaxStructuralNodes int
}

type RejectionReason string

const (
	ReasonNestingTooDeep           RejectionReason = "nesting_too_deep"
	ReasonNormalizedRecordTooLarge RejectionReason = "normalized_record_too_large"
)

// RecordRejection contains only an index and a stable reason; it never embeds
// the rejected content.
type RecordRejection struct {
	Index  int
	Reason RejectionReason
}

type DecodeResult struct {
	// Request contains accepted records only. Rejected indexes always refer to
	// positions in the original wire request, not this filtered request.
	Request *collectorlogs.ExportLogsServiceRequest
	// AcceptedOriginalIndexes is aligned with Request's flattened records and
	// maps each accepted record back to its position in the original wire
	// request. Later partial-rejection stages must retain this mapping.
	AcceptedOriginalIndexes []int
	Rejected                []RecordRejection
	// ProjectedMaterializedBytes is the conservative preflight weight of the
	// bounded raw request before record-local filtering and Go model allocation.
	ProjectedMaterializedBytes int64
}

// Decode enforces bounded expansion before protobuf decoding. Request-wide
// violations return an error; record-local depth violations are partial.
func Decode(payload []byte, encoding Encoding, configured Limits) (DecodeResult, error) {
	limits, err := configured.normalized()
	if err != nil {
		return DecodeResult{}, err
	}
	if encoding == EncodingGZIP && int64(len(payload)) > limits.MaxCompressedBytes {
		return DecodeResult{}, ErrCompressedRequestTooLarge
	}

	decoded, err := expand(payload, encoding, limits.MaxUncompressedBytes)
	if err != nil {
		return DecodeResult{}, err
	}
	if err := preflightStructure(decoded, limits.MaxRecords, limits.MaxStructuralNodes, limits.MaxNestingDepth); err != nil {
		return DecodeResult{}, err
	}
	projectedMaterializedBytes, err := projectMaterialization(decoded, limits.MaxNestingDepth, limits.MaxMaterializedBytes)
	if err != nil {
		return DecodeResult{}, err
	}
	decoded, nestingRejected, acceptedOriginalIndexes, err := sanitizeNesting(decoded, limits.MaxNestingDepth)
	if err != nil {
		return DecodeResult{}, ErrMalformedRequest
	}
	request := new(collectorlogs.ExportLogsServiceRequest)
	// Record-local structure is checked iteratively below. A separate generous
	// decoder ceiling prevents pathological protobuf recursion from reaching
	// the stack while still allowing just-over-limit records to be reported as
	// partial rejections.
	if err := (proto.UnmarshalOptions{RecursionLimit: 64}).Unmarshal(decoded, request); err != nil {
		return DecodeResult{}, ErrMalformedRequest
	}

	acceptedCount := len(flatten(request))
	if acceptedCount > limits.MaxRecords {
		return DecodeResult{}, ErrTooManyRecords
	}
	if validateRecordMapping(acceptedCount, acceptedOriginalIndexes) != nil {
		return DecodeResult{}, ErrMalformedRequest
	}
	return DecodeResult{
		Request: request, AcceptedOriginalIndexes: acceptedOriginalIndexes, Rejected: nestingRejected,
		ProjectedMaterializedBytes: projectedMaterializedBytes,
	}, nil
}

// preflightRecordCount walks only the length-delimited envelope hierarchy.
// This prevents a compact request containing millions of empty log records
// from causing millions of protobuf object allocations before the count limit
// can be checked.
func preflightStructure(data []byte, maximumRecords, maximumNodes, maximumDepth int) error {
	records := 0
	budget := nodeBudget{maximum: maximumNodes}
	err := forEachMessage(data, 1, func(resourceLogs []byte) error {
		if err := budget.add(); err != nil {
			return err
		}
		if err := forEachMessage(resourceLogs, 1, func(resource []byte) error {
			if err := budget.add(); err != nil {
				return err
			}
			return countAttributes(resource, 1, &budget, maximumDepth)
		}); err != nil {
			return err
		}
		return forEachMessage(resourceLogs, 2, func(scopeLogs []byte) error {
			if err := budget.add(); err != nil {
				return err
			}
			if err := forEachMessage(scopeLogs, 1, func(scope []byte) error {
				if err := budget.add(); err != nil {
					return err
				}
				return countAttributes(scope, 3, &budget, maximumDepth)
			}); err != nil {
				return err
			}
			return forEachMessage(scopeLogs, 2, func(record []byte) error {
				if err := budget.add(); err != nil {
					return err
				}
				records++
				if records > maximumRecords {
					return ErrTooManyRecords
				}
				return countLogRecord(record, &budget, maximumDepth)
			})
		})
	})
	if err != nil {
		if errors.Is(err, ErrTooManyRecords) || errors.Is(err, ErrTooManyStructuralNodes) {
			return err
		}
		return ErrMalformedRequest
	}
	return nil
}

type nodeBudget struct {
	used    int
	maximum int
}

func (b *nodeBudget) add() error {
	b.used++
	if b.used > b.maximum {
		return ErrTooManyStructuralNodes
	}
	return nil
}

func countLogRecord(data []byte, budget *nodeBudget, maximumDepth int) error {
	if err := forEachMessage(data, 5, func(value []byte) error {
		return countAnyValue(value, budget, 1, maximumDepth)
	}); err != nil {
		return err
	}
	return countAttributes(data, 6, budget, maximumDepth)
}

func countAttributes(data []byte, field protowire.Number, budget *nodeBudget, maximumDepth int) error {
	return forEachMessage(data, field, func(keyValue []byte) error {
		if err := budget.add(); err != nil {
			return err
		}
		return forEachMessage(keyValue, 2, func(value []byte) error {
			return countAnyValue(value, budget, 1, maximumDepth)
		})
	})
}

func countAnyValue(data []byte, budget *nodeBudget, depth, maximumDepth int) error {
	if err := budget.add(); err != nil {
		return err
	}
	if depth > maximumDepth {
		return nil
	}
	if err := forEachMessage(data, 5, func(array []byte) error {
		if err := budget.add(); err != nil {
			return err
		}
		return forEachMessage(array, 1, func(child []byte) error {
			return countAnyValue(child, budget, depth+1, maximumDepth)
		})
	}); err != nil {
		return err
	}
	return forEachMessage(data, 6, func(list []byte) error {
		if err := budget.add(); err != nil {
			return err
		}
		return forEachMessage(list, 1, func(keyValue []byte) error {
			if err := budget.add(); err != nil {
				return err
			}
			return forEachMessage(keyValue, 2, func(child []byte) error {
				return countAnyValue(child, budget, depth+1, maximumDepth)
			})
		})
	})
}

func forEachMessage(data []byte, field protowire.Number, visit func([]byte) error) error {
	for len(data) > 0 {
		number, wireType, tagBytes := protowire.ConsumeTag(data)
		if tagBytes < 0 {
			return protowire.ParseError(tagBytes)
		}
		data = data[tagBytes:]
		if number == field && wireType == protowire.BytesType {
			message, messageBytes := protowire.ConsumeBytes(data)
			if messageBytes < 0 {
				return protowire.ParseError(messageBytes)
			}
			if err := visit(message); err != nil {
				return err
			}
			data = data[messageBytes:]
			continue
		}
		valueBytes := protowire.ConsumeFieldValue(number, wireType, data)
		if valueBytes < 0 {
			return protowire.ParseError(valueBytes)
		}
		data = data[valueBytes:]
	}
	return nil
}

// Validate reports whether these limits are usable. A coordinator checks them
// once at construction, because invalid limits are a configuration error rather
// than something every request should rediscover as a rejection.
func (limits Limits) Validate() error {
	_, err := limits.normalized()
	return err
}

func (limits Limits) normalized() (Limits, error) {
	if limits.MaxCompressedBytes == 0 {
		limits.MaxCompressedBytes = DefaultMaxCompressedBytes
	}
	if limits.MaxUncompressedBytes == 0 {
		limits.MaxUncompressedBytes = DefaultMaxUncompressedBytes
	}
	if limits.MaxRecords == 0 {
		limits.MaxRecords = DefaultMaxRecords
	}
	if limits.MaxNestingDepth == 0 {
		limits.MaxNestingDepth = DefaultMaxNestingDepth
	}
	if limits.MaxNormalizedBytes == 0 {
		limits.MaxNormalizedBytes = DefaultMaxNormalizedBytes
	}
	if limits.MaxMaterializedBytes == 0 {
		limits.MaxMaterializedBytes = DefaultMaxMaterializedBytes
	}
	if limits.MaxStructuralNodes == 0 {
		limits.MaxStructuralNodes = DefaultMaxStructuralNodes
	}
	if limits.MaxCompressedBytes < 1 || limits.MaxCompressedBytes > DefaultMaxCompressedBytes ||
		limits.MaxUncompressedBytes < 1 || limits.MaxUncompressedBytes > DefaultMaxUncompressedBytes ||
		limits.MaxRecords < 1 || limits.MaxRecords > DefaultMaxRecords ||
		limits.MaxNestingDepth < 1 || limits.MaxNestingDepth > DefaultMaxNestingDepth ||
		limits.MaxNormalizedBytes < 1 || limits.MaxNormalizedBytes > DefaultMaxNormalizedBytes ||
		limits.MaxMaterializedBytes < 1 || limits.MaxMaterializedBytes > DefaultMaxMaterializedBytes ||
		limits.MaxStructuralNodes < 1 || limits.MaxStructuralNodes > DefaultMaxStructuralNodes {
		return Limits{}, ErrInvalidLimits
	}
	return limits, nil
}

func expand(payload []byte, encoding Encoding, maximum int64) ([]byte, error) {
	if encoding == "" {
		encoding = EncodingIdentity
	}
	var reader io.Reader = bytes.NewReader(payload)
	if encoding == EncodingGZIP {
		return expandGZIP(payload, maximum)
	} else if encoding != EncodingIdentity {
		return nil, ErrUnsupportedEncoding
	}

	decoded, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, ErrMalformedRequest
	}
	if int64(len(decoded)) > maximum {
		return nil, ErrUncompressedRequestTooLarge
	}
	return decoded, nil
}

func expandGZIP(payload []byte, maximum int64) ([]byte, error) {
	source := bytes.NewReader(payload)
	decoded := make([]byte, 0, min(int64(len(payload))*2, maximum))
	for source.Len() > 0 {
		if source.Len() < 2 {
			return nil, ErrMalformedRequest
		}
		header := make([]byte, 2)
		if _, err := source.ReadAt(header, int64(len(payload)-source.Len())); err != nil || header[0] != 0x1f || header[1] != 0x8b {
			return nil, ErrMalformedRequest
		}
		member, err := gzip.NewReader(source)
		if err != nil {
			return nil, ErrMalformedRequest
		}
		member.Multistream(false)
		remaining := maximum - int64(len(decoded))
		part, readErr := io.ReadAll(io.LimitReader(member, remaining+1))
		closeErr := member.Close()
		if readErr != nil || closeErr != nil {
			return nil, ErrMalformedRequest
		}
		decoded = append(decoded, part...)
		if int64(len(decoded)) > maximum {
			return nil, ErrUncompressedRequestTooLarge
		}
	}
	if len(payload) == 0 {
		return nil, ErrMalformedRequest
	}
	return decoded, nil
}

type recordView struct {
	record             *logs.LogRecord
	resourceAttributes []*common.KeyValue
	scopeAttributes    []*common.KeyValue
}

func flatten(request *collectorlogs.ExportLogsServiceRequest) []recordView {
	var records []recordView
	for _, resourceLogs := range request.GetResourceLogs() {
		for _, scopeLogs := range resourceLogs.GetScopeLogs() {
			for _, record := range scopeLogs.GetLogRecords() {
				records = append(records, recordView{
					record:             record,
					resourceAttributes: resourceLogs.GetResource().GetAttributes(),
					scopeAttributes:    scopeLogs.GetScope().GetAttributes(),
				})
			}
		}
	}
	return records
}

func recordDepth(view recordView) int {
	deepest := valueDepth(view.record.GetBody())
	for _, attributes := range [][]*common.KeyValue{
		view.record.GetAttributes(), view.resourceAttributes, view.scopeAttributes,
	} {
		for _, attribute := range attributes {
			if depth := valueDepth(attribute.GetValue()); depth > deepest {
				deepest = depth
			}
		}
	}
	return deepest
}

func valueDepth(root *common.AnyValue) int {
	if root == nil {
		return 0
	}
	type item struct {
		value *common.AnyValue
		depth int
	}
	stack := []item{{root, 1}}
	deepest := 0
	for len(stack) > 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]
		if current.depth > deepest {
			deepest = current.depth
		}
		switch value := current.value.GetValue().(type) {
		case *common.AnyValue_ArrayValue:
			for _, child := range value.ArrayValue.GetValues() {
				stack = append(stack, item{child, current.depth + 1})
			}
		case *common.AnyValue_KvlistValue:
			for _, child := range value.KvlistValue.GetValues() {
				stack = append(stack, item{child.GetValue(), current.depth + 1})
			}
		}
	}
	return deepest
}

// LimitNormalized performs the post-redaction, pre-persistence size check on
// each encoded safe record. acceptedOriginalIndexes must be aligned with the
// encoded accepted records so partial rejections retain their original wire
// positions. Exactly the configured maximum is accepted. The durable Protobuf
// schema is introduced in later schema work, so this boundary deliberately
// accepts caller-supplied encoded bytes rather than treating the diagnostic JSON
// representation as durable.
func LimitNormalized(encodedRecords [][]byte, acceptedOriginalIndexes []int, configured Limits) ([]RecordRejection, error) {
	limits, err := configured.normalized()
	if err != nil {
		return nil, err
	}
	if err := validateRecordMapping(len(encodedRecords), acceptedOriginalIndexes); err != nil {
		return nil, err
	}
	var rejected []RecordRejection
	for index, encoded := range encodedRecords {
		if len(encoded) > limits.MaxNormalizedBytes {
			rejected = append(rejected, RecordRejection{Index: acceptedOriginalIndexes[index], Reason: ReasonNormalizedRecordTooLarge})
		}
	}
	return rejected, nil
}

func validateRecordMapping(recordCount int, acceptedOriginalIndexes []int) error {
	if len(acceptedOriginalIndexes) != recordCount {
		return ErrInvalidRecordMapping
	}
	previous := -1
	for _, originalIndex := range acceptedOriginalIndexes {
		if originalIndex < 0 || originalIndex <= previous {
			return ErrInvalidRecordMapping
		}
		previous = originalIndex
	}
	return nil
}
