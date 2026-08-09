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
	"reflect"
	"regexp"
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
	attributeServiceName    = "service.name"
	attributeServiceNS      = "service.namespace"
	attributeServiceID      = "service.instance.id"
	attributeEnvironment    = "deployment.environment.name"
	attributeRegion         = "cloud.region"
	attributeRecordUID      = "log.record.uid"
	attributeExceptionType  = "exception.type"
	attributeExceptionMsg   = "exception.message"
	attributeExceptionStack = "exception.stacktrace"
)

// ErrClaimNotPermitted means a record claimed an identity its authenticated
// source is not allowed to speak for. The claim is refused rather than
// corrected, because the correct value is not knowable.
var ErrClaimNotPermitted = errors.New("claim is not permitted by the trusted envelope")

// ErrUnusableIdentity means no record identity could be established, so the
// record cannot participate in effectively-once processing.
var ErrUnusableIdentity = errors.New("record identity could not be established")

var (
	ErrInvalidRecordMapping    = errors.New("normalization accepted-record index mapping is invalid")
	ErrInvalidConfiguration    = errors.New("normalization configuration is invalid")
	ErrMaterializationTooLarge = errors.New("normalization projected materialization exceeds limit")
	ErrUnsafeEnvelope          = errors.New("normalization trusted envelope contains prohibited content")
	ErrMissingTimestamps       = errors.New("normalization record has neither event nor observed time")
	ErrMissingContent          = errors.New("normalization record has neither body nor event name")
	ErrProhibitedContent       = errors.New("normalization record failed prohibited-content validation")
	ErrStructuralRecord        = errors.New("normalization produced a structurally invalid record")
)

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
	BatchID string
	Records []model.NormalizedLog
	// RecordOriginalIndexes is aligned with Records and retains each record's
	// position in the original unfiltered wire request.
	RecordOriginalIndexes      []int
	Rejected                   []Rejection
	ProjectedMaterializedBytes int64
}

// Normalizer converts admitted OTLP batches.
type Normalizer struct {
	policy               *redact.Policy
	ids                  ids.Source
	resolver             DeploymentResolver
	maxMaterializedBytes int64
	configurationErr     error
}

// DeploymentResolver supplies the release a record belongs to.
//
// It is declared here, in the package that consumes it, and deliberately says
// nothing about caches or providers: deployment identity is part of grouping,
// so normalization needs the answer, not the machinery that produced it.
//
// An implementation MUST NOT block. architecture.md: "Detection and persistence
// never wait for external metadata." A resolver that waited would put an
// external dependency inside the ingestion path.
type DeploymentResolver interface {
	Deployment(service model.ServiceIdentity, region string) model.DeploymentIdentity
}

