// Package builders constructs normalized records and trusted envelopes that are
// valid by default.
//
// A test states only the part of a record its scenario is about; everything
// else is filled in with values that pass model validation. Every record is
// validated as it is built, so a test can never quietly assert on a record that
// production would have rejected as malformed.
//
// A builder produces records that are already normalized. It is the input to
// rule, incident, journal, and persistence tests. Tests for admission,
// normalization, and identity derivation start from OTLP instead, which the
// otlpgen package produces.
package builders

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/clock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/ids"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/tb"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/testids"
)

// Defaults describe the scenario a builder produces when a test says nothing.
// They match the payment failure path the vertical slice is built around.
const (
	DefaultRegion             = "us-east-1"
	DefaultEnvironment        = "production"
	DefaultService            = "paymentservice"
	DefaultSourceAccount      = "000000000001"
	DefaultSourceInstance     = "collector-us-east-1-0"
	DefaultCredentialIdentity = "spiffe://example.internal/ns/observability/sa/collector"
	DefaultDeploymentID       = "paymentservice-7f4c9a1"
	DefaultDeploymentVersion  = "2026.3.4"
	DefaultRedactionPolicy    = "2.2"

	// DefaultEventLag is how far a record's event time sits before its observed
	// time: recent enough to be used as-is rather than inferred.
	DefaultEventLag = 2 * time.Second
)

// Factory produces envelopes and records from one deterministic clock and one
// deterministic identifier source.
//
// Each test creates its own factory. There is no package-level default, because
// shared generator state between tests is how a suite becomes order dependent.
type Factory struct {
	clock    clock.Clock
	ids      ids.Source
	envelope model.TrustedEnvelope
}

// FactoryOption configures a Factory.
type FactoryOption func(*Factory)

// WithClock supplies the clock observed times are read from, normally the same
// fake clock the scenario advances.
func WithClock(c clock.Clock) FactoryOption { return func(f *Factory) { f.clock = c } }

// WithIDs supplies the identifier source.
func WithIDs(source ids.Source) FactoryOption { return func(f *Factory) { f.ids = source } }

// WithEnvelope replaces the trusted envelope every record is attributed to.
func WithEnvelope(envelope model.TrustedEnvelope) FactoryOption {
	return func(f *Factory) { f.envelope = envelope }
}

// NewFactory returns a factory. Without options it uses a fake clock at
// fakeclock.Origin, a deterministic identifier source, and the default
// single-service production envelope.
func NewFactory(opts ...FactoryOption) *Factory {
	fake := fakeclock.NewAtOrigin()
	f := &Factory{clock: fake, ids: testids.New(testids.WithClock(fake))}
	for _, opt := range opts {
		opt(f)
	}
	if f.envelope.SourceType == "" {
		f.envelope = f.defaultEnvelope()
	}
	return f
}

func (f *Factory) defaultEnvelope() model.TrustedEnvelope {
	return model.TrustedEnvelope{
		SourceType:          model.SourceTypeOTLP,
		SourceAccount:       DefaultSourceAccount,
		Region:              DefaultRegion,
		AllowedEnvironments: []string{DefaultEnvironment},
		AllowedServices:     []string{DefaultService},
		SourceInstance:      DefaultSourceInstance,
		CredentialIdentity:  DefaultCredentialIdentity,
		ReceivedAt:          f.clock.Now(),
	}
}

// Clock returns the clock the factory reads observed times from.
func (f *Factory) Clock() clock.Clock { return f.clock }

// Envelope returns the trusted envelope records are attributed to, received at
// the current time.
func (f *Factory) Envelope(t tb.TB, opts ...EnvelopeOption) model.TrustedEnvelope {
	t.Helper()
	envelope := f.envelope
	envelope.AllowedEnvironments = append([]string(nil), envelope.AllowedEnvironments...)
	envelope.AllowedServices = append([]string(nil), envelope.AllowedServices...)
	envelope.ReceivedAt = f.clock.Now()
	for _, opt := range opts {
		opt(&envelope)
	}
	if err := envelope.Validate(); err != nil {
		t.Fatalf("builders: envelope is invalid: %v", err)
	}
	return envelope
}

// EnvelopeOption adjusts a trusted envelope.
type EnvelopeOption func(*model.TrustedEnvelope)

