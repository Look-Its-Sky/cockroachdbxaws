package model_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

// validRecord is written out by hand rather than produced by the record
// builders, because the builders rely on Validate to prove what they emit. A
// builder-produced fixture here would make this test agree with itself.
func validRecord() model.NormalizedLog {
	received := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)
	envelope := model.TrustedEnvelope{
		SourceType:          model.SourceTypeOTLP,
		SourceAccount:       "000000000001",
		Region:              "us-east-1",
		AllowedEnvironments: []string{"production"},
		AllowedServices:     []string{"paymentservice"},
		SourceInstance:      "collector-use1-0",
		CredentialIdentity:  "spiffe://example/collector",
		ReceivedAt:          received,
	}
	return model.NormalizedLog{
		SchemaVersion: model.NormalizedLogSchemaVersion,
		// A SHA-256 digest, which is the shape every identity version produces.
		RecordID:        "6c2f5f1b7b1e4b2f9d1a2c3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a",
		RecordIDVersion: model.RecordIDVersionOTLPV1,
		IdentityQuality: model.IdentityQualityNative,
		BatchID:         "0194f0a0-0000-7000-8000-000000000001",
		Source:          envelope,
		Region:          "us-east-1",
		EventTime:       received.Add(-2 * time.Second),
		ObservedTime:    received,
		SeverityNumber:  17,
		SeverityText:    "ERROR",
		SeverityClass:   model.SeverityClassError,
		Body:            model.SafeString("charge declined for order"),
		Attributes:      map[string]model.SafeValue{"payment.provider": model.SafeString("acme")},
		Service: model.ServiceIdentity{
			Name:        "paymentservice",
			Environment: "production",
			Status:      model.EnrichmentAvailable,
		},
		Deployment: model.DeploymentIdentity{
			ID:     "paymentservice-7f4c",
			Status: model.EnrichmentAvailable,
		},
		Redaction: model.RedactionMetadata{PolicyVersion: "1.0"},
	}
}

func TestValidateAcceptsAWellFormedRecord(t *testing.T) {
	if err := validRecord().Validate(); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}
}

