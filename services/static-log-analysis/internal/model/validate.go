package model

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Violation is one failed structural requirement, named by its field path.
type Violation struct {
	Field  string
	Reason string
}

func (v Violation) String() string { return v.Field + ": " + v.Reason }

// ValidationError reports every structural requirement a value failed, rather
// than only the first, so a test naming one field does not hide another.
type ValidationError struct {
	Violations []Violation
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Violations))
	for _, v := range e.Violations {
		parts = append(parts, v.String())
	}
	return fmt.Sprintf("%d structural violation(s): %s", len(e.Violations), strings.Join(parts, "; "))
}

// Has reports whether a specific field path failed.
func (e *ValidationError) Has(field string) bool {
	for _, v := range e.Violations {
		if v.Field == field {
			return true
		}
	}
	return false
}

// Fields returns the failed field paths in the order they were checked.
func (e *ValidationError) Fields() []string {
	fields := make([]string, 0, len(e.Violations))
	for _, v := range e.Violations {
		fields = append(fields, v.Field)
	}
	return fields
}

// violations accumulates field failures during a validation pass.
type violations struct {
	prefix string
	list   []Violation
}

func (c *violations) add(field, reason string) {
	c.list = append(c.list, Violation{Field: c.prefix + field, Reason: reason})
}

// requireText rejects empty and whitespace-only strings, which are the common
// way a required field arrives technically present but meaningless.
func (c *violations) requireText(field, value string) {
	if strings.TrimSpace(value) == "" {
		c.add(field, "must not be empty or whitespace-only")
	}
}

func (c *violations) requireUTC(field string, value time.Time) {
	if value.IsZero() {
		c.add(field, "must be set")
		return
	}
	if value.Location() != time.UTC {
		c.add(field, "must be UTC")
	}
}

func (c *violations) nested(field string) *violations {
	return &violations{prefix: c.prefix + field + "."}
}

func (c *violations) merge(child *violations) {
	c.list = append(c.list, child.list...)
}

func (c *violations) err() error {
	if len(c.list) == 0 {
		return nil
	}
	return &ValidationError{Violations: c.list}
}

// Validate reports every structural requirement the envelope fails.
//
// This is a structural check only. Reconciling a record's own claims against
// the envelope is an admission decision and lives in the admission path.
func (e TrustedEnvelope) Validate() error {
	c := &violations{}
	e.validate(c)
	return c.err()
}

func (e TrustedEnvelope) validate(c *violations) {
	switch e.SourceType {
	case SourceTypeOTLP, SourceTypeCloudWatch:
	case "":
		c.add("source_type", "must be set")
	default:
		c.add("source_type", "unknown source type")
	}
	c.requireText("source_account", e.SourceAccount)
	c.requireText("region", e.Region)
	c.requireText("source_instance", e.SourceInstance)
	c.requireText("credential_identity", e.CredentialIdentity)
	if len(e.AllowedEnvironments) == 0 {
		c.add("allowed_environments", "must list at least one environment")
	}
	for i, env := range e.AllowedEnvironments {
		c.requireText(fmt.Sprintf("allowed_environments[%d]", i), env)
	}
	if len(e.AllowedServices) == 0 {
		c.add("allowed_services", "must list at least one service identity")
	}
	for i, svc := range e.AllowedServices {
		c.requireText(fmt.Sprintf("allowed_services[%d]", i), svc)
	}
	c.requireUTC("received_at", e.ReceivedAt)
}

