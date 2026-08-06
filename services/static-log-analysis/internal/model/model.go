// Package model holds domain types that carry no storage or transport
// dependencies. Types here are the internal canonical form; OTLP Protobuf types
// stay at the transport edge and never reach rule or persistence code.
package model

import "time"

// NormalizedLogSchemaVersion is the major.minor version of NormalizedLog.
// Readers accept unknown fields within the same major version; writers never
// change the meaning or type of an existing field.
const NormalizedLogSchemaVersion = "1.0"

// SourceType names the class of system a batch was admitted from.
type SourceType string

const (
	SourceTypeOTLP       SourceType = "otlp"
	SourceTypeCloudWatch SourceType = "cloudwatch"
)

// IdentityQuality records how a record_id was established. Derived identity is
// the measured and alerted fallback path, never the normal OTLP path.
type IdentityQuality string

const (
	// IdentityQualityNative means the identity came from a trusted native or
	// producer-assigned record ID.
	IdentityQualityNative IdentityQuality = "native"
	// IdentityQualityDerived means the identity was reconstructed from stable
	// record content because no trusted ID was available.
	IdentityQualityDerived IdentityQuality = "derived"
)

// SeverityClass is the coarse severity bucket rules match against.
type SeverityClass string

const (
	SeverityClassUnspecified SeverityClass = "unspecified"
	SeverityClassTrace       SeverityClass = "trace"
	SeverityClassDebug       SeverityClass = "debug"
	SeverityClassInfo        SeverityClass = "info"
	SeverityClassWarn        SeverityClass = "warn"
	SeverityClassError       SeverityClass = "error"
	SeverityClassFatal       SeverityClass = "fatal"
)

// EnrichmentStatus distinguishes the reasons an optional structure is absent so
// that absence is never ambiguous.
type EnrichmentStatus string

const (
	EnrichmentNotAvailable  EnrichmentStatus = "not_available"
	EnrichmentNotApplicable EnrichmentStatus = "not_applicable"
	EnrichmentPending       EnrichmentStatus = "pending"
	EnrichmentAvailable     EnrichmentStatus = "available"
)

// UnknownDeployment is the grouping key for records whose deployment identity is
// not yet known. It may be replaced by enrichment later.
const UnknownDeployment = "unknown-deployment"

// TrustedEnvelope is the authenticated description of where a batch came from.
// It is established outside application-controlled log attributes: for OTLP by
// the Collector's workload identity or mutual TLS, for CloudWatch by the
// adapter's regional AWS credentials and configured account and log groups.
//
// Values a record claims about itself are reconciled against this envelope
// during admission; the envelope always wins.
type TrustedEnvelope struct {
	SourceType SourceType `json:"source_type"`
	// SourceAccount is the cloud account or tenant the source authenticated as.
	SourceAccount string `json:"source_account"`
	Region        string `json:"region"`
	// AllowedEnvironments is the set of environments this source may claim. A
	// source dedicated to exactly one environment may supply a default.
	AllowedEnvironments []string `json:"allowed_environments"`
	// AllowedServices is the set of service identities this source may claim.
	AllowedServices []string `json:"allowed_services"`
	// SourceInstance identifies the Collector or adapter instance. It pairs with
	// a record UID to make identity unforgeable across sources.
	SourceInstance string `json:"source_instance"`
	// CredentialIdentity is the workload or credential principal that
	// authenticated, retained for audit.
	CredentialIdentity string    `json:"credential_identity"`
	ReceivedAt         time.Time `json:"received_at"`
}

// AllowsEnvironment reports whether the envelope permits an environment claim.
func (e TrustedEnvelope) AllowsEnvironment(environment string) bool {
	return contains(e.AllowedEnvironments, environment)
}

// AllowsService reports whether the envelope permits a service identity claim.
func (e TrustedEnvelope) AllowsService(service string) bool {
	return contains(e.AllowedServices, service)
}