// EnvelopeInRegion sets the region the source authenticated for.
func EnvelopeInRegion(region string) EnvelopeOption {
	return func(e *model.TrustedEnvelope) { e.Region = region }
}

// EnvelopeFromSource sets the source type and instance.
func EnvelopeFromSource(sourceType model.SourceType, instance string) EnvelopeOption {
	return func(e *model.TrustedEnvelope) {
		e.SourceType = sourceType
		e.SourceInstance = instance
	}
}

// EnvelopeAllowingServices replaces the set of service identities the source may
// claim.
func EnvelopeAllowingServices(services ...string) EnvelopeOption {
	return func(e *model.TrustedEnvelope) { e.AllowedServices = services }
}

// EnvelopeAllowingEnvironments replaces the set of environments the source may
// claim. A source with exactly one may supply it as a default.
func EnvelopeAllowingEnvironments(environments ...string) EnvelopeOption {
	return func(e *model.TrustedEnvelope) { e.AllowedEnvironments = environments }
}

// EnvelopeReceivedAt sets the reception time.
func EnvelopeReceivedAt(at time.Time) EnvelopeOption {
	return func(e *model.TrustedEnvelope) { e.ReceivedAt = at }
}

// Record returns a valid normalized record.
//
// The record is validated before it is returned, and an invalid one fails the
// test unless AllowInvalid was passed. Building a deliberately malformed record
// is supported, but never by accident.
func (f *Factory) Record(t tb.TB, opts ...RecordOption) model.NormalizedLog {
	t.Helper()

	uid := f.newID(t)
	observed := f.clock.Now()
	record := model.NormalizedLog{
		SchemaVersion:   model.NormalizedLogSchemaVersion,
		RecordID:        syntheticRecordID(uid),
		RecordIDVersion: model.RecordIDVersionOTLPV1,
		IdentityQuality: model.IdentityQualityNative,
		BatchID:         f.newID(t),
		Source:          f.Envelope(t),
		Region:          f.envelope.Region,
		EventTime:       observed.Add(-DefaultEventLag),
		ObservedTime:    observed,
		SeverityNumber:  17,
		SeverityText:    "ERROR",
		SeverityClass:   model.SeverityClassError,
		Body:            model.SafeString("charge failed for order"),
		Attributes: map[string]model.SafeValue{
			"payment.provider": model.SafeString("acme"),
			"http.route":       model.SafeString("/api/payments/{id}"),
		},
		ResourceAttributes: map[string]model.SafeValue{
			"service.name":                model.SafeString(DefaultService),
			"deployment.environment.name": model.SafeString(DefaultEnvironment),
			"cloud.region":                model.SafeString(f.envelope.Region),
		},
		Service: model.ServiceIdentity{
			Name:        DefaultService,
			Environment: DefaultEnvironment,
			InstanceID:  DefaultService + "-0",
			Status:      model.EnrichmentAvailable,
		},
		Deployment: model.DeploymentIdentity{
			ID:      DefaultDeploymentID,
			Version: DefaultDeploymentVersion,
			Status:  model.EnrichmentAvailable,
		},
		Redaction: model.RedactionMetadata{PolicyVersion: DefaultRedactionPolicy},
	}

	settings := &recordSettings{}
	for _, opt := range opts {
		opt(&record, settings)
	}

	if err := record.Validate(); err != nil {
		if !settings.allowInvalid {
			t.Fatalf("builders: record is invalid: %v\n"+
				"If the scenario needs a malformed record, pass builders.AllowInvalid().", err)
		}
	} else if settings.allowInvalid {
		// A scenario that says it wants a malformed record and gets a valid one
		// is testing something other than what it claims.
		t.Fatalf("builders: AllowInvalid() was requested but the record is valid")
	}
	return record
}

// Batch returns count records sharing one batch identifier, as one transport
// attempt delivers them.
//
// Every record has its own identity, and all of them carry the clock's current
// time: a batch does not move the scenario clock. A test that needs records
// spread over time advances the clock between calls.
func (f *Factory) Batch(t tb.TB, count int, opts ...RecordOption) []model.NormalizedLog {
	t.Helper()
	if count < 1 {
		t.Fatalf("builders: batch size must be at least 1, got %d", count)
	}
	batchID := f.newID(t)
	records := make([]model.NormalizedLog, 0, count)
	for i := 0; i < count; i++ {
		record := f.Record(t, append([]RecordOption{WithBatchID(batchID)}, opts...)...)
		records = append(records, record)
	}
	return records
}

