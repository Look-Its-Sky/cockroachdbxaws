package cloudwatch

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/identity"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/normalize"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
)

// Attribute names for the native locator a record carries. They follow the
// OpenTelemetry AWS logs convention so that a CloudWatch record and an OTLP
// record describing the same stream agree on the key.
const (
	AttributeLogGroup  = "aws.log.group.name"
	AttributeLogStream = "aws.log.stream.name"
	AttributeEventID   = "aws.log.event.id"
)

// DefaultRawReferenceTTL is how long a regional reference to the raw stream is
// assumed usable. It is a default, not a guarantee: the real bound is the log
// group's retention, which the adapter cannot read without another permission.
const DefaultRawReferenceTTL = 14 * 24 * time.Hour

// severityKeys are the message fields a structured logger declares its level
// in, in the order they are consulted. Only a declared field is read; a level
// word appearing in prose is not a declaration, and the missing-data policy is
// explicit that the normalizer does not guess.
var severityKeys = []string{"level", "severity", "severity_text", "log.level", "loglevel", "syslog.severity"}

// severityTokens is the closed vocabulary a declared level is mapped through.
// The value stored in SeverityText is this table's canonical name and never the
// token as it arrived, so a hostile level value cannot reach a persisted field.
var severityTokens = map[string]struct {
	number int32
	text   string
	class  model.SeverityClass
}{
	"trace":       {1, "TRACE", model.SeverityClassTrace},
	"finest":      {1, "TRACE", model.SeverityClassTrace},
	"verbose":     {1, "TRACE", model.SeverityClassTrace},
	"debug":       {5, "DEBUG", model.SeverityClassDebug},
	"fine":        {5, "DEBUG", model.SeverityClassDebug},
	"info":        {9, "INFO", model.SeverityClassInfo},
	"information": {9, "INFO", model.SeverityClassInfo},
	"notice":      {9, "INFO", model.SeverityClassInfo},
	"warn":        {13, "WARN", model.SeverityClassWarn},
	"warning":     {13, "WARN", model.SeverityClassWarn},
	"error":       {17, "ERROR", model.SeverityClassError},
	"err":         {17, "ERROR", model.SeverityClassError},
	"severe":      {17, "ERROR", model.SeverityClassError},
	"fatal":       {21, "FATAL", model.SeverityClassFatal},
	"critical":    {21, "FATAL", model.SeverityClassFatal},
	"crit":        {21, "FATAL", model.SeverityClassFatal},
	"alert":       {21, "FATAL", model.SeverityClassFatal},
	"emerg":       {21, "FATAL", model.SeverityClassFatal},
	"emergency":   {21, "FATAL", model.SeverityClassFatal},
	"panic":       {21, "FATAL", model.SeverityClassFatal},
}

// MapperConfig is the operator-supplied half of a CloudWatch record.
//
// CloudWatch events carry no authenticated service identity of their own, so
// service and environment come from the group's configuration, which is trusted
// in the same way the adapter's AWS credentials are. Nothing in a message can
// change them.
type MapperConfig struct {
	Policy *redact.Policy

	// Account and Region are the adapter's authenticated boundary and are part
	// of record identity.
	Account string
	Region  string
	// SourceInstance and CredentialIdentity describe the adapter replica and the
	// IAM principal it reads as, and are retained for audit.
	SourceInstance     string
	CredentialIdentity string
	// AllowedServices and AllowedEnvironments are the envelope's authorization.
	AllowedServices     []string
	AllowedEnvironments []string

	// Service and Environment are this log group's configured identity. Both
	// must be inside the allowed sets, or the configuration authorizes less than
	// it declares.
	Service     string
	Environment string

	// Classification is the access classification a regional reference to the
	// raw stream carries.
	Classification string
	// RawReferenceTTL is how long that reference is assumed usable.
	RawReferenceTTL time.Duration
}

// Mapper turns native CloudWatch events into canonical records.
//
// Redaction runs while a record is being built rather than afterwards, because
// a NormalizedLog is defined as already safe. Building one from raw content and
// cleaning it up would create a window in which unredacted values exist in a
// structure the rest of the service may read.
type Mapper struct {
	policy          *redact.Policy
	envelope        model.TrustedEnvelope
	service         string
	environment     string
	classification  string
	rawReferenceTTL time.Duration
}

