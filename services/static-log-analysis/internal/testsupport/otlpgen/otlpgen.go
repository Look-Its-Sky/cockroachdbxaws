// Package otlpgen produces deterministic OTLP log payloads.
//
// The OpenTelemetry demo generates realistic traffic but cannot be asked for an
// exact duplicate, a record one nanosecond either side of a window boundary, a
// payload just over a size limit, or a replay of the same batch. Those are the
// cases admission, identity, and window behaviour are decided by, so the
// harness generates them directly.
//
// Everything is derived from an injected clock and identifier source, so the
// same scenario produces byte-identical payloads on every run.
package otlpgen

import (
	"encoding/binary"
	"fmt"
	"strings"

	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	logs "go.opentelemetry.io/proto/otlp/logs/v1"
	resource "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/clock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/ids"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/testids"
)

// Record and ExportRequest are the OTLP types this package produces, aliased so
// that a test naming them does not have to import the generated packages.
type (
	Record        = logs.LogRecord
	ExportRequest = collectorlogs.ExportLogsServiceRequest
)

// RecordUIDAttribute is the attribute a direct OTLP producer sets before its
// first export attempt. Pairing it with the authenticated source envelope is
// what makes record identity stable across a retry.
const RecordUIDAttribute = "log.record.uid"

// Defaults for the payment failure scenario the vertical slice is built around.
const (
	DefaultService     = "paymentservice"
	DefaultNamespace   = "opentelemetry-demo"
	DefaultEnvironment = "production"
	DefaultRegion      = "us-east-1"
	DefaultScopeName   = "payment/charge"
	DefaultScopeVer    = "1.0.0"
)

// Producer builds OTLP log records and export requests.
type Producer struct {
	clock       clock.Clock
	ids         ids.Source
	service     string
	namespace   string
	environment string
	region      string
	instance    string
	container   string
	// traceCounter derives distinct trace and span identifiers without
	// randomness, so a scenario can point two records at the same trace or at
	// different ones and get the same bytes every run.
	traceCounter uint64
}

// Option configures a Producer.
type Option func(*Producer)

// WithClock supplies the clock event and observed times are read from.
func WithClock(c clock.Clock) Option { return func(p *Producer) { p.clock = c } }

// WithIDs supplies the source of record UIDs.
func WithIDs(source ids.Source) Option { return func(p *Producer) { p.ids = source } }

// WithService sets the service identity the records claim.
func WithService(name string) Option { return func(p *Producer) { p.service = name } }

// WithEnvironment sets the environment the records claim.
func WithEnvironment(environment string) Option {
	return func(p *Producer) { p.environment = environment }
}

// WithRegion sets the region the records claim.
func WithRegion(region string) Option { return func(p *Producer) { p.region = region } }

// WithContainer sets the container identity, which repeated records from
// several replicas of one service differ by.
func WithContainer(id string) Option { return func(p *Producer) { p.container = id } }