func (f *Factory) newID(t tb.TB) string {
	t.Helper()
	id, err := f.ids.New()
	if err != nil {
		t.Fatalf("builders: generating identifier: %v", err)
	}
	return id
}

// syntheticRecordID returns an opaque 64-character identifier shaped like the
// SHA-256 record identity production derives.
//
// It is deliberately not the real derivation: identity derivation is a
// behaviour with its own tests, and a builder that reimplemented it would make
// those tests agree with a copy of themselves.
func syntheticRecordID(uid string) string {
	sum := sha256.Sum256([]byte("builders/synthetic-record-id:" + uid))
	return hex.EncodeToString(sum[:])
}

// recordSettings holds builder behaviour that is not part of the record.
type recordSettings struct {
	allowInvalid bool
}

// RecordOption adjusts a record under construction.
type RecordOption func(*model.NormalizedLog, *recordSettings)

// AllowInvalid permits a record that fails validation, for scenarios about
// malformed input. Without it, an invalid record fails the test.
func AllowInvalid() RecordOption {
	return func(_ *model.NormalizedLog, s *recordSettings) { s.allowInvalid = true }
}

// WithRecordID sets the record identity.
func WithRecordID(recordID string) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) { r.RecordID = recordID }
}

// WithIdentity sets the record identity together with the version and quality
// that produced it.
func WithIdentity(recordID, version string, quality model.IdentityQuality) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) {
		r.RecordID = recordID
		r.RecordIDVersion = version
		r.IdentityQuality = quality
	}
}

// DerivedIdentity marks the record as having used the derived identity
// fallback, setting the version and the quality together because a record that
// declares one without the other escapes the measurement the fallback exists to
// be visible to.
func DerivedIdentity() RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) {
		r.RecordIDVersion = model.RecordIDVersionDerivedV1
		r.IdentityQuality = model.IdentityQualityDerived
	}
}

// WithBatchID sets the transport batch identifier.
func WithBatchID(batchID string) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) { r.BatchID = batchID }
}

// WithSourceEnvelope attributes the record to another trusted envelope, and
// moves its region to match.
func WithSourceEnvelope(envelope model.TrustedEnvelope) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) {
		r.Source = envelope
		r.Region = envelope.Region
	}
}

// WithRegion sets the record's region without changing its envelope, which is
// how a regional boundary violation is constructed.
func WithRegion(region string) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) { r.Region = region }
}

// WithService sets the claimed service name.
func WithService(name string) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) {
		r.Service.Name = name
		r.ResourceAttributes["service.name"] = model.SafeString(name)
	}
}

// WithEnvironment sets the claimed environment.
func WithEnvironment(environment string) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) {
		r.Service.Environment = environment
		r.ResourceAttributes["deployment.environment.name"] = model.SafeString(environment)
	}
}

// WithoutEnvironment removes the environment claim and marks service identity
// incomplete. Such a record remains valid evidence but cannot drive a
// service-specific investigation.
func WithoutEnvironment() RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) {
		r.Service.Environment = ""
		r.Service.Status = model.EnrichmentNotAvailable
		delete(r.ResourceAttributes, "deployment.environment.name")
	}
}

// WithoutService removes service identity, which prevents service-specific
// agent execution while still allowing ingestion.
func WithoutService() RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) {
		r.Service = model.ServiceIdentity{Status: model.EnrichmentNotAvailable}
		delete(r.ResourceAttributes, "service.name")
	}
}

// WithSeverity sets the severity number, text, and class together, because a
// record whose severity fields disagree is not a scenario any rule describes.
func WithSeverity(number int32, text string, class model.SeverityClass) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) {
		r.SeverityNumber = number
		r.SeverityText = text
		r.SeverityClass = class
	}
}

