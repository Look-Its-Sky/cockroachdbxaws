package cloudwatch_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/identity"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/cwgen"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
)

const testBatchID = "0194f0a0-0000-7000-8000-0000000000b1"

func testGroup() cloudwatch.Group {
	return cloudwatch.Group{
		Account:  cwgen.DefaultAccount,
		Region:   cwgen.DefaultRegion,
		LogGroup: cwgen.DefaultLogGroup,
	}
}

func mapperConfig() cloudwatch.MapperConfig {
	return cloudwatch.MapperConfig{
		Policy:              redact.MinimalPolicy(),
		Account:             cwgen.DefaultAccount,
		Region:              cwgen.DefaultRegion,
		SourceInstance:      "cloudwatch-adapter-us-east-1-0",
		CredentialIdentity:  "arn:aws:iam::123456789012:role/log-analysis-reader",
		AllowedServices:     []string{cwgen.DefaultService},
		AllowedEnvironments: []string{cwgen.DefaultEnvironment},
		Service:             cwgen.DefaultService,
		Environment:         cwgen.DefaultEnvironment,
		Classification:      "INTERNAL",
		RawReferenceTTL:     72 * time.Hour,
	}
}

func newMapper(t *testing.T) *cloudwatch.Mapper {
	t.Helper()
	mapper, err := cloudwatch.NewMapper(mapperConfig())
	if err != nil {
		t.Fatalf("building a mapper: %v", err)
	}
	return mapper
}

func mapOne(t *testing.T, mapper *cloudwatch.Mapper, event cloudwatch.Event) model.NormalizedLog {
	t.Helper()
	record, err := mapper.Record(testBatchID, testGroup(), event, fakeclock.Origin)
	if err != nil {
		t.Fatalf("mapping an event: %v", err)
	}
	return record
}

// The identity a record carries must be the one identity-and-admission.md
// defines for this source, derived from the native event ID rather than from
// anything the adapter chose at read time.
func TestMapperGivesAnEventTheNativeCloudWatchIdentity(t *testing.T) {
	producer := cwgen.New()
	event := producer.PaymentError()
	mapper := newMapper(t)

	record := mapOne(t, mapper, event)

	want, err := identity.CloudWatchV1(identity.CloudWatchLocator{
		Account:   cwgen.DefaultAccount,
		Region:    cwgen.DefaultRegion,
		LogGroup:  cwgen.DefaultLogGroup,
		LogStream: event.StreamName,
		EventID:   event.EventID,
	})
	if err != nil {
		t.Fatalf("deriving the expected identity: %v", err)
	}
	if record.RecordID != want {
		t.Fatalf("want record id %s, got %s", want, record.RecordID)
	}
	if record.RecordIDVersion != model.RecordIDVersionCloudWatchV1 {
		t.Fatalf("want record id version %s, got %s", model.RecordIDVersionCloudWatchV1, record.RecordIDVersion)
	}
	if record.IdentityQuality != model.IdentityQualityNative {
		t.Fatalf("a native event ID is native identity, got %s", record.IdentityQuality)
	}
}

// This is the identity fixture the replay behaviour rests on: the same event
// read twice, in two batches, is the same record.
func TestMapperGivesTheSameEventTheSameIdentityInEveryBatch(t *testing.T) {
	producer := cwgen.New()
	event := producer.PaymentError()
	mapper := newMapper(t)

	first := mapOne(t, mapper, event)
	second, err := mapper.Record("0194f0a0-0000-7000-8000-0000000000b2", testGroup(), event, fakeclock.Origin.Add(time.Hour))
	if err != nil {
		t.Fatalf("mapping the replayed event: %v", err)
	}

	if first.RecordID != second.RecordID {
		t.Fatalf("replay changed record identity: %s then %s", first.RecordID, second.RecordID)
	}
	if first.BatchID == second.BatchID {
		t.Fatal("a batch id names a transport attempt, so two attempts must not share one")
	}
}

func TestMapperGivesDistinctEventsDistinctIdentities(t *testing.T) {
	producer := cwgen.New()
	mapper := newMapper(t)

	seen := map[string]cloudwatch.Event{}
	for i := 0; i < 50; i++ {
		event := producer.PaymentError()
		record := mapOne(t, mapper, event)
		if previous, collided := seen[record.RecordID]; collided {
			t.Fatalf("events %s and %s collided on %s", previous.EventID, event.EventID, record.RecordID)
		}
		seen[record.RecordID] = event
	}
}

