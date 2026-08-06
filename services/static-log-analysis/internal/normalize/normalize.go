// Package normalize turns OTLP log records into the canonical internal form.
//
// The order of work is fixed by a safety requirement rather than by
// convenience:
//
//	trusted-envelope validation
//	-> claim reconciliation
//	-> redaction
//	-> record identity
//	-> normalization
//	-> structural validation
//
// Redaction precedes normalization because a NormalizedLog is defined as
// already safe. Building one from raw content and cleaning it afterwards would
// create a window in which unredacted values exist in a structure the rest of
// the service may read.
//
// This package does not evaluate rules, persist anything, or serve a protocol.
// It converts an admitted batch and reports what it could not convert.
package normalize

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	logs "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/identity"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/ids"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
)

// Resource attribute names carrying the claims that are reconciled against the
// trusted envelope.
const (
	attributeServiceName   = "service.name"
	attributeServiceNS     = "service.namespace"
	attributeServiceID     = "service.instance.id"
	attributeEnvironment   = "deployment.environment.name"
	attributeRegion        = "cloud.region"
	attributeRecordUID     = "log.record.uid"
	attributeExceptionType = "exception.type"
	attributeExceptionMsg  = "exception.message"
)

// ErrClaimNotPermitted means a record claimed an identity its authenticated
// source is not allowed to speak for. The claim is refused rather than
// corrected, because the correct value is not knowable.
var ErrClaimNotPermitted = errors.New("claim is not permitted by the trusted envelope")

// ErrUnusableIdentity means no record identity could be established, so the
// record cannot participate in effectively-once processing.
var ErrUnusableIdentity = errors.New("record identity could not be established")

// Rejection is one record that was not normalized. The rest of its batch is
// unaffected: valid records survive malformed siblings.
type Rejection struct {
	// Index is the record's position within the request, counted across every
	// resource and scope, so it can be found in the payload again.
	Index int
	Err   error
}

// Result is the outcome of normalizing one transport batch.
type Result struct {
	// BatchID identifies this transport attempt. It is regenerated per attempt,
	// so a retry of the same records carries a new batch and the same record
	// identities.
	BatchID  string
	Records  []model.NormalizedLog
	Rejected []Rejection
}

// Normalizer converts admitted OTLP batches.
type Normalizer struct {
	policy *redact.Policy
	ids    ids.Source
}

// Option configures a Normalizer.
type Option func(*Normalizer)

// WithRedactionPolicy replaces the redaction policy applied to log-derived text.
func WithRedactionPolicy(policy *redact.Policy) Option {
	return func(n *Normalizer) { n.policy = policy }
}

// WithIDs replaces the source of batch identifiers.
func WithIDs(source ids.Source) Option {
	return func(n *Normalizer) { n.ids = source }
}

// New returns a Normalizer using the minimal redaction policy and generated
// batch identifiers.
func New(opts ...Option) *Normalizer {
	n := &Normalizer{policy: redact.MinimalPolicy(), ids: ids.System()}
	for _, opt := range opts {
		opt(n)
	}
	return n
}

// Request normalizes every record in an export request.
//
// An error means the batch itself was not admissible and nothing in it was
// processed. A rejection inside the result means one record could not be
// converted while the rest of the batch was.
func (n *Normalizer) Request(
	envelope model.TrustedEnvelope,
	request *collectorlogs.ExportLogsServiceRequest,
) (Result, error) {
	if err := envelope.Validate(); err != nil {
		// Without a valid envelope there is no authenticated source, so no
		// claim in the batch can be checked against anything.
		return Result{}, fmt.Errorf("normalize: trusted envelope is not usable: %w", err)
	}

	batchID, err := n.ids.New()
	if err != nil {
		return Result{}, fmt.Errorf("normalize: generating a batch id: %w", err)
	}

	result := Result{BatchID: batchID}
	index := 0
	for _, resourceLogs := range request.GetResourceLogs() {
		resourceAttributes := resourceLogs.GetResource().GetAttributes()
		for _, scopeLogs := range resourceLogs.GetScopeLogs() {
			for _, logRecord := range scopeLogs.GetLogRecords() {
				record, err := n.record(envelope, batchID, resourceAttributes, scopeLogs, logRecord)
				if err != nil {
					result.Rejected = append(result.Rejected, Rejection{Index: index, Err: err})
				} else {
					result.Records = append(result.Records, record)
				}
				index++
			}
		}
	}
	return result, nil
}