// NewMapper validates the configuration completely before returning a mapper,
// so a misconfiguration is a startup failure rather than a per-record one.
func NewMapper(config MapperConfig) (*Mapper, error) {
	if config.Policy == nil {
		return nil, fmt.Errorf("%w: a redaction policy is required", ErrInvalidConfig)
	}
	for name, value := range map[string]string{
		"account":             config.Account,
		"region":              config.Region,
		"source instance":     config.SourceInstance,
		"credential identity": config.CredentialIdentity,
		"service":             config.Service,
		"environment":         config.Environment,
	} {
		if !trimmed(value) {
			return nil, fmt.Errorf("%w: %s is required and must be an exact, unpadded value", ErrInvalidConfig, name)
		}
	}
	if len(config.AllowedServices) == 0 || len(config.AllowedEnvironments) == 0 {
		return nil, fmt.Errorf("%w: an empty allowed set would admit no record at all", ErrInvalidConfig)
	}
	envelope := model.TrustedEnvelope{
		SourceType:          model.SourceTypeCloudWatch,
		SourceAccount:       config.Account,
		Region:              config.Region,
		AllowedEnvironments: append([]string(nil), config.AllowedEnvironments...),
		AllowedServices:     append([]string(nil), config.AllowedServices...),
		SourceInstance:      config.SourceInstance,
		CredentialIdentity:  config.CredentialIdentity,
	}
	if !envelope.AllowsService(config.Service) {
		return nil, fmt.Errorf("%w: the configured service is outside the envelope's allowed set", ErrInvalidConfig)
	}
	if !envelope.AllowsEnvironment(config.Environment) {
		return nil, fmt.Errorf("%w: the configured environment is outside the envelope's allowed set", ErrInvalidConfig)
	}
	if !validClassification(config.Classification) {
		return nil, fmt.Errorf("%w: classification %q is not one of PUBLIC, INTERNAL, SENSITIVE, RESTRICTED",
			ErrInvalidConfig, config.Classification)
	}
	ttl := config.RawReferenceTTL
	if ttl == 0 {
		ttl = DefaultRawReferenceTTL
	}
	if ttl < 0 {
		return nil, fmt.Errorf("%w: a raw reference cannot expire before it is written", ErrInvalidConfig)
	}
	// The envelope's own text is validated once here rather than per record. A
	// prohibited value in configuration is a deployment fault, and discovering
	// it per record would make every record look individually invalid.
	if err := validateEnvelopeContent(config.Policy, envelope); err != nil {
		return nil, err
	}
	return &Mapper{
		policy: config.Policy, envelope: envelope, service: config.Service,
		environment: config.Environment, classification: config.Classification, rawReferenceTTL: ttl,
	}, nil
}

// Envelope returns the trusted envelope records are admitted under, stamped
// with a receipt time. The allowed sets are copied on every call: they travel
// into ingestion, and sharing the mapper's backing arrays would let one batch's
// mutation change the set every later batch is authorized against.
func (m *Mapper) Envelope(receivedAt time.Time) model.TrustedEnvelope {
	envelope := m.envelope
	envelope.AllowedEnvironments = append([]string(nil), m.envelope.AllowedEnvironments...)
	envelope.AllowedServices = append([]string(nil), m.envelope.AllowedServices...)
	envelope.ReceivedAt = receivedAt.UTC()
	return envelope
}