// New returns a producer. Without options it uses a fake clock at
// fakeclock.Origin and a deterministic identifier source.
func New(opts ...Option) *Producer {
	fake := fakeclock.NewAtOrigin()
	p := &Producer{
		clock:       fake,
		ids:         testids.New(testids.WithClock(fake)),
		service:     DefaultService,
		namespace:   DefaultNamespace,
		environment: DefaultEnvironment,
		region:      DefaultRegion,
		instance:    DefaultService + "-0",
		container:   "c0ffee0000000000",
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Clock returns the clock the producer reads times from.
func (p *Producer) Clock() clock.Clock { return p.clock }

// recordSettings holds the parts of a record the options build up.
type recordSettings struct {
	severityNumber logs.SeverityNumber
	severityText   string
	body           *common.AnyValue
	eventName      string
	attributes     []*common.KeyValue
	traceID        []byte
	spanID         []byte
	timeUnixNano   uint64
	observedUnix   uint64
	// omitUID suppresses the record UID, which is how a legacy producer that
	// forces the derived identity fallback is represented.
	omitUID bool
	uid     string
}

// RecordOption adjusts a record under construction.
type RecordOption func(*recordSettings)

// Record returns one OTLP log record.
func (p *Producer) Record(opts ...RecordOption) *logs.LogRecord {
	now := p.clock.Now()
	settings := &recordSettings{
		severityNumber: logs.SeverityNumber_SEVERITY_NUMBER_ERROR,
		severityText:   "ERROR",
		body:           stringValue("charge failed for order"),
		observedUnix:   uint64(now.UnixNano()),
		// A producer stamps event time before export; the two-second lag keeps
		// it inside the window where event time is used as given.
		timeUnixNano: uint64(now.Add(-2e9).UnixNano()),
	}
	for _, opt := range opts {
		opt(settings)
	}

	attributes := []*common.KeyValue{
		keyValue("payment.provider", stringValue("acme")),
		keyValue("http.route", stringValue("/api/payments/{id}")),
	}
	if !settings.omitUID {
		uid := settings.uid
		if uid == "" {
			generated, err := p.ids.New()
			if err != nil {
				panic(fmt.Sprintf("otlpgen: generating a record uid: %v", err))
			}
			uid = generated
		}
		attributes = append(attributes, keyValue(RecordUIDAttribute, stringValue(uid)))
	}
	attributes = append(attributes, settings.attributes...)

	return &logs.LogRecord{
		TimeUnixNano:         settings.timeUnixNano,
		ObservedTimeUnixNano: settings.observedUnix,
		SeverityNumber:       settings.severityNumber,
		SeverityText:         settings.severityText,
		Body:                 settings.body,
		EventName:            settings.eventName,
		Attributes:           attributes,
		TraceId:              settings.traceID,
		SpanId:               settings.spanID,
	}
}

// PaymentError returns the representative payment failure: an error with an
// exception, a stack trace whose frames are partly in-application, and trace
// correlation.
func (p *Producer) PaymentError(opts ...RecordOption) *logs.LogRecord {
	traceID, spanID := p.nextTrace()
	base := []RecordOption{
		WithBody("charge failed for order 4711: card declined"),
		WithException(
			"PaymentDeclined",
			"card declined by acme for request 8f2c1d, customer 90210",
			strings.Join([]string{
				"PaymentDeclined: card declined",
				"  at charge (payment/charge.go:118)",
				"  at handleCharge (payment/handler.go:64)",
				"  at (*mux).ServeHTTP (net/http/server.go:2938)",
			}, "\n"),
		),
		WithTrace(traceID, spanID),
		WithAttribute("payment.amount", doubleValue(42.5)),
		WithAttribute("payment.currency", stringValue("USD")),
		WithAttribute("request.id", stringValue("8f2c1d")),
	}
	return p.Record(append(base, opts...)...)
}

// WithBody sets a string body.
func WithBody(body string) RecordOption {
	return func(s *recordSettings) { s.body = stringValue(body) }
}

// WithBodyValue sets a structured body.
func WithBodyValue(body *common.AnyValue) RecordOption {
	return func(s *recordSettings) { s.body = body }
}

// WithoutBody removes the body.
func WithoutBody() RecordOption {
	return func(s *recordSettings) { s.body = nil }
}

// WithEventName sets the event name, which stands in for a body.
func WithEventName(name string) RecordOption {
	return func(s *recordSettings) { s.eventName = name }
}

// WithSeverity sets the severity number and its text.
func WithSeverity(number logs.SeverityNumber, text string) RecordOption {
	return func(s *recordSettings) {
		s.severityNumber = number
		s.severityText = text
	}
}

// Severity presets.
func SeverityDebug() RecordOption {
	return WithSeverity(logs.SeverityNumber_SEVERITY_NUMBER_DEBUG, "DEBUG")
}
func SeverityInfo() RecordOption {
	return WithSeverity(logs.SeverityNumber_SEVERITY_NUMBER_INFO, "INFO")
}
func SeverityWarn() RecordOption {
	return WithSeverity(logs.SeverityNumber_SEVERITY_NUMBER_WARN, "WARN")
}
func SeverityError() RecordOption {
	return WithSeverity(logs.SeverityNumber_SEVERITY_NUMBER_ERROR, "ERROR")
}
func SeverityFatal() RecordOption {
	return WithSeverity(logs.SeverityNumber_SEVERITY_NUMBER_FATAL, "FATAL")
}

// WithAttribute adds one attribute.
func WithAttribute(key string, value *common.AnyValue) RecordOption {
	return func(s *recordSettings) { s.attributes = append(s.attributes, keyValue(key, value)) }
}

// WithException adds the exception attributes a language SDK sets.
func WithException(exceptionType, message, stackTrace string) RecordOption {
	return func(s *recordSettings) {
		s.attributes = append(s.attributes,
			keyValue("exception.type", stringValue(exceptionType)),
			keyValue("exception.message", stringValue(message)),
			keyValue("exception.stacktrace", stringValue(stackTrace)),
		)
	}
}

// WithTrace sets trace correlation.
func WithTrace(traceID, spanID []byte) RecordOption {
	return func(s *recordSettings) {
		s.traceID = traceID
		s.spanID = spanID
	}
}

// WithRecordUID sets the record UID explicitly, which is how two records are
// made to claim the same identity.
func WithRecordUID(uid string) RecordOption {
	return func(s *recordSettings) { s.uid = uid }
}

// WithoutRecordUID removes the record UID, forcing the derived identity
// fallback.
func WithoutRecordUID() RecordOption {
	return func(s *recordSettings) { s.omitUID = true }
}

// AtEventTime sets the event time in nanoseconds since the Unix epoch. Window
// boundary scenarios use it to place a record exactly at, just before, or just
// after an instant.
func AtEventTime(unixNano uint64) RecordOption {
	return func(s *recordSettings) { s.timeUnixNano = unixNano }
}

// AtObservedTime sets the observed time in nanoseconds since the Unix epoch.
func AtObservedTime(unixNano uint64) RecordOption {
	return func(s *recordSettings) { s.observedUnix = unixNano }
}

// WithoutTimestamps removes both timestamps.
func WithoutTimestamps() RecordOption {
	return func(s *recordSettings) {
		s.timeUnixNano = 0
		s.observedUnix = 0
	}
}

// Request wraps records in an export request with this producer's resource and
// scope.
func (p *Producer) Request(records ...*logs.LogRecord) *collectorlogs.ExportLogsServiceRequest {
	return &collectorlogs.ExportLogsServiceRequest{
		ResourceLogs: []*logs.ResourceLogs{{
			Resource: &resource.Resource{Attributes: p.resourceAttributes()},
			ScopeLogs: []*logs.ScopeLogs{{
				Scope: &common.InstrumentationScope{
					Name:    DefaultScopeName,
					Version: DefaultScopeVer,
				},
				LogRecords: records,
			}},
		}},
	}
}

func (p *Producer) resourceAttributes() []*common.KeyValue {
	return []*common.KeyValue{
		keyValue("service.name", stringValue(p.service)),
		keyValue("service.namespace", stringValue(p.namespace)),
		keyValue("service.instance.id", stringValue(p.instance)),
		keyValue("deployment.environment.name", stringValue(p.environment)),
		keyValue("cloud.region", stringValue(p.region)),
		keyValue("container.id", stringValue(p.container)),
	}
}

// Duplicate returns an independent copy of a record.
//
// A Collector that retries after a lost acknowledgement resends the same bytes,
// so a duplicate carries the same record UID rather than a fresh one.
func Duplicate(record *logs.LogRecord) *logs.LogRecord {
	return proto.Clone(record).(*logs.LogRecord)
}

// DuplicateRequest returns an independent copy of an export request.
func DuplicateRequest(request *collectorlogs.ExportLogsServiceRequest) *collectorlogs.ExportLogsServiceRequest {
	return proto.Clone(request).(*collectorlogs.ExportLogsServiceRequest)
}

// ReplaySequence returns the same request repeated count times, as an
// at-least-once transport delivers it.
func ReplaySequence(request *collectorlogs.ExportLogsServiceRequest, count int) []*collectorlogs.ExportLogsServiceRequest {
	replays := make([]*collectorlogs.ExportLogsServiceRequest, 0, count)
	for i := 0; i < count; i++ {
		replays = append(replays, DuplicateRequest(request))
	}
	return replays
}

// MalformedKind names a way a record can be structurally wrong.
type MalformedKind string

const (
	// MalformedNoIdentity has neither a record UID nor enough content to derive
	// one, so it cannot be deduplicated.
	MalformedNoIdentity MalformedKind = "no_identity"
	// MalformedInvalidUID carries a record UID that is not a UUID.
	MalformedInvalidUID MalformedKind = "invalid_uid"
	// MalformedNoTimestamps carries neither an event nor an observed time.
	MalformedNoTimestamps MalformedKind = "no_timestamps"
	// MalformedNoContent carries neither a body nor an event name.
	MalformedNoContent MalformedKind = "no_content"
	// MalformedShortTraceID carries a trace id of the wrong length.
	MalformedShortTraceID MalformedKind = "short_trace_id"
	// MalformedEmptyAttributeKey carries an attribute with no name.
	MalformedEmptyAttributeKey MalformedKind = "empty_attribute_key"
	// MalformedDuplicateAttributeKey carries the same attribute key twice,
	// which OTLP forbids and whose handling must be defined rather than
	// unpredictable.
	MalformedDuplicateAttributeKey MalformedKind = "duplicate_attribute_key"
	// MalformedFutureEventTime carries an event time far ahead of observed
	// time, which must not be able to hold an incident open.
	MalformedFutureEventTime MalformedKind = "future_event_time"
)

// Malformed returns a record that is wrong in exactly one named way. Valid
// siblings in the same batch must survive it.
func (p *Producer) Malformed(kind MalformedKind, opts ...RecordOption) *logs.LogRecord {
	now := uint64(p.clock.Now().UnixNano())
	switch kind {
	case MalformedNoIdentity:
		return p.Record(append([]RecordOption{
			WithoutRecordUID(), WithoutBody(), WithoutTimestamps(),
		}, opts...)...)
	case MalformedInvalidUID:
		return p.Record(append([]RecordOption{WithRecordUID("not-a-uuid")}, opts...)...)
	case MalformedNoTimestamps:
		return p.Record(append([]RecordOption{WithoutTimestamps()}, opts...)...)
	case MalformedNoContent:
		return p.Record(append([]RecordOption{WithoutBody()}, opts...)...)
	case MalformedShortTraceID:
		return p.Record(append([]RecordOption{WithTrace([]byte{1, 2, 3}, []byte{4, 5})}, opts...)...)
	case MalformedEmptyAttributeKey:
		return p.Record(append([]RecordOption{WithAttribute("", stringValue("orphan"))}, opts...)...)
	case MalformedDuplicateAttributeKey:
		return p.Record(append([]RecordOption{
			WithAttribute("payment.provider", stringValue("first")),
			WithAttribute("payment.provider", stringValue("second")),
		}, opts...)...)
	case MalformedFutureEventTime:
		const oneHour = uint64(3600e9)
		return p.Record(append([]RecordOption{AtEventTime(now + oneHour)}, opts...)...)
	default:
		panic(fmt.Sprintf("otlpgen: unknown malformed kind %q", kind))
	}
}

// Oversized returns a record whose encoded size is at least size bytes, for
// per-record and per-request limit tests.
func (p *Producer) Oversized(size int, opts ...RecordOption) *logs.LogRecord {
	record := p.Record(opts...)
	// Padding is added rather than guessed at, so the record is over the limit
	// regardless of how the rest of it encodes.
	for proto.Size(record) < size {
		shortfall := size - proto.Size(record)
		record.Body = stringValue(strings.Repeat("p", shortfall+16))
	}
	return record
}

// DeepAttributes returns a record with one attribute nested to the given depth,
// for attribute nesting limit tests. A depth of 1 is a plain scalar.
func (p *Producer) DeepAttributes(depth int, opts ...RecordOption) *logs.LogRecord {
	if depth < 1 {
		panic(fmt.Sprintf("otlpgen: depth must be at least 1, got %d", depth))
	}
	value := stringValue("leaf")
	for level := 1; level < depth; level++ {
		value = &common.AnyValue{Value: &common.AnyValue_KvlistValue{
			KvlistValue: &common.KeyValueList{Values: []*common.KeyValue{
				keyValue(fmt.Sprintf("level_%d", depth-level), value),
			}},
		}}
	}
	return p.Record(append([]RecordOption{WithAttribute("nested", value)}, opts...)...)
}

// nextTrace returns the next deterministic trace and span identifier pair.
func (p *Producer) nextTrace() (traceID, spanID []byte) {
	p.traceCounter++
	traceID = make([]byte, 16)
	binary.BigEndian.PutUint64(traceID[0:8], 0x4bf92f3577b34da6)
	binary.BigEndian.PutUint64(traceID[8:16], p.traceCounter)
	spanID = make([]byte, 8)
	binary.BigEndian.PutUint64(spanID, 0x00f067aa0ba90000|p.traceCounter)
	return traceID, spanID
}

// NextTrace returns a deterministic trace and span identifier pair, so a
// scenario can put several records on one trace.
func (p *Producer) NextTrace() (traceID, spanID []byte) { return p.nextTrace() }

// Encode returns the deterministic wire encoding of a request, for size limits
// and for asserting that two payloads are byte-identical.
func Encode(request *collectorlogs.ExportLogsServiceRequest) ([]byte, error) {
	return proto.MarshalOptions{Deterministic: true}.Marshal(request)
}

func keyValue(key string, value *common.AnyValue) *common.KeyValue {
	return &common.KeyValue{Key: key, Value: value}
}

func stringValue(v string) *common.AnyValue {
	return &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: v}}
}