// Two events with byte-identical messages are two occurrences, not one. Content
// hashing is prohibited for exactly this reason.
func TestMapperDoesNotMergeTwoIdenticalMessages(t *testing.T) {
	producer := cwgen.New()
	mapper := newMapper(t)

	first := producer.Event(cwgen.WithMessage("connection reset"))
	second := producer.Event(cwgen.WithMessage("connection reset"))

	if mapOne(t, mapper, first).RecordID == mapOne(t, mapper, second).RecordID {
		t.Fatal("two legitimate identical messages must remain two occurrences")
	}
}

func TestMapperMapsCloudWatchTimesToEventAndObservedTime(t *testing.T) {
	producer := cwgen.New()
	event := producer.Event(cwgen.LateBy(90 * time.Second))
	mapper := newMapper(t)

	record := mapOne(t, mapper, event)

	if !record.EventTime.Equal(event.Timestamp) {
		t.Fatalf("want event time %s, got %s", event.Timestamp, record.EventTime)
	}
	if !record.ObservedTime.Equal(event.IngestionTime) {
		t.Fatalf("want observed time %s, got %s", event.IngestionTime, record.ObservedTime)
	}
	if record.TimestampInferred {
		t.Fatal("a reported event time inside its valid range is not inferred")
	}
}

func TestMapperInfersEventTimeWhenCloudWatchReportsNone(t *testing.T) {
	producer := cwgen.New()
	event := producer.Event(cwgen.WithoutEventTime())
	mapper := newMapper(t)

	record := mapOne(t, mapper, event)

	if !record.EventTime.Equal(event.IngestionTime) {
		t.Fatalf("a missing event time uses observed time, got %s", record.EventTime)
	}
	if !record.TimestampInferred || record.TimestampInferenceReason == "" {
		t.Fatalf("an inferred timestamp must say so and say why, got inferred=%t reason=%q",
			record.TimestampInferred, record.TimestampInferenceReason)
	}
}

func TestMapperInfersAnEventTimeOutsideItsValidRange(t *testing.T) {
	producer := cwgen.New()
	// A producer clock two days behind must not be able to hold, or reopen, an
	// incident window at an arbitrary point in the past.
	event := producer.Event(cwgen.AtEventTime(fakeclock.Origin.Add(-48 * time.Hour)))
	mapper := newMapper(t)

	record := mapOne(t, mapper, event)

	if !record.EventTime.Equal(event.IngestionTime) {
		t.Fatalf("an out-of-range event time falls back to observed time, got %s", record.EventTime)
	}
	if !record.TimestampInferred {
		t.Fatal("want the fallback marked inferred")
	}
}

// CloudWatch has no severity field. A declared level inside a structured
// message is a mapping; a word that happens to appear in prose is a guess, and
// the missing-data policy says the normalizer does not guess.
func TestMapperMapsDeclaredSeverityAndNeverGuessesFromProse(t *testing.T) {
	tests := []struct {
		name      string
		message   string
		wantClass model.SeverityClass
		wantText  string
	}{
		{"declared error level", `{"level":"error","msg":"charge failed"}`, model.SeverityClassError, "ERROR"},
		{"declared severity field", `{"severity":"WARNING","msg":"retrying"}`, model.SeverityClassWarn, "WARN"},
		{"declared fatal", `{"level":"critical","msg":"out of memory"}`, model.SeverityClassFatal, "FATAL"},
		{"declared debug", `{"level":"debug","msg":"cache miss"}`, model.SeverityClassDebug, "DEBUG"},
		{"declared info", `{"level":"info","msg":"started"}`, model.SeverityClassInfo, "INFO"},
		{"unknown level token", `{"level":"purple","msg":"???"}`, model.SeverityClassUnspecified, ""},
		{"plain prose naming a level", "ERROR: charge failed for order", model.SeverityClassUnspecified, ""},
		{"no level at all", `{"msg":"charge failed"}`, model.SeverityClassUnspecified, ""},
	}

	mapper := newMapper(t)
	producer := cwgen.New()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := mapOne(t, mapper, producer.Event(cwgen.WithMessage(test.message)))

			if record.SeverityClass != test.wantClass {
				t.Fatalf("want severity class %s, got %s", test.wantClass, record.SeverityClass)
			}
			if record.SeverityText != test.wantText {
				t.Fatalf("want severity text %q, got %q", test.wantText, record.SeverityText)
			}
			if test.wantClass == model.SeverityClassUnspecified && record.SeverityNumber != 0 {
				t.Fatalf("an unmapped severity has no number, got %d", record.SeverityNumber)
			}
		})
	}
}