// Record maps one event.
//
// A returned ErrUnusableRecord is record-local: this event cannot become a
// record and its siblings are unaffected. A returned ErrRegionalBoundary is
// not, because a crossed boundary is a deployment fault whose blast radius is
// every record in the batch.
func (m *Mapper) Record(batchID string, group Group, event Event, receivedAt time.Time) (model.NormalizedLog, error) {
	if m == nil || m.policy == nil {
		return model.NormalizedLog{}, fmt.Errorf("%w: mapper is not configured", ErrInvalidConfig)
	}
	if !group.valid() {
		return model.NormalizedLog{}, fmt.Errorf("%w: an incomplete group names no boundary", ErrRegionalBoundary)
	}
	if group.Region != m.envelope.Region || group.Account != m.envelope.SourceAccount {
		// Not record-local. Reading another region's or account's stream is a
		// misconfiguration, and quarantining the records would destroy every
		// acknowledged payload for what is a deployment typo.
		return model.NormalizedLog{}, fmt.Errorf(
			"%w: this adapter serves one account and region only", ErrRegionalBoundary)
	}
	if !trimmed(batchID) {
		return model.NormalizedLog{}, fmt.Errorf("%w: a batch id is required", ErrInvalidConfig)
	}

	stream := Stream{Group: group, Name: event.StreamName}
	recordID, err := identity.CloudWatchV1(identity.CloudWatchLocator{
		Account:   group.Account,
		Region:    group.Region,
		LogGroup:  group.LogGroup,
		LogStream: event.StreamName,
		EventID:   event.EventID,
	})
	if err != nil {
		// No identity means no effectively-once processing. There is no fallback
		// here: content hashing is prohibited, because two legitimate identical
		// messages do occur and would be merged into one occurrence.
		return model.NormalizedLog{}, fmt.Errorf("%w: %w", ErrUnusableRecord, err)
	}
	if event.IngestionTime.IsZero() {
		return model.NormalizedLog{}, fmt.Errorf(
			"%w: observed time is required and CloudWatch always reports an ingestion time", ErrUnusableRecord)
	}
	if strings.TrimSpace(event.Message) == "" {
		// A record needs a body or an event name, and this source has no event
		// name to fall back on. The mapper never invents either.
		return model.NormalizedLog{}, fmt.Errorf("%w: the event carries no message", ErrUnusableRecord)
	}

	rules := newRuleSet()
	withheld := newPathSet()
	observed := event.IngestionTime.UTC()
	eventTime, inferred, inferenceReason := normalize.EventTime(event.Timestamp.UTC(), observed)

	body := m.value(event.Message, "body", rules, withheld)
	if body.IsZero() {
		return model.NormalizedLog{}, fmt.Errorf("%w: the message redacted away to nothing", ErrUnusableRecord)
	}
	severityNumber, severityText, severityClass := severity(body)

	record := model.NormalizedLog{
		SchemaVersion:            model.NormalizedLogSchemaVersion,
		RecordID:                 recordID,
		RecordIDVersion:          model.RecordIDVersionCloudWatchV1,
		IdentityQuality:          model.IdentityQualityNative,
		BatchID:                  batchID,
		Source:                   m.Envelope(receivedAt),
		Region:                   group.Region,
		EventTime:                eventTime,
		ObservedTime:             observed,
		TimestampInferred:        inferred,
		TimestampInferenceReason: inferenceReason,
		SeverityNumber:           severityNumber,
		SeverityText:             severityText,
		SeverityClass:            severityClass,
		Body:                     body,
		Service: model.ServiceIdentity{
			Name:        m.service,
			Environment: m.environment,
			Status:      model.EnrichmentAvailable,
		},
		Deployment: model.DeploymentIdentity{ID: model.UnknownDeployment, Status: model.EnrichmentPending},
		Attributes: m.attributes(map[string]string{
			AttributeLogGroup:  group.LogGroup,
			AttributeLogStream: event.StreamName,
			AttributeEventID:   event.EventID,
		}, "attributes", withheld),
		ResourceAttributes: m.attributes(map[string]string{
			"cloud.provider":              "aws",
			"cloud.region":                group.Region,
			"cloud.account.id":            group.Account,
			"service.name":                m.service,
			"deployment.environment.name": m.environment,
		}, "resource_attributes", withheld),
	}
	record.RawReference = m.rawReference(stream, eventTime, observed, withheld)
	record.Redaction = model.RedactionMetadata{
		PolicyVersion:  m.policy.Version(),
		RuleIDs:        rules.sorted(),
		WithheldFields: withheld.sorted(),
	}

	// The last two gates are the same ones normalization applies, in the same
	// order, because a record leaving this package is about to be journalled.
	if err := m.policy.ValidateRecord(record); err != nil {
		return model.NormalizedLog{}, fmt.Errorf("%w: final prohibited-content validation failed: %w", ErrUnusableRecord, err)
	}
	if err := record.Validate(); err != nil {
		return model.NormalizedLog{}, fmt.Errorf("%w: %w", ErrUnusableRecord, err)
	}
	return record, nil
}