func doubleValue(v float64) *common.AnyValue {
	return &common.AnyValue{Value: &common.AnyValue_DoubleValue{DoubleValue: v}}
}

// IntValue returns an integer attribute value.
func IntValue(v int64) *common.AnyValue {
	return &common.AnyValue{Value: &common.AnyValue_IntValue{IntValue: v}}
}

// StringValue returns a string attribute value.
func StringValue(v string) *common.AnyValue { return stringValue(v) }

// BoolValue returns a boolean attribute value.
func BoolValue(v bool) *common.AnyValue {
	return &common.AnyValue{Value: &common.AnyValue_BoolValue{BoolValue: v}}
}

// AttributeValue returns the value of an attribute by key, and whether it was
// present. Tests read record UIDs and identity-bearing attributes with it.
func AttributeValue(attributes []*common.KeyValue, key string) (*common.AnyValue, bool) {
	for _, attribute := range attributes {
		if attribute.GetKey() == key {
			return attribute.GetValue(), true
		}
	}
	return nil, false
}

// RecordUID returns the record UID a record carries, and whether it has one.
func RecordUID(record *logs.LogRecord) (string, bool) {
	value, ok := AttributeValue(record.GetAttributes(), RecordUIDAttribute)
	if !ok {
		return "", false
	}
	return value.GetStringValue(), true
}