// The severity token is only ever read out of the already-redacted body, and
// the text stored is this package's own canonical name. A hostile level value
// therefore cannot smuggle content into a persisted field.
func TestMapperNeverStoresARawSeverityToken(t *testing.T) {
	producer := cwgen.New()
	mapper := newMapper(t)
	event := producer.Event(cwgen.WithMessage(`{"level":"error authorization: Bearer abcdef","msg":"x"}`))

	record := mapOne(t, mapper, event)

	if strings.Contains(record.SeverityText, "Bearer") || strings.Contains(record.SeverityText, "authorization") {
		t.Fatalf("severity text echoed a raw token: %q", record.SeverityText)
	}
}

func TestMapperCarriesTheNativeLocatorAsSafeMetadata(t *testing.T) {
	producer := cwgen.New()
	event := producer.PaymentError()
	mapper := newMapper(t)

	record := mapOne(t, mapper, event)

	wantAttributes := map[string]string{
		cloudwatch.AttributeLogGroup:  cwgen.DefaultLogGroup,
		cloudwatch.AttributeLogStream: event.StreamName,
		cloudwatch.AttributeEventID:   event.EventID,
	}
	for key, want := range wantAttributes {
		value, present := record.Attributes[key]
		if !present {
			t.Fatalf("want attribute %s, got none", key)
		}
		if value.Kind != model.SafeKindString || value.String != want {
			t.Fatalf("want attribute %s = %q, got %+v", key, want, value)
		}
	}

	wantResource := map[string]string{
		"cloud.provider":              "aws",
		"cloud.region":                cwgen.DefaultRegion,
		"cloud.account.id":            cwgen.DefaultAccount,
		"service.name":                cwgen.DefaultService,
		"deployment.environment.name": cwgen.DefaultEnvironment,
	}
	for key, want := range wantResource {
		value, present := record.ResourceAttributes[key]
		if !present {
			t.Fatalf("want resource attribute %s, got none", key)
		}
		if value.String != want {
			t.Fatalf("want resource attribute %s = %q, got %q", key, want, value.String)
		}
	}
}

// CloudWatch carries no authenticated service identity, so the identity comes
// from the operator's group configuration. Anything the message says about
// itself is an untrusted claim and must not displace it.
func TestMapperTakesServiceIdentityFromConfigurationNotFromTheMessage(t *testing.T) {
	producer := cwgen.New()
	mapper := newMapper(t)
	event := producer.Event(cwgen.WithMessage(
		`{"service.name":"cartservice","deployment.environment.name":"staging","msg":"charge failed"}`))

	record := mapOne(t, mapper, event)

	if record.Service.Name != cwgen.DefaultService {
		t.Fatalf("a message claim overrode configured service identity: %s", record.Service.Name)
	}
	if record.Service.Environment != cwgen.DefaultEnvironment {
		t.Fatalf("a message claim overrode configured environment: %s", record.Service.Environment)
	}
	if record.Service.Status != model.EnrichmentAvailable {
		t.Fatalf("a configured service identity is available, got %s", record.Service.Status)
	}
}

func TestMapperLeavesDeploymentToEnrichment(t *testing.T) {
	producer := cwgen.New()
	mapper := newMapper(t)

	record := mapOne(t, mapper, producer.PaymentError())

	if record.Deployment.ID != model.UnknownDeployment || record.Deployment.Status != model.EnrichmentPending {
		t.Fatalf("want an unknown, pending deployment, got %+v", record.Deployment)
	}
}