func (n *Normalizer) record(
	envelope model.TrustedEnvelope,
	batchID string,
	resourceAttributes []*common.KeyValue,
	scopeLogs *logs.ScopeLogs,
	logRecord *logs.LogRecord,
) (model.NormalizedLog, error) {
	service, err := reconcileClaims(envelope, resourceAttributes)
	if err != nil {
		return model.NormalizedLog{}, err
	}

	uid, _ := stringAttribute(logRecord.GetAttributes(), attributeRecordUID)
	recordID, err := identity.OTLPV1(envelope.SourceInstance, uid)
	if err != nil {
		// The derived-identity fallback for producers without a usable UID is
		// a separate identity version with its own behaviour.
		return model.NormalizedLog{}, fmt.Errorf("%w: %v", ErrUnusableIdentity, err)
	}

	// Everything below this point is redacted as it is converted, so no
	// unredacted value is ever assigned into the record.
	rules := newRuleSet()
	record := model.NormalizedLog{
		SchemaVersion:      model.NormalizedLogSchemaVersion,
		RecordID:           recordID,
		RecordIDVersion:    model.RecordIDVersionOTLPV1,
		IdentityQuality:    model.IdentityQualityNative,
		BatchID:            batchID,
		Source:             envelope,
		Region:             envelope.Region,
		EventTime:          unixNano(logRecord.GetTimeUnixNano()),
		ObservedTime:       unixNano(logRecord.GetObservedTimeUnixNano()),
		SeverityNumber:     int32(logRecord.GetSeverityNumber()),
		SeverityText:       logRecord.GetSeverityText(),
		SeverityClass:      severityClass(logRecord.GetSeverityNumber()),
		EventName:          logRecord.GetEventName(),
		Service:            service,
		Deployment:         model.DeploymentIdentity{ID: model.UnknownDeployment, Status: model.EnrichmentPending},
		Correlation:        correlation(logRecord),
		Body:               n.value(logRecord.GetBody(), rules),
		Attributes:         n.recordAttributes(logRecord.GetAttributes(), rules),
		ResourceAttributes: n.attributes(resourceAttributes, rules),
		ScopeAttributes:    n.attributes(scopeLogs.GetScope().GetAttributes(), rules),
	}
	record.Exception = n.exception(logRecord.GetAttributes(), rules)
	record.Redaction = model.RedactionMetadata{
		PolicyVersion: n.policy.Version(),
		RuleIDs:       rules.sorted(),
	}

	if err := record.Validate(); err != nil {
		return model.NormalizedLog{}, fmt.Errorf("normalize: produced an invalid record: %w", err)
	}
	return record, nil
}

// reconcileClaims checks what a record says about itself against what its
// source authenticated as.
//
// A claim outside the envelope's allowed set is rejected rather than replaced:
// the record is either misrouted or forged, and in both cases the true value is
// unknown. A missing claim may take an envelope default only when the source is
// dedicated to exactly one value, because otherwise the default would be a
// guess.
func reconcileClaims(envelope model.TrustedEnvelope, resourceAttributes []*common.KeyValue) (model.ServiceIdentity, error) {
	if region, ok := stringAttribute(resourceAttributes, attributeRegion); ok && region != envelope.Region {
		return model.ServiceIdentity{}, fmt.Errorf(
			"%w: record claims region %q, envelope authenticated %q",
			ErrClaimNotPermitted, region, envelope.Region)
	}

	name, hasName := stringAttribute(resourceAttributes, attributeServiceName)
	if hasName && !envelope.AllowsService(name) {
		return model.ServiceIdentity{}, fmt.Errorf(
			"%w: record claims service %q, envelope allows %s",
			ErrClaimNotPermitted, name, strings.Join(envelope.AllowedServices, ", "))
	}

	environment, hasEnvironment := stringAttribute(resourceAttributes, attributeEnvironment)
	switch {
	case hasEnvironment && !envelope.AllowsEnvironment(environment):
		return model.ServiceIdentity{}, fmt.Errorf(
			"%w: record claims environment %q, envelope allows %s",
			ErrClaimNotPermitted, environment, strings.Join(envelope.AllowedEnvironments, ", "))
	case !hasEnvironment:
		environment, _ = envelope.DefaultEnvironment()
	}

	if !hasName {
		// Ingestion continues without a service. Only service-specific agent
		// execution requires one.
		return model.ServiceIdentity{Environment: environment, Status: model.EnrichmentNotAvailable}, nil
	}

	namespace, _ := stringAttribute(resourceAttributes, attributeServiceNS)
	instance, _ := stringAttribute(resourceAttributes, attributeServiceID)
	return model.ServiceIdentity{
		Name:        name,
		Namespace:   namespace,
		InstanceID:  instance,
		Environment: environment,
		Status:      model.EnrichmentAvailable,
	}, nil
}

// severityClass maps an OTLP severity number to the class rules match on. OTLP
// numbers each class in a block of four.
func severityClass(number logs.SeverityNumber) model.SeverityClass {
	switch {
	case number >= 21:
		return model.SeverityClassFatal
	case number >= 17:
		return model.SeverityClassError
	case number >= 13:
		return model.SeverityClassWarn
	case number >= 9:
		return model.SeverityClassInfo
	case number >= 5:
		return model.SeverityClassDebug
	case number >= 1:
		return model.SeverityClassTrace
	default:
		// The normalizer does not guess a severity that was not sent.
		return model.SeverityClassUnspecified
	}
}

