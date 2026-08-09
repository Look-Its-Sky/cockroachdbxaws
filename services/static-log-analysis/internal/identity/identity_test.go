package identity_test

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/identity"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

const (
	instance = "collector-us-east-1-0"
	uid      = "0194f0a0-0000-7000-8000-000000000001"
)

func TestOTLPV1MatchesTheDocumentedDerivation(t *testing.T) {
	// SHA-256("otlp:v1" || trusted_source_instance || log.record.uid)
	digest := sha256.Sum256([]byte("otlp:v1" + instance + uid))
	want := hex.EncodeToString(digest[:])

	got, err := identity.OTLPV1(instance, uid)
	if err != nil {
		t.Fatalf("deriving: %v", err)
	}
	if got != want {
		t.Fatalf("want %s, got %s", want, got)
	}
}

func TestTheSameInputsAlwaysProduceTheSameIdentity(t *testing.T) {
	first, err := identity.OTLPV1(instance, uid)
	if err != nil {
		t.Fatalf("deriving: %v", err)
	}

	// This is the whole point: a retried record must hash to the value it
	// hashed to before the process restarted, and before this release.
	for i := 0; i < 100; i++ {
		again, err := identity.OTLPV1(instance, uid)
		if err != nil {
			t.Fatalf("deriving: %v", err)
		}
		if again != first {
			t.Fatalf("derivation is not stable: %s then %s", first, again)
		}
	}
}

func TestIdentityHasTheShapeItsVersionDeclares(t *testing.T) {
	got, err := identity.OTLPV1(instance, uid)
	if err != nil {
		t.Fatalf("deriving: %v", err)
	}

	// A record whose identifier does not match its version's shape is rejected
	// by validation, so the deriver has to produce that shape.
	record := model.NormalizedLog{RecordID: got, RecordIDVersion: model.RecordIDVersionOTLPV1}
	if err := record.Validate(); err != nil {
		var validation *model.ValidationError
		if ok := asValidation(err, &validation); ok && validation.Has("record_id") {
			t.Fatalf("derived identity %s does not have the shape its version declares: %v", got, err)
		}
	}
}

func TestADifferentSourceInstanceProducesADifferentIdentity(t *testing.T) {
	first, _ := identity.OTLPV1(instance, uid)
	second, _ := identity.OTLPV1("collector-us-east-1-1", uid)

	// A record uid is a claim, not an authorization. Two sources that chose the
	// same uid must not collide, or one could overwrite the other's history.
	if first == second {
		t.Fatalf("want distinct identities per source instance, both were %s", first)
	}
}

func TestADifferentRecordUIDProducesADifferentIdentity(t *testing.T) {
	first, _ := identity.OTLPV1(instance, uid)
	second, _ := identity.OTLPV1(instance, "0194f0a0-0000-7000-8000-000000000002")

	if first == second {
		t.Fatalf("want distinct identities per record, both were %s", first)
	}
}

func TestUnusableInputsAreRefused(t *testing.T) {
	tests := []struct {
		name     string
		instance string
		uid      string
		want     string
		because  string
	}{
		{
			name:     "missing source instance",
			instance: "",
			uid:      uid,
			want:     "source instance",
			because:  "without it a uid alone is forgeable by another source",
		},
		{
			name:     "whitespace source instance",
			instance: "   ",
			uid:      uid,
			want:     "source instance",
			because:  "a blank instance is a missing instance",
		},
		{
			name:     "missing uid",
			instance: instance,
			uid:      "",
			want:     "record uid",
			because:  "a producer without a uid uses the derived identity version",
		},
		{
			name:     "uid that is not a uuid",
			instance: instance,
			uid:      "record-1",
			want:     "record uid",
			because:  "a non-conforming uid cannot be shown to be stable across a retry",
		},
		{
			name:     "uid that is not version 7",
			instance: instance,
			uid:      "9f8b7c6d-5e4f-4a3b-8c9d-0e1f2a3b4c5d",
			want:     "record uid",
			because:  "the contract requires a UUIDv7-compatible identifier",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := identity.OTLPV1(test.instance, test.uid)

			if err == nil {
				t.Fatalf("want a refusal because %s, got %s", test.because, got)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("want the error to name %s, got %v", test.want, err)
			}
			if got != "" {
				t.Errorf("want no identity alongside a refusal, got %s", got)
			}
		})
	}
}