// Validate reports every structural requirement the record fails. A record that
// validates is well formed; whether it is admissible, correctly attributed, or
// safe to shed is decided elsewhere.
func (r NormalizedLog) Validate() error {
	c := &violations{}

	validateSchemaVersion(c, r.SchemaVersion)
	validateIdentity(c, r)
	c.requireText("batch_id", r.BatchID)

	source := c.nested("source")
	r.Source.validate(source)
	c.merge(source)

	c.requireText("region", r.Region)
	// A record's region must match the region of the source that authenticated
	// it. Disagreement means a regional boundary was crossed before this point.
	if r.Region != "" && r.Source.Region != "" && r.Region != r.Source.Region {
		c.add("region", "must equal source.region")
	}

	c.requireUTC("observed_time", r.ObservedTime)
	c.requireUTC("event_time", r.EventTime)
	if r.TimestampInferred {
		c.requireText("timestamp_inference_reason", r.TimestampInferenceReason)
	} else if strings.TrimSpace(r.TimestampInferenceReason) != "" {
		c.add("timestamp_inference_reason", "must be empty when timestamp_inferred is false")
	}

	switch r.SeverityClass {
	case SeverityClassUnspecified, SeverityClassTrace, SeverityClassDebug,
		SeverityClassInfo, SeverityClassWarn, SeverityClassError, SeverityClassFatal:
	case "":
		c.add("severity_class", "must be set")
	default:
		c.add("severity_class", "unknown severity class")
	}

	// Minimum ingestion identity requires a body or an event name.
	if r.Body.IsZero() && strings.TrimSpace(r.EventName) == "" {
		c.add("body", "a body or an event name is required")
	}
	if !r.Body.IsZero() {
		body := c.nested("body")
		r.Body.validate(body)
		c.merge(body)
	}

	validateAttributes(c, "attributes", r.Attributes)
	validateAttributes(c, "resource_attributes", r.ResourceAttributes)
	validateAttributes(c, "scope_attributes", r.ScopeAttributes)

	validateStatus(c, "service.status", r.Service.Status)
	validateStatus(c, "deployment.status", r.Deployment.Status)
	if r.Deployment.Status == EnrichmentAvailable {
		c.requireText("deployment.id", r.Deployment.ID)
	}

	validateTraceID(c, "correlation.trace_id", r.Correlation.TraceID, 32)
	validateTraceID(c, "correlation.span_id", r.Correlation.SpanID, 16)
	if r.Correlation.SpanID != "" && r.Correlation.TraceID == "" {
		c.add("correlation.trace_id", "must be set when a span id is present")
	}

	if r.Exception != nil {
		if strings.TrimSpace(r.Exception.Type) == "" && strings.TrimSpace(r.Exception.SafeMessage) == "" {
			c.add("exception", "requires an exception type or a safe message")
		}
		for i, frame := range r.Exception.StackFrames {
			if strings.TrimSpace(frame.Function) == "" && strings.TrimSpace(frame.Module) == "" {
				c.add(fmt.Sprintf("exception.stack_frames[%d]", i), "requires a function or a module")
			}
		}
	}

	c.requireText("redaction.policy_version", r.Redaction.PolicyVersion)

	if ref := r.RawReference; ref != nil {
		switch ref.SourceType {
		case SourceTypeOTLP, SourceTypeCloudWatch:
		case "":
			c.add("raw_reference.source_type", "must be set")
		default:
			c.add("raw_reference.source_type", "unknown source type")
		}
		c.requireText("raw_reference.region", ref.Region)
		c.requireText("raw_reference.locator", ref.Locator)
		switch ref.Classification {
		case "PUBLIC", "INTERNAL", "SENSITIVE", "RESTRICTED":
		case "":
			c.add("raw_reference.classification", "must be set")
		default:
			c.add("raw_reference.classification", "unknown classification")
		}
		c.requireUTC("raw_reference.from", ref.From)
		c.requireUTC("raw_reference.to", ref.To)
		c.requireUTC("raw_reference.expires_at", ref.ExpiresAt)
		if !ref.From.IsZero() && !ref.To.IsZero() && ref.To.Before(ref.From) {
			c.add("raw_reference.to", "must not be before raw_reference.from")
		}
		if !ref.To.IsZero() && !ref.ExpiresAt.IsZero() && !ref.ExpiresAt.After(ref.To) {
			c.add("raw_reference.expires_at", "must be after raw_reference.to")
		}
		if ref.Region != "" && r.Region != "" && ref.Region != r.Region {
			c.add("raw_reference.region", "must not point outside the record's region")
		}
	}

	return c.err()
}

// validateSchemaVersion rejects a version this build cannot read.
//
// Accepting an unknown major version would mean interpreting fields whose
// meaning or type may have changed, which is worse than refusing the record.
func validateSchemaVersion(c *violations, text string) {
	if strings.TrimSpace(text) == "" {
		c.add("schema_version", "must not be empty or whitespace-only")
		return
	}
	version, err := ParseSchemaVersion(text)
	if err != nil {
		c.add("schema_version", "must be canonical major.minor text")
		return
	}
	reader := SchemaVersion{Major: NormalizedLogSchemaMajor, Minor: NormalizedLogSchemaMinor}
	if !reader.CompatibleWith(version) {
		c.add("schema_version", fmt.Sprintf("major version is not readable by this build, which reads %d.x", reader.Major))
	}
}