// Severity presets for the classes rules match on. The numbers are the OTLP
// severity numbers for each class.
func SeverityDebug() RecordOption { return WithSeverity(5, "DEBUG", model.SeverityClassDebug) }
func SeverityInfo() RecordOption  { return WithSeverity(9, "INFO", model.SeverityClassInfo) }
func SeverityWarn() RecordOption  { return WithSeverity(13, "WARN", model.SeverityClassWarn) }
func SeverityError() RecordOption { return WithSeverity(17, "ERROR", model.SeverityClassError) }
func SeverityFatal() RecordOption { return WithSeverity(21, "FATAL", model.SeverityClassFatal) }

// WithBody sets the safe body text.
func WithBody(body string) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) { r.Body = model.SafeString(body) }
}

// WithBodyValue sets a structured safe body.
func WithBodyValue(body model.SafeValue) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) { r.Body = body }
}

// WithEventName replaces the body with an event name.
func WithEventName(name string) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) {
		r.Body = model.SafeValue{}
		r.EventName = name
	}
}

// WithObservedTime sets when the service received the record, keeping the
// default lag between event and observed time.
func WithObservedTime(at time.Time) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) {
		r.ObservedTime = at
		r.EventTime = at.Add(-DefaultEventLag)
	}
}

// WithEventTime sets when the event happened, leaving observed time alone. This
// is how late and out-of-order arrivals are built.
func WithEventTime(at time.Time) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) { r.EventTime = at }
}

// WithInferredTimestamp marks the event time as inferred from observed time and
// records why.
func WithInferredTimestamp(reason string) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) {
		r.EventTime = r.ObservedTime
		r.TimestampInferred = true
		r.TimestampInferenceReason = reason
	}
}

// WithAttribute sets one record attribute.
func WithAttribute(key string, value model.SafeValue) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) {
		if r.Attributes == nil {
			r.Attributes = map[string]model.SafeValue{}
		}
		r.Attributes[key] = value
	}
}

// WithoutAttribute removes a record attribute.
func WithoutAttribute(key string) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) { delete(r.Attributes, key) }
}

// WithResourceAttribute sets one resource attribute.
func WithResourceAttribute(key string, value model.SafeValue) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) {
		if r.ResourceAttributes == nil {
			r.ResourceAttributes = map[string]model.SafeValue{}
		}
		r.ResourceAttributes[key] = value
	}
}

// WithException attaches a normalized exception.
func WithException(exceptionType, safeMessage string, frames ...model.StackFrame) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) {
		r.Exception = &model.NormalizedException{
			Type:        exceptionType,
			SafeMessage: safeMessage,
			StackFrames: frames,
		}
	}
}

// AppFrame returns an in-application stack frame, the kind fingerprints keep.
func AppFrame(function, module string) model.StackFrame {
	return model.StackFrame{Function: function, Module: module, InApplication: true}
}

// LibraryFrame returns a library stack frame, the kind fingerprints exclude.
func LibraryFrame(function, module string) model.StackFrame {
	return model.StackFrame{Function: function, Module: module}
}

// WithCorrelation attaches trace correlation.
func WithCorrelation(traceID, spanID string) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) {
		r.Correlation = model.CorrelationIdentity{TraceID: traceID, SpanID: spanID}
	}
}

// WithDeployment sets the deployment a record belongs to. A change of
// deployment always starts a linked incident generation.
func WithDeployment(id, version string) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) {
		r.Deployment = model.DeploymentIdentity{ID: id, Version: version, Status: model.EnrichmentAvailable}
	}
}

// WithUnknownDeployment groups the record under the unknown deployment, which
// enrichment may resolve later.
func WithUnknownDeployment() RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) {
		r.Deployment = model.DeploymentIdentity{ID: model.UnknownDeployment, Status: model.EnrichmentPending}
	}
}

// WithRedactionPolicy names the redaction policy that produced the safe content.
func WithRedactionPolicy(version string) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) { r.Redaction.PolicyVersion = version }
}

// WithRawReference attaches a reference to raw content that stayed in region.
func WithRawReference(locator string, expires time.Time) RecordOption {
	return func(r *model.NormalizedLog, _ *recordSettings) {
		r.RawReference = &model.RegionalLogReference{
			SourceType:     r.Source.SourceType,
			Region:         r.Region,
			Locator:        locator,
			From:           r.EventTime,
			To:             r.ObservedTime,
			Classification: "RESTRICTED",
			ExpiresAt:      expires,
		}
	}
}