func TestInvalidProducerUIDErrorIsCategorizedWithoutEchoingTheUID(t *testing.T) {
	secret := "password=hunter2"
	_, err := identity.OTLPV1(instance, secret)
	if !errors.Is(err, identity.ErrInvalidRecordUID) {
		t.Fatalf("want invalid uid category, got %v", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("identity error echoed producer uid: %v", err)
	}
}

func TestDerivedV1IsStableAcrossRetriesAndMapOrder(t *testing.T) {
	record := derivedRecord()
	first, err := identity.DerivedV1(record)
	if err != nil {
		t.Fatal(err)
	}

	// Neither transport-attempt state nor map iteration order belongs to a
	// record's identity. Both can change when the same export is retried.
	retried := record
	retried.BatchID = "0194f0a0-0000-7000-8000-000000000099"
	retried.Source.ReceivedAt = retried.Source.ReceivedAt.Add(time.Minute)
	retried.Attributes = map[string]model.SafeValue{
		"z": model.SafeInt(7),
		"a": model.SafeString("stable"),
	}
	again, err := identity.DerivedV1(retried)
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatalf("a retry changed derived identity: %s != %s", first, again)
	}
	if len(first) != 64 || strings.ToLower(first) != first {
		t.Fatalf("derived:v1 must produce 64 lowercase hex characters, got %q", first)
	}
	const documentedDigest = "7f54f9ab435ea4ad50896e73486049066bd832e43c4ad0c0e69957193a0fea7b"
	if first != documentedDigest {
		t.Fatalf("derived:v1 representative digest changed: got %s", first)
	}
}

func TestDerivedV1IncludesStableIncidentEvidence(t *testing.T) {
	base := derivedRecord()
	want, err := identity.DerivedV1(base)
	if err != nil {
		t.Fatal(err)
	}

	tests := map[string]func(*model.NormalizedLog){
		"source instance": func(r *model.NormalizedLog) { r.Source.SourceInstance = "collector-b" },
		"service":         func(r *model.NormalizedLog) { r.Service.Name = "cartservice" },
		"event time":      func(r *model.NormalizedLog) { r.EventTime = r.EventTime.Add(time.Nanosecond) },
		"severity":        func(r *model.NormalizedLog) { r.SeverityNumber++ },
		"body":            func(r *model.NormalizedLog) { r.Body = model.SafeString("different") },
		"attribute":       func(r *model.NormalizedLog) { r.Attributes["a"] = model.SafeString("different") },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			changed := derivedRecord()
			mutate(&changed)
			got, err := identity.DerivedV1(changed)
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Fatalf("changing %s did not change derived identity", name)
			}
		})
	}
}

func derivedRecord() model.NormalizedLog {
	return model.NormalizedLog{
		BatchID: "0194f0a0-0000-7000-8000-000000000001",
		Source: model.TrustedEnvelope{
			SourceType: model.SourceTypeOTLP, SourceAccount: "account-a", Region: "us-east-1",
			AllowedEnvironments: []string{"production"}, AllowedServices: []string{"paymentservice"},
			SourceInstance: "collector-a", CredentialIdentity: "spiffe://example/collector",
			ReceivedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		},
		EventTime: time.Date(2026, 1, 2, 3, 4, 0, 1, time.UTC), ObservedTime: time.Date(2026, 1, 2, 3, 4, 0, 2, time.UTC),
		SeverityNumber: 17, Body: model.SafeString("charge failed"),
		Attributes:  map[string]model.SafeValue{"a": model.SafeString("stable"), "z": model.SafeInt(7)},
		Service:     model.ServiceIdentity{Name: "paymentservice", Environment: "production"},
		Correlation: model.CorrelationIdentity{TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef"},
	}
}

const (
	account   = "123456789012"
	region    = "us-east-1"
	logGroup  = "/aws/lambda/paymentservice"
	logStream = "2026/01/01/[$LATEST]0e1f2a3b"
	eventID   = "39518522779705548990893699571816765783291046441449848832"
)

func cloudWatchLocator() identity.CloudWatchLocator {
	return identity.CloudWatchLocator{
		Account: account, Region: region, LogGroup: logGroup, LogStream: logStream, EventID: eventID,
	}
}

func TestCloudWatchV1MatchesTheDocumentedDerivation(t *testing.T) {
	// SHA-256("cw:v1" || D(account) || D(region) || D(log_group) || D(log_stream) || D(event_id)),
	// where D length-delimits each component with a big-endian uint32.
	hash := sha256.New()
	hash.Write([]byte("cw:v1"))
	for _, component := range []string{account, region, logGroup, logStream, eventID} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(component)))
		hash.Write(length[:])
		hash.Write([]byte(component))
	}
	want := hex.EncodeToString(hash.Sum(nil))

	got, err := identity.CloudWatchV1(cloudWatchLocator())
	if err != nil {
		t.Fatalf("deriving: %v", err)
	}
	if got != want {
		t.Fatalf("want %s, got %s", want, got)
	}
}

func TestCloudWatchV1IsStableAcrossRepeatedDerivation(t *testing.T) {
	first, err := identity.CloudWatchV1(cloudWatchLocator())
	if err != nil {
		t.Fatalf("deriving: %v", err)
	}
	// A checkpoint that was never committed makes the adapter reread an overlap.
	// The replay is only harmless because the same event hashes to the same
	// value on every pass.
	for i := 0; i < 100; i++ {
		again, err := identity.CloudWatchV1(cloudWatchLocator())
		if err != nil {
			t.Fatalf("deriving: %v", err)
		}
		if again != first {
			t.Fatalf("derivation is not stable: %s then %s", first, again)
		}
	}
}