// The redaction fixture: content that must never be persisted is gone before a
// NormalizedLog exists, not cleaned up afterwards.
func TestMapperRedactsProhibitedContentBeforeARecordExists(t *testing.T) {
	policy, err := redact.MinimalPolicy().WithForbiddenValues("hunter2", "AKIAIOSFODNN7EXAMPLE")
	if err != nil {
		t.Fatalf("building a policy: %v", err)
	}
	config := mapperConfig()
	config.Policy = policy
	mapper, err := cloudwatch.NewMapper(config)
	if err != nil {
		t.Fatalf("building a mapper: %v", err)
	}
	producer := cwgen.New()

	messages := []string{
		"authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl",
		"db connection postgres://app:hunter2@db.internal:5432/payments failed",
		`{"password":"hunter2","msg":"login failed"}`,
		"aws key AKIAIOSFODNN7EXAMPLE rejected",
		"customer alice@example.com could not be charged",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----",
	}
	for _, message := range messages {
		t.Run(message[:min(24, len(message))], func(t *testing.T) {
			record := mapOne(t, mapper, producer.Event(cwgen.WithMessage(message)))

			if err := policy.ValidateRecord(record); err != nil {
				t.Fatalf("a mapped record still holds prohibited content: %v", err)
			}
			rendered := renderRecord(t, record)
			for _, secret := range []string{"hunter2", "AKIAIOSFODNN7EXAMPLE", "alice@example.com", "MIIEow"} {
				if strings.Contains(rendered, secret) {
					t.Fatalf("mapped record leaked %q: %s", secret, rendered)
				}
			}
			if record.Redaction.PolicyVersion != policy.Version() {
				t.Fatalf("want the producing policy version recorded, got %q", record.Redaction.PolicyVersion)
			}
		})
	}
}

func TestMapperWithholdsAMessageItCannotProveSafe(t *testing.T) {
	producer := cwgen.New()
	mapper := newMapper(t)
	// Invalid UTF-8 cannot be shown to be safe, so it becomes withheld metadata
	// rather than being passed through.
	event := producer.Event(cwgen.WithMessage("charge failed \xff\xfe"))

	record := mapOne(t, mapper, event)

	if record.Body.Kind != model.SafeKindWithheld {
		t.Fatalf("want a withheld body, got %+v", record.Body)
	}
	if len(record.Redaction.WithheldFields) == 0 {
		t.Fatal("a withheld value must be named in withheld_fields so it is measurable")
	}
	if err := redact.MinimalPolicy().ValidateRecord(record); err != nil {
		t.Fatalf("a withheld record must still be safe: %v", err)
	}
}

func TestMapperAttachesARegionLocalRawReference(t *testing.T) {
	producer := cwgen.New()
	mapper := newMapper(t)
	event := producer.PaymentError()

	record := mapOne(t, mapper, event)

	reference := record.RawReference
	if reference == nil {
		t.Fatal("want a regional reference to the raw stream")
	}
	if reference.Region != record.Region {
		t.Fatalf("a reference must not point outside the record's region: %s vs %s", reference.Region, record.Region)
	}
	if reference.SourceType != model.SourceTypeCloudWatch {
		t.Fatalf("want source type %s, got %s", model.SourceTypeCloudWatch, reference.SourceType)
	}
	if !strings.Contains(reference.Locator, event.StreamName) {
		t.Fatalf("a locator must name the stream it points at, got %q", reference.Locator)
	}
	for _, credential := range []string{"AKIA", "ASIA", "aws_secret", "Bearer", "token"} {
		if strings.Contains(reference.Locator, credential) {
			t.Fatalf("a regional reference must never contain source credentials, got %q", reference.Locator)
		}
	}
	if !reference.ExpiresAt.After(reference.To) {
		t.Fatalf("want an expiry after the referenced range, got %s vs %s", reference.ExpiresAt, reference.To)
	}
}

func TestMapperRefusesAnEventItCannotIdentifyOrRead(t *testing.T) {
	producer := cwgen.New()
	mapper := newMapper(t)

	tests := []struct {
		name    string
		event   cloudwatch.Event
		because string
	}{
		{"no native event id", producer.Event(cwgen.WithEventID("")),
			"content hashing is prohibited, so there is no identity to fall back to"},
		{"no stream name", producer.Event(cwgen.WithStream("")),
			"the checkpoint and the identity are both per stream"},
		{"no message", producer.Event(cwgen.WithMessage("")),
			"a record needs a body or an event name and the mapper never invents either"},
		{"no ingestion time", producer.Event(cwgen.WithoutIngestionTime()),
			"observed time is the one timestamp a record cannot do without"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := mapper.Record(testBatchID, testGroup(), test.event, fakeclock.Origin)
			if err == nil {
				t.Fatalf("want a record-local refusal because %s", test.because)
			}
			if !errors.Is(err, cloudwatch.ErrUnusableRecord) {
				t.Fatalf("want a record-local category so siblings survive, got %v", err)
			}
		})
	}
}