// WithDeploymentResolver supplies deployment identity. Without one, every
// record groups under unknown-deployment with enrichment pending, which is what
// data-model.md specifies for a record whose deployment is not yet known.
func WithDeploymentResolver(resolver DeploymentResolver) Option {
	return func(n *Normalizer) { n.resolver = resolver }
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

const DefaultMaxMaterializedBytes int64 = 16 << 20

// WithMaxMaterializedBytes configures a downward-only aggregate normalized
// batch projection limit enforced before ID generation.
func WithMaxMaterializedBytes(maximum int64) Option {
	return func(n *Normalizer) {
		if maximum < 1 || maximum > DefaultMaxMaterializedBytes {
			n.configurationErr = ErrInvalidConfiguration
			return
		}
		n.maxMaterializedBytes = maximum
	}
}

// New returns a Normalizer using the minimal redaction policy and generated
// batch identifiers.
func New(opts ...Option) *Normalizer {
	n := &Normalizer{policy: redact.MinimalPolicy(), ids: ids.System(), maxMaterializedBytes: DefaultMaxMaterializedBytes}
	for _, opt := range opts {
		if opt == nil {
			n.configurationErr = ErrInvalidConfiguration
			continue
		}
		opt(n)
	}
	return n
}

// deployment resolves the release this record belongs to, or reports that it is
// not yet known. It never waits: the resolver's contract forbids blocking.
func (n *Normalizer) deployment(service model.ServiceIdentity, region string) model.DeploymentIdentity {
	if n.resolver == nil {
		return model.DeploymentIdentity{ID: model.UnknownDeployment, Status: model.EnrichmentPending}
	}
	resolved := n.resolver.Deployment(service, region)
	// A resolver is outside this package and may answer with anything, including
	// a zero value. Normalization is the last place a record can still be made
	// valid, so an incomplete answer is completed here rather than becoming a
	// record-local rejection an operator would have to trace back to a resolver.
	if resolved.ID == "" {
		// Every record needs a grouping key, and unknown-deployment is the one
		// data-model.md names for a deployment that is not known.
		resolved.ID = model.UnknownDeployment
		if resolved.Status == model.EnrichmentAvailable {
			// "Available" with nothing to show is not available. Believing it
			// would group real records under the unknown-deployment key while
			// claiming their deployment was resolved.
			resolved.Status = model.EnrichmentPending
		}
	}
	if resolved.Status == "" {
		resolved.Status = model.EnrichmentPending
	}
	return resolved
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
	if !n.validConfiguration() {
		return Result{}, ErrInvalidConfiguration
	}
	recordCount := requestRecordCount(request)
	if recordCount > 10_000 {
		return Result{}, ErrMaterializationTooLarge
	}
	originalIndexes := make([]int, recordCount)
	for index := range originalIndexes {
		originalIndexes[index] = index
	}
	return n.MappedRequest(envelope, request, originalIndexes)
}

// MappedRequest normalizes an accepted-only request using originalIndexes to
// preserve raw-wire identity through record-local rejections. The mapping is
// validated before batch-ID generation.
func (n *Normalizer) MappedRequest(
	envelope model.TrustedEnvelope,
	request *collectorlogs.ExportLogsServiceRequest,
	originalIndexes []int,
) (Result, error) {
	if !n.validConfiguration() {
		return Result{}, ErrInvalidConfiguration
	}
	recordCount := requestRecordCount(request)
	if !validOriginalIndexes(recordCount, originalIndexes) {
		return Result{}, ErrInvalidRecordMapping
	}
	if err := envelope.Validate(); err != nil {
		// Without a valid envelope there is no authenticated source, so no
		// claim in the batch can be checked against anything.
		return Result{}, fmt.Errorf("normalize: trusted envelope is not usable: %w", err)
	}
	if n.validateEnvelopeContent(envelope) != nil {
		return Result{}, ErrUnsafeEnvelope
	}
	projectedMaterializedBytes, err := projectMaterialization(envelope, request, n.maxMaterializedBytes)
	if err != nil {
		return Result{}, err
	}
	envelope = cloneEnvelope(envelope)

	batchID, err := n.ids.New()
	if err != nil {
		return Result{}, fmt.Errorf("normalize: generating a batch id: %w", err)
	}

	result := Result{BatchID: batchID, ProjectedMaterializedBytes: projectedMaterializedBytes}
	acceptedIndex := 0
	for _, resourceLogs := range request.GetResourceLogs() {
		resourceAttributes := resourceLogs.GetResource().GetAttributes()
		for _, scopeLogs := range resourceLogs.GetScopeLogs() {
			for _, logRecord := range scopeLogs.GetLogRecords() {
				originalIndex := originalIndexes[acceptedIndex]
				record, err := n.record(envelope, batchID, resourceAttributes, scopeLogs, logRecord)
				if err != nil {
					result.Rejected = append(result.Rejected, Rejection{Index: originalIndex, Err: err})
				} else {
					result.Records = append(result.Records, record)
					result.RecordOriginalIndexes = append(result.RecordOriginalIndexes, originalIndex)
				}
				acceptedIndex++
			}
		}
	}
	return result, nil
}

func (n *Normalizer) validateEnvelopeContent(envelope model.TrustedEnvelope) error {
	texts := []string{
		string(envelope.SourceType), envelope.SourceAccount, envelope.Region,
		envelope.SourceInstance, envelope.CredentialIdentity,
	}
	texts = append(texts, envelope.AllowedEnvironments...)
	texts = append(texts, envelope.AllowedServices...)
	for _, text := range texts {
		if n.policy.ValidateText(text) != nil {
			return ErrUnsafeEnvelope
		}
	}
	return nil
}

func requestRecordCount(request *collectorlogs.ExportLogsServiceRequest) int {
	count := 0
	for _, resourceLogs := range request.GetResourceLogs() {
		for _, scopeLogs := range resourceLogs.GetScopeLogs() {
			count += len(scopeLogs.GetLogRecords())
		}
	}
	return count
}

func (n *Normalizer) validConfiguration() bool {
	return n != nil && n.policy != nil && !nilInterface(n.ids) && n.configurationErr == nil &&
		n.maxMaterializedBytes >= 1 && n.maxMaterializedBytes <= DefaultMaxMaterializedBytes
}

func validOriginalIndexes(recordCount int, indexes []int) bool {
	if len(indexes) != recordCount {
		return false
	}
	previous := -1
	for _, index := range indexes {
		if index < 0 || index <= previous {
			return false
		}
		previous = index
	}
	return true
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func cloneEnvelope(envelope model.TrustedEnvelope) model.TrustedEnvelope {
	envelope.AllowedEnvironments = append([]string(nil), envelope.AllowedEnvironments...)
	envelope.AllowedServices = append([]string(nil), envelope.AllowedServices...)
	return envelope
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

	uid, hasUID, err := producerRecordUID(logRecord.GetAttributes())
	if err != nil {
		return model.NormalizedLog{}, fmt.Errorf("%w: %w", ErrUnusableIdentity, err)
	}

	// Everything below this point is redacted as it is converted, so no
	// unredacted value is ever assigned into the record.
	rules := newRuleSet()
	withheld := newPathSet()
	sourceEventTime := unixNano(logRecord.GetTimeUnixNano())
	sourceObservedTime := unixNano(logRecord.GetObservedTimeUnixNano())
	observedTime, observedInferred, observedInferenceReason, err := normalizeObservedTime(sourceEventTime, sourceObservedTime)
	if err != nil {
		return model.NormalizedLog{}, err
	}
	eventTime, inferred, inferenceReason := normalizeEventTime(sourceEventTime, observedTime)
	record := model.NormalizedLog{
		SchemaVersion:               model.NormalizedLogSchemaVersion,
		BatchID:                     batchID,
		Source:                      cloneEnvelope(envelope),
		Region:                      envelope.Region,
		EventTime:                   eventTime,
		ObservedTime:                observedTime,
		TimestampInferred:           inferred,
		TimestampInferenceReason:    inferenceReason,
		ObservedTimeInferred:        observedInferred,
		ObservedTimeInferenceReason: observedInferenceReason,
		SeverityNumber:              int32(logRecord.GetSeverityNumber()),
		SeverityText:                n.text(logRecord.GetSeverityText(), "severity_text", rules, withheld),
		SeverityClass:               severityClass(logRecord.GetSeverityNumber()),
		EventName:                   n.text(logRecord.GetEventName(), "event_name", rules, withheld),
		Service:                     service,
		Deployment:                  n.deployment(service, envelope.Region),
		Correlation:                 correlation(logRecord),
		Body:                        n.value(logRecord.GetBody(), "body", rules, withheld),
		Attributes:                  n.recordAttributes(logRecord.GetAttributes(), rules, withheld),
		ResourceAttributes:          n.attributes(resourceAttributes, "resource_attributes", rules, withheld),
		ScopeAttributes:             n.attributes(scopeLogs.GetScope().GetAttributes(), "scope_attributes", rules, withheld),
	}
	record.Exception = n.exception(logRecord.GetAttributes(), service.Name, rules, withheld)
	record.Redaction = model.RedactionMetadata{
		PolicyVersion:  n.policy.Version(),
		RuleIDs:        rules.sorted(),
		WithheldFields: withheld.sorted(),
	}
	if record.Body.IsZero() && strings.TrimSpace(record.EventName) == "" {
		return model.NormalizedLog{}, ErrMissingContent
	}
	if err := n.policy.ValidateRecord(record); err != nil {
		return model.NormalizedLog{}, fmt.Errorf("%w: %v", ErrProhibitedContent, err)
	}
	if hasUID {
		record.RecordID, err = identity.OTLPV1(envelope.SourceInstance, uid)
		record.RecordIDVersion = model.RecordIDVersionOTLPV1
		record.IdentityQuality = model.IdentityQualityNative
	} else {
		record.RecordID, err = identity.DerivedV1(record)
		record.RecordIDVersion = model.RecordIDVersionDerivedV1
		record.IdentityQuality = model.IdentityQualityDerived
	}
	if err != nil {
		return model.NormalizedLog{}, fmt.Errorf("%w: %w", ErrUnusableIdentity, err)
	}

	if err := record.Validate(); err != nil {
		return model.NormalizedLog{}, fmt.Errorf("%w: %v", ErrStructuralRecord, err)
	}
	return record, nil
}

// producerRecordUID distinguishes a genuinely absent UID, which uses the
// measured derived fallback, from a malformed UID claim. Falling back from a
// malformed claim would hide a producer defect and could assign a second
// identity to a record that previously used the native path. OTLP duplicate
// attributes retain the existing first-value behaviour; all UID copies are
// removed from evidence after that first value establishes identity.
func producerRecordUID(attributes []*common.KeyValue) (string, bool, error) {
	for _, attribute := range attributes {
		if attribute.GetKey() != attributeRecordUID {
			continue
		}
		value, ok := attribute.GetValue().GetValue().(*common.AnyValue_StringValue)
		if !ok {
			return "", false, identity.ErrInvalidRecordUID
		}
		return value.StringValue, true, nil
	}
	return "", false, nil
}

const (
	maximumEventAge    = 24 * time.Hour
	maximumEventFuture = 5 * time.Minute
)

// EventTime applies the missing-data policy for timestamps, and is exported so
// that every source adapter answers the question the same way. A second
// implementation of "is this event time believable" would drift from this one,
// and the two would disagree about which records can hold a window open.
//
// It reports the time to process the record at, whether that time was inferred
// rather than reported, and why.
func EventTime(event, observed time.Time) (time.Time, bool, string) {
	return normalizeEventTime(event, observed)
}

func normalizeEventTime(event, observed time.Time) (time.Time, bool, string) {
	if event.IsZero() {
		return observed, true, "event_time_missing"
	}
	if !observed.IsZero() && (event.Before(observed.Add(-maximumEventAge)) || event.After(observed.Add(maximumEventFuture))) {
		return observed, true, "event_time_outside_valid_range"
	}
	return event, false, ""
}

func normalizeObservedTime(event, observed time.Time) (time.Time, bool, string, error) {
	if !observed.IsZero() {
		return observed, false, "", nil
	}
	if !event.IsZero() {
		return event, true, "observed_time_missing_event_time_used", nil
	}
	return time.Time{}, false, "", ErrMissingTimestamps
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
	region, hasRegion, err := uniqueTrustedClaim(resourceAttributes, attributeRegion)
	if err != nil {
		return model.ServiceIdentity{}, err
	}
	if hasRegion && region != envelope.Region {
		return model.ServiceIdentity{}, fmt.Errorf(
			"%w: cloud.region conflicts with authenticated source",
			ErrClaimNotPermitted)
	}

	name, hasName, err := uniqueTrustedClaim(resourceAttributes, attributeServiceName)
	if err != nil {
		return model.ServiceIdentity{}, err
	}
	if hasName && !envelope.AllowsService(name) {
		return model.ServiceIdentity{}, fmt.Errorf(
			"%w: service.name is outside the authenticated source policy",
			ErrClaimNotPermitted)
	}

	environment, hasEnvironment, err := uniqueTrustedClaim(resourceAttributes, attributeEnvironment)
	if err != nil {
		return model.ServiceIdentity{}, err
	}
	switch {
	case hasEnvironment && !envelope.AllowsEnvironment(environment):
		return model.ServiceIdentity{}, fmt.Errorf(
			"%w: deployment.environment.name is outside the authenticated source policy",
			ErrClaimNotPermitted)
	case !hasEnvironment:
		environment, _ = envelope.DefaultEnvironment()
	}

	if !hasName {
		// Ingestion continues without a service. Only service-specific agent
		// execution requires one.
		return model.ServiceIdentity{Environment: environment, Status: model.EnrichmentNotAvailable}, nil
	}

	instance, _ := stringAttribute(resourceAttributes, attributeServiceID)
	status := model.EnrichmentAvailable
	if strings.TrimSpace(environment) == "" {
		status = model.EnrichmentNotAvailable
	}
	return model.ServiceIdentity{
		Name:        name,
		InstanceID:  instance,
		Environment: environment,
		Status:      status,
	}, nil
}

// uniqueTrustedClaim checks every occurrence because OTLP forbids duplicate
// keys but a hostile producer can still encode them. Accepting only the first
// would let a later conflicting claim evade trusted-envelope reconciliation.
func uniqueTrustedClaim(attributes []*common.KeyValue, key string) (string, bool, error) {
	var value string
	found := false
	for _, attribute := range attributes {
		if attribute.GetKey() != key {
			continue
		}
		stringValue, ok := attribute.GetValue().GetValue().(*common.AnyValue_StringValue)
		if !ok {
			return "", false, fmt.Errorf("%w: %s claim must be a string", ErrClaimNotPermitted, key)
		}
		if found && stringValue.StringValue != value {
			return "", false, fmt.Errorf(
				"%w: conflicting duplicate %s claims",
				ErrClaimNotPermitted, key)
		}
		value = stringValue.StringValue
		found = true
	}
	return value, found, nil
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
func (n *Normalizer) exception(attributes []*common.KeyValue, serviceName string, rules *ruleSet, withheld *pathSet) *model.NormalizedException {
	exceptionType, hasType := stringAttribute(attributes, attributeExceptionType)
	message, hasMessage := stringAttribute(attributes, attributeExceptionMsg)
	stackTrace, hasStack := stringAttribute(attributes, attributeExceptionStack)
	if !hasType && !hasMessage && !hasStack {
		return nil
	}
	safeMessage := n.text(message, "exception.safe_message", rules, withheld)
	frames := n.stackFrames(stackTrace, serviceName, rules, withheld)
	if strings.TrimSpace(exceptionType) == "" && strings.TrimSpace(safeMessage) == "" && len(frames) == 0 {
		return nil
	}
	return &model.NormalizedException{Type: n.text(exceptionType, "exception.type", rules, withheld), SafeMessage: safeMessage, StackFrames: frames}
}

var stackFramePattern = regexp.MustCompile(`^\s*at\s+(.+?)\s+\((.+?):(\d+)(?::\d+)?\)\s*$`)

func (n *Normalizer) stackFrames(stackTrace, serviceName string, rules *ruleSet, withheld *pathSet) []model.StackFrame {
	stackSafety := n.policy.SafeText(stackTrace)
	rules.add(stackSafety.RuleIDs)
	if stackSafety.Value.Kind == model.SafeKindWithheld {
		withheld.add("exception.stack_frames")
		return nil
	}
	var frames []model.StackFrame
	for _, line := range strings.Split(stackTrace, "\n") {
		match := stackFramePattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		location := match[2]
		module := strings.TrimSuffix(location, ".go")
		index := len(frames)
		frames = append(frames, model.StackFrame{
			Function:      n.text(match[1], fmt.Sprintf("exception.stack_frames[%d].function", index), rules, withheld),
			Module:        n.text(module, fmt.Sprintf("exception.stack_frames[%d].module", index), rules, withheld),
			File:          n.text(location, fmt.Sprintf("exception.stack_frames[%d].file", index), rules, withheld),
			InApplication: isApplicationLocation(location, serviceName),
		})
	}
	return frames
}

func isApplicationLocation(location, serviceName string) bool {
	lowered := strings.ToLower(strings.TrimSpace(location))
	serviceModule := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(serviceName)), "service")
	if serviceModule != "" && (strings.HasPrefix(lowered, serviceModule+"/") ||
		strings.Contains(lowered, "/"+serviceModule+"/")) {
		return true
	}
	// Only paths known to be runtime/standard-library locations are excluded.
	// Unknown modules remain application frames; a generic directory name such
	// as internal/ is commonly used by application code and proves nothing.
	for _, library := range []string{
		"net/", "runtime/", "database/sql", "context/", "crypto/", "encoding/",
		"fmt/", "io/", "os/", "sync/", "syscall/", "time/", "vendor/",
		"node:", "java.", "sun.",
	} {
		if strings.HasPrefix(lowered, library) {
			return false
		}
	}
	return !strings.Contains(lowered, "/pkg/mod/") && !strings.Contains(lowered, "node_modules/")
}

func (n *Normalizer) text(text, path string, rules *ruleSet, withheld *pathSet) string {
	result := n.policy.SafeText(text)
	rules.add(result.RuleIDs)
	if result.Value.Kind == model.SafeKindWithheld {
		withheld.add(path)
		return redact.WithheldText
	}
	return result.Value.String
}

// recordAttributes converts a log record's own attributes.
//
// The producer's record UID is dropped once identity has been derived from it.
// Its only purpose was to establish record_id, which now carries that identity;
// keeping it would leave a redaction placeholder in its place, which names
// nothing and correlates with nothing.
func (n *Normalizer) recordAttributes(attributes []*common.KeyValue, rules *ruleSet, withheld *pathSet) map[string]model.SafeValue {
	filtered := make([]*common.KeyValue, 0, len(attributes))
	for _, attribute := range attributes {
		if attribute.GetKey() != attributeRecordUID {
			filtered = append(filtered, attribute)
		}
	}
	converted := n.attributes(filtered, "attributes", rules, withheld)
	if len(converted) == 0 {
		return nil
	}
	return converted
}

func (n *Normalizer) attributes(attributes []*common.KeyValue, root string, rules *ruleSet, withheld *pathSet) map[string]model.SafeValue {
	if len(attributes) == 0 {
		return nil
	}
	type classifiedAttribute struct {
		attribute *common.KeyValue
		rawKey    string
		safeKey   string
		unsafe    bool
	}
	classified := make([]classifiedAttribute, 0, len(attributes))
	safeKeyCounts := make(map[string]int, len(attributes))
	for _, attribute := range attributes {
		key := attribute.GetKey()
		if strings.TrimSpace(key) == "" {
			continue
		}
		keyResult := n.policy.SafeFieldName(key)
		rules.add(keyResult.RuleIDs)
		entry := classifiedAttribute{attribute: attribute, rawKey: key, unsafe: keyResult.Value.Kind == model.SafeKindWithheld}
		if !entry.unsafe {
			entry.safeKey = keyResult.Value.String
			safeKeyCounts[entry.safeKey]++
		}
		classified = append(classified, entry)
	}

	converted := make(map[string]model.SafeValue, len(classified))
	withheldKeyOrdinal := 0
	nextWithheldKey := func() string {
		for {
			withheldKeyOrdinal++
			candidate := "[WITHHELD_FIELD_NAME]"
			if withheldKeyOrdinal > 1 {
				candidate = fmt.Sprintf("[WITHHELD_FIELD_NAME]#%d", withheldKeyOrdinal)
			}
			if _, occupied := converted[candidate]; !occupied {
				return candidate
			}
		}
	}
	for _, entry := range classified {
		unsafeKey := entry.unsafe || safeKeyCounts[entry.safeKey] > 1
		safeKey := entry.safeKey
		if unsafeKey {
			safeKey = nextWithheldKey()
			if !entry.unsafe {
				rules.add([]string{"safety.map_key_collision"})
			}
		}
		path := root + "." + safeKey
		if unsafeKey {
			converted[safeKey] = model.Withheld(redact.ReasonRedactionFailure)
			withheld.add(path)
			continue
		}
		if n.policy.SensitiveField(entry.rawKey) {
			converted[safeKey] = model.SafeString("[REDACTED_FIELD]")
			rules.add([]string{"service.sensitive_field"})
			continue
		}
		converted[safeKey] = n.value(entry.attribute.GetValue(), path, rules, withheld)
	}
	if len(converted) == 0 {
		return nil
	}
	return converted
}

// value converts an OTLP value, redacting every string it contains.
func (n *Normalizer) value(value *common.AnyValue, path string, rules *ruleSet, withheld *pathSet) model.SafeValue {
	switch value.GetValue().(type) {
	case nil:
		return model.SafeValue{}
	case *common.AnyValue_StringValue:
		result := n.policy.StructuredText(value.GetStringValue())
		rules.add(result.RuleIDs)
		collectWithheld(result.Value, path, withheld)
		return result.Value
	case *common.AnyValue_IntValue:
		return model.SafeInt(value.GetIntValue())
	case *common.AnyValue_DoubleValue:
		return model.SafeDouble(value.GetDoubleValue())
	case *common.AnyValue_BoolValue:
		return model.SafeBool(value.GetBoolValue())
	case *common.AnyValue_ArrayValue:
		items := value.GetArrayValue().GetValues()
		converted := make([]model.SafeValue, 0, len(items))
		for index, item := range items {
			converted = append(converted, n.value(item, fmt.Sprintf("%s[%d]", path, index), rules, withheld))
		}
		return model.SafeSlice(converted...)
	case *common.AnyValue_KvlistValue:
		return model.SafeMap(n.attributes(value.GetKvlistValue().GetValues(), path, rules, withheld))
	default:
		// Bytes and anything a later OTLP version adds cannot be shown to be
		// safe, so the content is withheld rather than passed through.
		withheld.add(path)
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

type pathSet struct{ seen map[string]bool }

func newPathSet() *pathSet { return &pathSet{seen: map[string]bool{}} }

func (p *pathSet) add(path string) { p.seen[path] = true }

func (p *pathSet) sorted() []string {
	if len(p.seen) == 0 {
		return nil
	}
	paths := make([]string, 0, len(p.seen))
	for path := range p.seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func collectWithheld(value model.SafeValue, path string, withheld *pathSet) {
	switch value.Kind {
	case model.SafeKindWithheld:
		withheld.add(path)
	case model.SafeKindMap:
		for key, child := range value.Map {
			collectWithheld(child, path+"."+key, withheld)
		}
	case model.SafeKindSlice:
		for index, child := range value.Slice {
			collectWithheld(child, fmt.Sprintf("%s[%d]", path, index), withheld)
		}
	}
}