func TestCloudWatchV1HasTheShapeItsVersionDeclares(t *testing.T) {
	got, err := identity.CloudWatchV1(cloudWatchLocator())
	if err != nil {
		t.Fatalf("deriving: %v", err)
	}
	record := model.NormalizedLog{RecordID: got, RecordIDVersion: model.RecordIDVersionCloudWatchV1}
	if err := record.Validate(); err != nil {
		var validation *model.ValidationError
		if ok := asValidation(err, &validation); ok && validation.Has("record_id") {
			t.Fatalf("derived identity %s does not have the shape its version declares: %v", got, err)
		}
	}
}

func TestCloudWatchV1DistinguishesEveryComponent(t *testing.T) {
	base := cloudWatchLocator()
	baseID, err := identity.CloudWatchV1(base)
	if err != nil {
		t.Fatalf("deriving: %v", err)
	}

	tests := []struct {
		name   string
		change func(*identity.CloudWatchLocator)
	}{
		{"account", func(l *identity.CloudWatchLocator) { l.Account = "210987654321" }},
		{"region", func(l *identity.CloudWatchLocator) { l.Region = "eu-west-1" }},
		{"log group", func(l *identity.CloudWatchLocator) { l.LogGroup = "/aws/lambda/cartservice" }},
		{"log stream", func(l *identity.CloudWatchLocator) { l.LogStream = "2026/01/01/[$LATEST]ffffffff" }},
		{"event id", func(l *identity.CloudWatchLocator) {
			l.EventID = "39518522779705548990893699571816765783291046441449848833"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := base
			test.change(&changed)
			got, err := identity.CloudWatchV1(changed)
			if err != nil {
				t.Fatalf("deriving: %v", err)
			}
			if got == baseID {
				t.Fatalf("a different %s must not collide, both were %s", test.name, got)
			}
		})
	}
}

// The components are operator-chosen free-form names, so a boundary between two
// of them that is not encoded is a boundary an operator can move. These two
// locators are distinct streams whose plain concatenation is identical.
func TestCloudWatchV1DoesNotCollideAcrossAMovedComponentBoundary(t *testing.T) {
	left := identity.CloudWatchLocator{
		Account: account, Region: region, LogGroup: "app/ab", LogStream: "c", EventID: eventID,
	}
	right := identity.CloudWatchLocator{
		Account: account, Region: region, LogGroup: "app/a", LogStream: "bc", EventID: eventID,
	}

	leftID, err := identity.CloudWatchV1(left)
	if err != nil {
		t.Fatalf("deriving left: %v", err)
	}
	rightID, err := identity.CloudWatchV1(right)
	if err != nil {
		t.Fatalf("deriving right: %v", err)
	}
	if leftID == rightID {
		t.Fatalf("two distinct streams share record id %s; their occurrences would merge", leftID)
	}
}

func TestCloudWatchV1RefusesAnIncompleteLocator(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*identity.CloudWatchLocator)
		want    string
		because string
	}{
		{"missing account", func(l *identity.CloudWatchLocator) { l.Account = "" }, "account",
			"the account is the trust anchor that stops another account colliding"},
		{"missing region", func(l *identity.CloudWatchLocator) { l.Region = "" }, "region",
			"identity must name the regional boundary the record stayed inside"},
		{"missing log group", func(l *identity.CloudWatchLocator) { l.LogGroup = "  " }, "log group",
			"a blank group is a missing group"},
		{"missing log stream", func(l *identity.CloudWatchLocator) { l.LogStream = "" }, "log stream",
			"the checkpoint is per stream, so identity must name one"},
		{"missing event id", func(l *identity.CloudWatchLocator) { l.EventID = "" }, "event id",
			"without the native event id two identical messages would share an identity"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			locator := cloudWatchLocator()
			test.change(&locator)
			got, err := identity.CloudWatchV1(locator)
			if err == nil {
				t.Fatalf("want a refusal because %s, got %s", test.because, got)
			}
			if !errors.Is(err, identity.ErrIncompleteLocator) {
				t.Fatalf("want the incomplete-locator category, got %v", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("want the error to name %s, got %v", test.want, err)
			}
			if got != "" {
				t.Errorf("want no identity alongside a refusal, got %s", got)
			}
		})
	}
}

func TestCloudWatchLocatorErrorDoesNotEchoTheLocator(t *testing.T) {
	locator := cloudWatchLocator()
	locator.LogStream = "customer-4711-token-hunter2"
	locator.EventID = ""

	_, err := identity.CloudWatchV1(locator)
	if err == nil {
		t.Fatal("want a refusal for a missing event id")
	}
	// A log stream name is operator-chosen but still untrusted content, and an
	// error travels into logs that are not redacted.
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), locator.LogStream) {
		t.Fatalf("identity error echoed the locator: %v", err)
	}
}

func asValidation(err error, target **model.ValidationError) bool {
	validation, ok := err.(*model.ValidationError)
	if ok {
		*target = validation
	}
	return ok
}