// correlation renders trace identifiers as canonical lowercase hex.
//
// Identifiers of the wrong length are dropped rather than padded: OTLP defines
// them as invalid, and a corrected value would correlate this record with a
// trace it does not belong to.
func correlation(logRecord *logs.LogRecord) model.CorrelationIdentity {
	var result model.CorrelationIdentity
	if traceID := logRecord.GetTraceId(); len(traceID) == 16 {
		result.TraceID = hex.EncodeToString(traceID)
	}
	if spanID := logRecord.GetSpanId(); len(spanID) == 8 && result.TraceID != "" {
		result.SpanID = hex.EncodeToString(spanID)
	}
	return result
}

// exception promotes the exception attributes a language SDK sets into the
// typed structure rules and fingerprints read.
//
// Stack frames are not extracted yet. Frame selection is what separates an
// in-application frame from a library one, and it is defined with the
// fingerprint. The redacted stack trace remains available as an attribute.
func (n *Normalizer) exception(attributes []*common.KeyValue, rules *ruleSet) *model.NormalizedException {
	exceptionType, hasType := stringAttribute(attributes, attributeExceptionType)
	message, hasMessage := stringAttribute(attributes, attributeExceptionMsg)
	if !hasType && !hasMessage {
		return nil
	}
	safeMessage := n.text(message, rules)
	if strings.TrimSpace(exceptionType) == "" && strings.TrimSpace(safeMessage) == "" {
		return nil
	}
	return &model.NormalizedException{Type: exceptionType, SafeMessage: safeMessage}
}

func (n *Normalizer) text(text string, rules *ruleSet) string {
	result := n.policy.Text(text)
	rules.add(result.RuleIDs)
	return result.Text
}

// recordAttributes converts a log record's own attributes.
//
// The producer's record UID is dropped once identity has been derived from it.
// Its only purpose was to establish record_id, which now carries that identity;
// keeping it would leave a redaction placeholder in its place, which names
// nothing and correlates with nothing.
func (n *Normalizer) recordAttributes(attributes []*common.KeyValue, rules *ruleSet) map[string]model.SafeValue {
	converted := n.attributes(attributes, rules)
	delete(converted, attributeRecordUID)
	if len(converted) == 0 {
		return nil
	}
	return converted
}

func (n *Normalizer) attributes(attributes []*common.KeyValue, rules *ruleSet) map[string]model.SafeValue {
	if len(attributes) == 0 {
		return nil
	}
	converted := make(map[string]model.SafeValue, len(attributes))
	for _, attribute := range attributes {
		key := attribute.GetKey()
		if strings.TrimSpace(key) == "" {
			// An unnamed attribute cannot be matched or redacted by name, and
			// a valid record may not carry one. Its siblings survive.
			continue
		}
		// OTLP forbids duplicate keys but does not prevent them. The first
		// occurrence wins, so the outcome is defined rather than dependent on
		// map assignment order.
		if _, seen := converted[key]; seen {
			continue
		}
		converted[key] = n.value(attribute.GetValue(), rules)
	}
	if len(converted) == 0 {
		return nil
	}
	return converted
}

// value converts an OTLP value, redacting every string it contains.
func (n *Normalizer) value(value *common.AnyValue, rules *ruleSet) model.SafeValue {
	switch value.GetValue().(type) {
	case nil:
		return model.SafeValue{}
	case *common.AnyValue_StringValue:
		return model.SafeString(n.text(value.GetStringValue(), rules))
	case *common.AnyValue_IntValue:
		return model.SafeInt(value.GetIntValue())
	case *common.AnyValue_DoubleValue:
		return model.SafeDouble(value.GetDoubleValue())
	case *common.AnyValue_BoolValue:
		return model.SafeBool(value.GetBoolValue())
	case *common.AnyValue_ArrayValue:
		items := value.GetArrayValue().GetValues()
		converted := make([]model.SafeValue, 0, len(items))
		for _, item := range items {
			converted = append(converted, n.value(item, rules))
		}
		return model.SafeSlice(converted...)
	case *common.AnyValue_KvlistValue:
		return model.SafeMap(n.attributes(value.GetKvlistValue().GetValues(), rules))
	default:
		// Bytes and anything a later OTLP version adds cannot be shown to be
		// safe, so the content is withheld rather than passed through.
		return model.Withheld("unsupported value kind")
	}
}

func stringAttribute(attributes []*common.KeyValue, key string) (string, bool) {
	for _, attribute := range attributes {
		if attribute.GetKey() != key {
			continue
		}
		if _, ok := attribute.GetValue().GetValue().(*common.AnyValue_StringValue); !ok {
			return "", false
		}
		return attribute.GetValue().GetStringValue(), true
	}
	return "", false
}

func unixNano(value uint64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(value)).UTC()
}

// ruleSet collects the redaction rules that fired anywhere in one record.
type ruleSet struct{ seen map[string]bool }

func newRuleSet() *ruleSet { return &ruleSet{seen: map[string]bool{}} }

func (r *ruleSet) add(ids []string) {
	for _, id := range ids {
		r.seen[id] = true
	}
}

func (r *ruleSet) sorted() []string {
	if len(r.seen) == 0 {
		return nil
	}
	sorted := make([]string, 0, len(r.seen))
	for id := range r.seen {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	return sorted
}