// validateIdentity checks the record identity against the version that claims
// to have produced it.
//
// A version is immutable, so an identifier that does not have that version's
// shape was produced by something else, and treating it as that version would
// let a later algorithm silently rewrite history.
func validateIdentity(c *violations, r NormalizedLog) {
	switch r.IdentityQuality {
	case IdentityQualityNative, IdentityQualityDerived:
	case "":
		c.add("identity_quality", "must be set")
	default:
		c.add("identity_quality", "unknown identity quality")
	}

	if strings.TrimSpace(r.RecordIDVersion) == "" {
		c.add("record_id_version", "must not be empty or whitespace-only")
		c.requireText("record_id", r.RecordID)
		return
	}
	spec, known := recordIDVersions[r.RecordIDVersion]
	if !known {
		c.add("record_id_version", "unknown identity version; known versions are "+strings.Join(KnownRecordIDVersions(), ", "))
		c.requireText("record_id", r.RecordID)
		return
	}

	// The fallback identity is explicitly marked derived so that its use can be
	// measured and alerted. A record claiming a native version while declaring
	// derived quality, or the reverse, would escape that measurement.
	if r.IdentityQuality != "" && r.IdentityQuality != spec.quality {
		c.add("identity_quality", "must match the identity version")
	}

	validateHexDigest(c, "record_id", r.RecordID, spec.hexDigits)
}

func validateHexDigest(c *violations, field, value string, digits int) {
	if value == "" {
		c.add(field, "must not be empty or whitespace-only")
		return
	}
	if len(value) != digits {
		c.add(field, fmt.Sprintf("identity version produces %d lowercase hex characters, got %d", digits, len(value)))
		return
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			c.add(field, "identity version produces lowercase hex")
			return
		}
	}
}

func validateAttributes(c *violations, field string, attributes map[string]SafeValue) {
	// Sorted so that violation output does not depend on map iteration order.
	for index, key := range sortedKeys(attributes) {
		ordinalField := fmt.Sprintf("%s[%d]", field, index)
		if strings.TrimSpace(key) == "" {
			c.add(ordinalField, "attribute key must not be empty or whitespace-only")
			continue
		}
		child := c.nested(ordinalField)
		attributes[key].validate(child)
		c.merge(child)
	}
}

func sortedKeys(m map[string]SafeValue) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func validateStatus(c *violations, field string, status EnrichmentStatus) {
	switch status {
	case EnrichmentNotAvailable, EnrichmentNotApplicable, EnrichmentPending, EnrichmentAvailable:
	case "":
		c.add(field, "must be set")
	default:
		c.add(field, "unknown enrichment status")
	}
}

func validateTraceID(c *violations, field, value string, width int) {
	if value == "" {
		return
	}
	if len(value) != width {
		c.add(field, fmt.Sprintf("must be %d lowercase hex characters, got %d", width, len(value)))
		return
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			c.add(field, "must be lowercase hex")
			return
		}
	}
	if strings.Trim(value, "0") == "" {
		c.add(field, "must not be all zeroes")
	}
}

// Validate reports whether a value's kind and payload agree.
func (v SafeValue) Validate() error {
	c := &violations{}
	v.validate(c)
	return c.err()
}

func (v SafeValue) validate(c *violations) {
	v.validateDepth(c, 0)
}

func (v SafeValue) validateDepth(c *violations, depth int) {
	if depth > 64 {
		c.add("structure", "nesting exceeds validation limit")
		return
	}
	// A value's payload must match its kind exactly. A mismatch means the value
	// was built by literal rather than through a constructor, and some consumer
	// will read the wrong field.
	// Ordered so that violation output is identical on every run.
	var set []SafeValueKind
	if v.String != "" {
		set = append(set, SafeKindString)
	}
	if v.Int != 0 {
		set = append(set, SafeKindInt)
	}
	if v.Double != 0 {
		set = append(set, SafeKindDouble)
	}
	if v.Bool {
		set = append(set, SafeKindBool)
	}
	if v.Map != nil {
		set = append(set, SafeKindMap)
	}
	if v.Slice != nil {
		set = append(set, SafeKindSlice)
	}
	if v.Withheld != "" {
		set = append(set, SafeKindWithheld)
	}

	switch v.Kind {
	case SafeKindEmpty:
		if len(set) > 0 {
			c.add("kind", "must be set when a payload is present")
		}
		return
	case SafeKindString, SafeKindInt, SafeKindDouble, SafeKindBool, SafeKindMap, SafeKindSlice:
	case SafeKindWithheld:
		if strings.TrimSpace(v.Withheld) == "" {
			c.add("withheld", "must carry a reason")
		}
	default:
		c.add("kind", "unknown value kind")
		return
	}

	for _, kind := range set {
		if kind != v.Kind {
			c.add("kind", "kind and payload disagree")
		}
	}

	for index, key := range sortedKeys(v.Map) {
		ordinalField := fmt.Sprintf("map[%d]", index)
		if strings.TrimSpace(key) == "" {
			c.add(ordinalField, "key must not be empty or whitespace-only")
			continue
		}
		nested := c.nested(ordinalField)
		v.Map[key].validateDepth(nested, depth+1)
		c.merge(nested)
	}
	for i, child := range v.Slice {
		nested := c.nested(fmt.Sprintf("slice[%d]", i))
		child.validateDepth(nested, depth+1)
		c.merge(nested)
	}
}