func TestValidateReportsStructuralViolations(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*model.NormalizedLog)
		field   string
		because string
	}{
		{
			name:    "blank record id",
			mutate:  func(r *model.NormalizedLog) { r.RecordID = "" },
			field:   "record_id",
			because: "record_id is the effectively-once key",
		},
		{
			name:    "whitespace-only record id version",
			mutate:  func(r *model.NormalizedLog) { r.RecordIDVersion = "   " },
			field:   "record_id_version",
			because: "a required string must not be satisfied by whitespace",
		},
		{
			name:    "unknown record id version",
			mutate:  func(r *model.NormalizedLog) { r.RecordIDVersion = "otlp:v9" },
			field:   "record_id_version",
			because: "an unrecognized algorithm cannot be checked or reproduced",
		},
		{
			name:    "record id of the wrong length for its version",
			mutate:  func(r *model.NormalizedLog) { r.RecordID = "6c2f5f1b" },
			field:   "record_id",
			because: "an identity version names a fixed digest shape",
		},
		{
			name: "record id in uppercase hex",
			mutate: func(r *model.NormalizedLog) {
				r.RecordID = strings.ToUpper(r.RecordID)
			},
			field:   "record_id",
			because: "the same identity must not be representable two ways",
		},
		{
			name:    "unknown identity quality",
			mutate:  func(r *model.NormalizedLog) { r.IdentityQuality = "guessed" },
			field:   "identity_quality",
			because: "derived identity is measured and alerted, so its values are closed",
		},
		{
			name:    "native version declaring derived quality",
			mutate:  func(r *model.NormalizedLog) { r.IdentityQuality = model.IdentityQualityDerived },
			field:   "identity_quality",
			because: "fallback identity use is measured, so it cannot be claimed by a native version",
		},
		{
			name: "derived version declaring native quality",
			mutate: func(r *model.NormalizedLog) {
				r.RecordIDVersion = model.RecordIDVersionDerivedV1
			},
			field:   "identity_quality",
			because: "the fallback path must be visible in the record that used it",
		},
		{
			name:    "region disagrees with authenticated source",
			mutate:  func(r *model.NormalizedLog) { r.Region = "eu-west-1" },
			field:   "region",
			because: "a record may not claim a region its source did not authenticate for",
		},
		{
			name: "event time not UTC",
			mutate: func(r *model.NormalizedLog) {
				r.EventTime = r.EventTime.In(time.FixedZone("CET", 3600))
			},
			field:   "event_time",
			because: "duration arithmetic never uses local timezone rules",
		},
		{
			name:    "inferred timestamp without a reason",
			mutate:  func(r *model.NormalizedLog) { r.TimestampInferred = true },
			field:   "timestamp_inference_reason",
			because: "an inferred timestamp must record why it was inferred",
		},
		{
			name: "inference reason without inference",
			mutate: func(r *model.NormalizedLog) {
				r.TimestampInferenceReason = "event_time missing"
			},
			field:   "timestamp_inference_reason",
			because: "a reason without inference means the two fields disagree",
		},
		{
			name:    "unknown severity class",
			mutate:  func(r *model.NormalizedLog) { r.SeverityClass = "critical" },
			field:   "severity_class",
			because: "rules match on a closed set of severity classes",
		},
		{
			name: "neither body nor event name",
			mutate: func(r *model.NormalizedLog) {
				r.Body = model.SafeValue{}
				r.EventName = ""
			},
			field:   "body",
			because: "minimum ingestion identity requires a body or an event name",
		},
		{
			name:    "blank redaction policy version",
			mutate:  func(r *model.NormalizedLog) { r.Redaction.PolicyVersion = "" },
			field:   "redaction.policy_version",
			because: "stored evidence must name the policy that made it safe",
		},
		{
			name:    "blank source instance",
			mutate:  func(r *model.NormalizedLog) { r.Source.SourceInstance = "" },
			field:   "source.source_instance",
			because: "record identity is only unforgeable when paired with the source instance",
		},
		{
			name:    "no allowed services",
			mutate:  func(r *model.NormalizedLog) { r.Source.AllowedServices = nil },
			field:   "source.allowed_services",
			because: "an envelope that allows nothing cannot authorize a service claim",
		},
		{
			name:    "span without trace",
			mutate:  func(r *model.NormalizedLog) { r.Correlation.SpanID = "00f067aa0ba902b7" },
			field:   "correlation.trace_id",
			because: "a span id is meaningless without its trace",
		},
		{
			name: "uppercase trace id",
			mutate: func(r *model.NormalizedLog) {
				r.Correlation.TraceID = "4BF92F3577B34DA6A3CE929D0E0E4736"
				r.Correlation.SpanID = "00f067aa0ba902b7"
			},
			field:   "correlation.trace_id",
			because: "correlation ids are compared as lowercase hex",
		},
		{
			name: "all-zero trace id",
			mutate: func(r *model.NormalizedLog) {
				r.Correlation.TraceID = strings.Repeat("0", 32)
			},
			field:   "correlation.trace_id",
			because: "an all-zero trace id is the absence of a trace, not a trace",
		},
		{
			name:    "missing service enrichment status",
			mutate:  func(r *model.NormalizedLog) { r.Service.Status = "" },
			field:   "service.status",
			because: "absence must never be ambiguous",
		},
		{
			name: "available deployment without an id",
			mutate: func(r *model.NormalizedLog) {
				r.Deployment.ID = ""
			},
			field:   "deployment.id",
			because: "deployment identity drives generation linking",
		},
		{
			name: "blank attribute key",
			mutate: func(r *model.NormalizedLog) {
				r.Attributes[" "] = model.SafeString("x")
			},
			field:   "attributes[0]",
			because: "an unnamed attribute cannot be matched or redacted by name",
		},
		{
			name: "exception with neither type nor message",
			mutate: func(r *model.NormalizedLog) {
				r.Exception = &model.NormalizedException{}
			},
			field:   "exception",
			because: "an exception with no content contributes nothing to a fingerprint",
		},
		{
			name: "raw reference outside the record region",
			mutate: func(r *model.NormalizedLog) {
				r.RawReference = &model.RegionalLogReference{
					SourceType:     model.SourceTypeOTLP,
					Region:         "eu-west-1",
					Locator:        "s3://bucket/key",
					Classification: "restricted",
					ExpiresAt:      time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
				}
			},
			field:   "raw_reference.region",
			because: "raw content never leaves its region",
		},
		{
			name: "raw reference without from",
			mutate: func(r *model.NormalizedLog) {
				r.RawReference = validRawReference(r.Region)
				r.RawReference.From = time.Time{}
			},
			field:   "raw_reference.from",
			because: "a raw evidence range must be complete",
		},
		{
			name: "raw reference without source type",
			mutate: func(r *model.NormalizedLog) {
				r.RawReference = validRawReference(r.Region)
				r.RawReference.SourceType = ""
			},
			field:   "raw_reference.source_type",
			because: "raw evidence routing must use a supported source",
		},
		{
			name: "raw reference with unknown source type",
			mutate: func(r *model.NormalizedLog) {
				r.RawReference = validRawReference(r.Region)
				r.RawReference.SourceType = model.SourceType("future-source")
			},
			field:   "raw_reference.source_type",
			because: "unknown raw evidence routing cannot be authorized",
		},
		{
			name: "raw reference with unknown classification",
			mutate: func(r *model.NormalizedLog) {
				r.RawReference = validRawReference(r.Region)
				r.RawReference.Classification = "SECRET"
			},
			field:   "raw_reference.classification",
			because: "raw evidence classification is a closed authorization enum",
		},
		{
			name: "raw reference without to",
			mutate: func(r *model.NormalizedLog) {
				r.RawReference = validRawReference(r.Region)
				r.RawReference.To = time.Time{}
			},
			field:   "raw_reference.to",
			because: "a raw evidence range must be complete",
		},
		{
			name: "raw reference with inverted range",
			mutate: func(r *model.NormalizedLog) {
				r.RawReference = validRawReference(r.Region)
				r.RawReference.From = r.RawReference.To.Add(time.Nanosecond)
			},
			field:   "raw_reference.to",
			because: "a raw evidence range cannot run backward",
		},
		{
			name: "raw reference expires at range end",
			mutate: func(r *model.NormalizedLog) {
				r.RawReference = validRawReference(r.Region)
				r.RawReference.ExpiresAt = r.RawReference.To
			},
			field:   "raw_reference.expires_at",
			because: "a raw reference must remain valid after its range ends",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := validRecord()
			test.mutate(&record)

			err := record.Validate()
			if err == nil {
				t.Fatalf("want a violation on %s because %s, got none", test.field, test.because)
			}
			var validation *model.ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("want *model.ValidationError, got %T", err)
			}
			if !validation.Has(test.field) {
				t.Fatalf("want a violation on %s because %s, got violations on %v",
					test.field, test.because, validation.Fields())
			}
		})
	}
}