// A stream from another region is not a bad record, it is a crossed regional
// boundary. Quarantine deletes durable payloads, so this may never be treated
// as record-local.
func TestMapperRefusesAStreamOutsideItsRegion(t *testing.T) {
	producer := cwgen.New()
	mapper := newMapper(t)
	foreign := testGroup()
	foreign.Region = "eu-west-1"

	_, err := mapper.Record(testBatchID, foreign, producer.PaymentError(), fakeclock.Origin)

	if !errors.Is(err, cloudwatch.ErrRegionalBoundary) {
		t.Fatalf("want a regional boundary refusal, got %v", err)
	}
	if errors.Is(err, cloudwatch.ErrUnusableRecord) {
		t.Fatal("a crossed boundary must not be reported as a record-local fault")
	}
}

func TestMapperRefusesAnAccountOutsideItsConfiguration(t *testing.T) {
	producer := cwgen.New()
	mapper := newMapper(t)
	foreign := testGroup()
	foreign.Account = "210987654321"

	_, err := mapper.Record(testBatchID, foreign, producer.PaymentError(), fakeclock.Origin)

	if !errors.Is(err, cloudwatch.ErrRegionalBoundary) {
		t.Fatalf("want a boundary refusal for another account, got %v", err)
	}
}

func TestMapperConfigurationIsValidatedBeforeItCanProduceAnything(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*cloudwatch.MapperConfig)
		because string
	}{
		{"no policy", func(c *cloudwatch.MapperConfig) { c.Policy = nil },
			"nothing may be built without the policy that makes it safe"},
		{"service outside the allowed set", func(c *cloudwatch.MapperConfig) { c.Service = "cartservice" },
			"the envelope's allowed set is the authorization, not a suggestion"},
		{"environment outside the allowed set", func(c *cloudwatch.MapperConfig) { c.Environment = "staging" },
			"the same rule applies to environment claims"},
		{"no region", func(c *cloudwatch.MapperConfig) { c.Region = "" },
			"a regional adapter without a region has no boundary to enforce"},
		{"no account", func(c *cloudwatch.MapperConfig) { c.Account = "" },
			"the account is part of record identity"},
		{"no source instance", func(c *cloudwatch.MapperConfig) { c.SourceInstance = "" },
			"the trusted envelope requires one"},
		{"unknown classification", func(c *cloudwatch.MapperConfig) { c.Classification = "SECRET" },
			"a regional reference carries an access classification from a closed set"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := mapperConfig()
			test.change(&config)

			mapper, err := cloudwatch.NewMapper(config)

			if err == nil {
				t.Fatalf("want a configuration refusal because %s", test.because)
			}
			if !errors.Is(err, cloudwatch.ErrInvalidConfig) {
				t.Fatalf("want the configuration category, got %v", err)
			}
			if mapper != nil {
				t.Fatal("want no mapper alongside a refusal")
			}
		})
	}
}

// Every record this package emits is handed to a journal that validates it. A
// mapper that can produce an invalid record produces a poisoned batch instead
// of an admitted one.
func TestMapperOutputAlwaysPassesStructuralAndRedactionValidation(t *testing.T) {
	policy := redact.MinimalPolicy()
	mapper := newMapper(t)
	producer := cwgen.New()

	for _, message := range cwgen.AwkwardMessages() {
		event := producer.Event(cwgen.WithMessage(message))
		record, err := mapper.Record(testBatchID, testGroup(), event, fakeclock.Origin)
		if errors.Is(err, cloudwatch.ErrUnusableRecord) {
			continue
		}
		if err != nil {
			t.Fatalf("mapping %q: %v", message, err)
		}
		if err := record.Validate(); err != nil {
			t.Fatalf("mapping %q produced a structurally invalid record: %v", message, err)
		}
		if err := policy.ValidateRecord(record); err != nil {
			t.Fatalf("mapping %q produced an unsafe record: %v", message, err)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// renderRecord serializes a record so a test can assert that a secret appears
// nowhere in it at all, rather than only in the field it was expected in.
func renderRecord(t *testing.T, record model.NormalizedLog) string {
	t.Helper()
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("rendering a record: %v", err)
	}
	return string(encoded)
}