// DefaultEnvironment returns the envelope's environment when the source is
// dedicated to exactly one, and false otherwise. Only a dedicated source may
// supply a default for a missing claim.
func (e TrustedEnvelope) DefaultEnvironment() (string, bool) {
	if len(e.AllowedEnvironments) != 1 {
		return "", false
	}
	return e.AllowedEnvironments[0], true
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// ServiceIdentity is the service a record is attributed to. Missing service
// identity prevents service-specific agent execution but does not prevent
// ingestion.
type ServiceIdentity struct {
	Name        string           `json:"name,omitempty"`
	Namespace   string           `json:"namespace,omitempty"`
	InstanceID  string           `json:"instance_id,omitempty"`
	Environment string           `json:"environment,omitempty"`
	Status      EnrichmentStatus `json:"status"`
}

// DeploymentIdentity is the release a record belongs to. A deployment change
// always creates a linked incident generation, so this value is part of
// grouping rather than decoration.
type DeploymentIdentity struct {
	ID      string           `json:"id,omitempty"`
	Version string           `json:"version,omitempty"`
	Status  EnrichmentStatus `json:"status"`
}

// CorrelationIdentity carries trace correlation as lowercase hex, empty when the
// record was not part of a trace.
type CorrelationIdentity struct {
	TraceID string `json:"trace_id,omitempty"`
	SpanID  string `json:"span_id,omitempty"`
}

// StackFrame is one normalized stack frame. InApplication distinguishes frames
// that belong to the service being analyzed from library-only frames, which are
// excluded from fingerprints.
type StackFrame struct {
	Function      string `json:"function,omitempty"`
	Module        string `json:"module,omitempty"`
	File          string `json:"file,omitempty"`
	InApplication bool   `json:"in_application"`
}

// NormalizedException is the redacted exception attached to a record. The
// message is already safe; raw exception text never reaches this type.
type NormalizedException struct {
	Type        string       `json:"type,omitempty"`
	SafeMessage string       `json:"safe_message,omitempty"`
	StackFrames []StackFrame `json:"stack_frames,omitempty"`
}

// RedactionMetadata records which policy produced the safe content in a record,
// so that stored evidence can be re-evaluated when a policy changes.
type RedactionMetadata struct {
	PolicyVersion string `json:"policy_version"`
	// RuleIDs are the redaction rules that matched, for regression analysis.
	RuleIDs []string `json:"rule_ids,omitempty"`
	// WithheldFields names fields replaced by withheld metadata because they
	// could not be safely parsed.
	WithheldFields []string `json:"withheld_fields,omitempty"`
}

// RegionalLogReference points at raw log content that stays in its own region.
// It never contains source credentials.
type RegionalLogReference struct {
	SourceType     SourceType `json:"source_type"`
	Region         string     `json:"region"`
	Locator        string     `json:"locator"`
	From           time.Time  `json:"from"`
	To             time.Time  `json:"to"`
	Classification string     `json:"classification"`
	ExpiresAt      time.Time  `json:"expires_at"`
}

// NormalizedLog is the canonical internal record. Every field is already safe:
// redaction runs before normalization completes, so no unredacted value is
// representable here.
type NormalizedLog struct {
	SchemaVersion string `json:"schema_version"`

	// RecordID is the global effectively-once key. RecordIDVersion names the
	// immutable algorithm that produced it so a later version never silently
	// rewrites history.
	RecordID        string          `json:"record_id"`
	RecordIDVersion string          `json:"record_id_version"`
	IdentityQuality IdentityQuality `json:"identity_quality"`

	// BatchID identifies a transport attempt, not a domain occurrence. It never
	// replaces per-record deduplication.
	BatchID string `json:"batch_id"`

	Source TrustedEnvelope `json:"source"`
	Region string          `json:"region"`

	EventTime    time.Time `json:"event_time"`
	ObservedTime time.Time `json:"observed_time"`
	// TimestampInferred is set when EventTime was absent or outside its valid
	// bound relative to ObservedTime, in which case ObservedTime is used for
	// processing and TimestampInferenceReason explains why.
	TimestampInferred        bool   `json:"timestamp_inferred"`
	TimestampInferenceReason string `json:"timestamp_inference_reason,omitempty"`

	SeverityNumber int32         `json:"severity_number"`
	SeverityText   string        `json:"severity_text,omitempty"`
	SeverityClass  SeverityClass `json:"severity_class"`

	// Body or EventName must be present; the normalizer never invents either.
	Body      SafeValue `json:"body,omitempty"`
	EventName string    `json:"event_name,omitempty"`

	Attributes         map[string]SafeValue `json:"attributes,omitempty"`
	ResourceAttributes map[string]SafeValue `json:"resource_attributes,omitempty"`
	ScopeAttributes    map[string]SafeValue `json:"scope_attributes,omitempty"`

	Service     ServiceIdentity      `json:"service"`
	Deployment  DeploymentIdentity   `json:"deployment"`
	Correlation CorrelationIdentity  `json:"correlation"`
	Exception   *NormalizedException `json:"exception,omitempty"`

	Redaction    RedactionMetadata     `json:"redaction"`
	RawReference *RegionalLogReference `json:"raw_reference,omitempty"`
}