func validRawReference(region string) *model.RegionalLogReference {
	to := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	return &model.RegionalLogReference{
		SourceType:     model.SourceTypeCloudWatch,
		Region:         region,
		Locator:        "safe-locator",
		From:           to.Add(-time.Minute),
		To:             to,
		Classification: "SENSITIVE",
		ExpiresAt:      to.Add(time.Hour),
	}
}

func TestValidateReportsEveryViolationNotOnlyTheFirst(t *testing.T) {
	record := validRecord()
	record.RecordID = ""
	record.BatchID = ""
	record.Redaction.PolicyVersion = ""

	err := record.Validate()
	var validation *model.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("want *model.ValidationError, got %v", err)
	}
	// A validator that stopped at the first failure would let a test that names
	// one field hide two more.
	for _, want := range []string{"record_id", "batch_id", "redaction.policy_version"} {
		if !validation.Has(want) {
			t.Errorf("want a violation on %s, got %v", want, validation.Fields())
		}
	}
}

func TestValidateOrdersViolationsDeterministically(t *testing.T) {
	record := validRecord()
	record.Attributes = map[string]model.SafeValue{
		"zeta":  {Kind: model.SafeKindString, Int: 1},
		"alpha": {Kind: model.SafeKindString, Int: 2},
		"mid":   {Kind: model.SafeKindString, Int: 3},
	}

	first := fieldsOf(t, record.Validate())
	for i := 0; i < 20; i++ {
		if got := fieldsOf(t, record.Validate()); !equalStrings(got, first) {
			t.Fatalf("violation order varies between runs: %v then %v", first, got)
		}
	}
	want := []string{
		"attributes[0].kind",
		"attributes[1].kind",
		"attributes[2].kind",
	}
	if !equalStrings(first, want) {
		t.Fatalf("want violations sorted by attribute key %v, got %v", want, first)
	}
}

func fieldsOf(t *testing.T, err error) []string {
	t.Helper()
	var validation *model.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("want *model.ValidationError, got %v", err)
	}
	return validation.Fields()
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