// severity reads a declared level out of the already-redacted body.
//
// Reading it from the safe value rather than from the raw message means the
// mapper never parses untrusted JSON itself: depth, node count, duplicate keys,
// and unsafe encodings were all bounded by the redaction policy first.
func severity(body model.SafeValue) (int32, string, model.SeverityClass) {
	if body.Kind != model.SafeKindMap {
		return 0, "", model.SeverityClassUnspecified
	}
	for _, key := range severityKeys {
		value, present := body.Map[key]
		if !present || value.Kind != model.SafeKindString {
			continue
		}
		mapped, known := severityTokens[strings.ToLower(strings.TrimSpace(value.String))]
		if !known {
			// A declared level this build does not recognise is still a
			// declaration, so no later key is consulted; guessing from another
			// field would make severity depend on key order.
			return 0, "", model.SeverityClassUnspecified
		}
		return mapped.number, mapped.text, mapped.class
	}
	return 0, "", model.SeverityClassUnspecified
}

// rawReference points at the raw stream, which never leaves its region.
//
// The locator is checked against the policy rather than rewritten: a redacted
// ARN would point at nothing, so an unsafe one is dropped and named in
// withheld_fields instead.
func (m *Mapper) rawReference(stream Stream, eventTime, observed time.Time, withheld *pathSet) *model.RegionalLogReference {
	locator := stream.Locator()
	if m.policy.ValidateText(locator) != nil {
		withheld.add("raw_reference.locator")
		return nil
	}
	from, to := eventTime, observed
	if to.Before(from) {
		from, to = to, from
	}
	return &model.RegionalLogReference{
		SourceType:     model.SourceTypeCloudWatch,
		Region:         stream.Group.Region,
		Locator:        locator,
		From:           from,
		To:             to,
		Classification: m.classification,
		ExpiresAt:      to.Add(m.rawReferenceTTL),
	}
}

func (m *Mapper) value(text, path string, rules *ruleSet, withheld *pathSet) model.SafeValue {
	result := m.policy.StructuredText(text)
	rules.add(result.RuleIDs)
	collectWithheld(result.Value, path, withheld)
	return result.Value
}

// attributes carries the native locator and the configured identity.
//
// These values are checked against the policy rather than rewritten by it. The
// policy's dynamic rules replace hex, UUIDs, and timestamps with placeholders
// so that two occurrences of one error fingerprint alike; a log stream really
// named ecs/paymentservice/0e1f2a3b would come back as
// ecs/paymentservice/<hex>, which points at no stream an operator can open.
// Being operator-chosen does not make them exempt from the prohibited-content
// scan, though: a stream named after a customer is withheld rather than stored.
func (m *Mapper) attributes(values map[string]string, root string, withheld *pathSet) map[string]model.SafeValue {
	converted := make(map[string]model.SafeValue, len(values))
	for _, key := range sortedStringKeys(values) {
		value := values[key]
		if value == "" {
			continue
		}
		if m.policy.ValidateFieldName(key) != nil {
			// Every key here is a constant this package chose, so an unsafe one
			// is a bug in this package rather than untrusted input.
			continue
		}
		if m.policy.ValidateText(value) != nil {
			withheld.add(root + "." + key)
			converted[key] = model.Withheld(redact.ReasonRedactionFailure)
			continue
		}
		converted[key] = model.SafeString(value)
	}
	if len(converted) == 0 {
		return nil
	}
	return converted
}

func validateEnvelopeContent(policy *redact.Policy, envelope model.TrustedEnvelope) error {
	texts := []string{
		string(envelope.SourceType), envelope.SourceAccount, envelope.Region,
		envelope.SourceInstance, envelope.CredentialIdentity,
	}
	texts = append(texts, envelope.AllowedEnvironments...)
	texts = append(texts, envelope.AllowedServices...)
	for _, text := range texts {
		if policy.ValidateText(text) != nil {
			return fmt.Errorf("%w: the trusted envelope contains prohibited content", ErrInvalidConfig)
		}
	}
	return nil
}

func validClassification(value string) bool {
	switch value {
	case "PUBLIC", "INTERNAL", "SENSITIVE", "RESTRICTED":
		return true
	default:
		return false
	}
}

func sortedStringKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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
