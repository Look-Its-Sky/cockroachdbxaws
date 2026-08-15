package fingerprint_test

import (
	"strings"
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/fingerprint"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/normalize"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/builders"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/otlpgen"
)

func TestDynamicRequestAndCustomerValuesProduceTheSameFingerprint(t *testing.T) {
	producer := otlpgen.New()
	first := producer.Record(
		otlpgen.WithRecordUID("0194f0a0-0000-7000-8000-000000000011"),
		otlpgen.WithBody("charge failed for order 4711"),
		otlpgen.WithException("PaymentDeclined", "request 8f2c1d customer 90210", ""),
		otlpgen.WithAttribute("request.id", otlpgen.StringValue("8f2c1d")),
	)
	second := producer.Record(
		otlpgen.WithRecordUID("0194f0a0-0000-7000-8000-000000000012"),
		otlpgen.WithBody("charge failed for order 6384"),
		otlpgen.WithException("PaymentDeclined", "request 7e3d9a customer 80421", ""),
		otlpgen.WithAttribute("request.id", otlpgen.StringValue("7e3d9a")),
	)

	records := normalizeRecords(t, producer, first, second)
	left := fingerprintError(t, records[0])
	right := fingerprintError(t, records[1])

	if left.Digest != right.Digest {
		t.Fatalf("dynamic identifiers split one error family:\n first  %s\n second %s", left.Digest, right.Digest)
	}
	for _, unsafe := range []string{"4711", "6384", "8f2c1d", "7e3d9a", "90210", "80421"} {
		if strings.Contains(left.Explanation(), unsafe) || strings.Contains(right.Explanation(), unsafe) {
			t.Errorf("dynamic value %q survived into a fingerprint explanation", unsafe)
		}
	}
}

func TestMateriallyDifferentExceptionsAndApplicationFramesProduceDifferentFingerprints(t *testing.T) {
	factory := builders.NewFactory()
	base := factory.Record(t, builders.WithException(
		"PaymentDeclined", "card declined for request <hex>",
		builders.AppFrame("charge", "payment/charge"),
		builders.LibraryFrame("ServeHTTP", "net/http"),
	))

	tests := []struct {
		name   string
		change builders.RecordOption
	}{
		{
			name: "exception type",
			change: builders.WithException(
				"PaymentProviderUnavailable", "card declined for request <hex>",
				builders.AppFrame("charge", "payment/charge"),
			),
		},
		{
			name: "application frame",
			change: builders.WithException(
				"PaymentDeclined", "card declined for request <hex>",
				builders.AppFrame("authorize", "payment/provider"),
			),
		},
	}

	want := fingerprintError(t, base)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := factory.Record(t, test.change)
			got := fingerprintError(t, changed)
			if got.Digest == want.Digest {
				t.Fatalf("materially different %s produced fingerprint %s", test.name, got.Digest)
			}
		})
	}
}

func TestUnvalidatedServiceNamespaceDoesNotSplitFingerprints(t *testing.T) {
	factory := builders.NewFactory()
	first := factory.Record(t)
	second := factory.Record(t)
	first.Service.Namespace = "producer-controlled-a"
	second.Service.Namespace = "producer-controlled-b"

	left := fingerprintError(t, first)
	right := fingerprintError(t, second)
	if left.Digest != right.Digest {
		t.Fatalf("unvalidated namespaces split one trusted service identity: %s != %s", left.Digest, right.Digest)
	}
	if strings.Contains(left.Explanation(), "producer-controlled") {
		t.Fatalf("unvalidated namespace entered fingerprint explanation: %s", left.Explanation())
	}
}

func TestRealOTLPApplicationFrameChangesSplitFingerprints(t *testing.T) {
	producer := otlpgen.New()
	first := producer.Record(
		otlpgen.WithRecordUID("0194f0a0-0000-7000-8000-000000000021"),
		otlpgen.WithBody("payment failed"),
		otlpgen.WithException("PaymentDeclined", "provider declined", "  at charge (internal/payment/charge.go:118:9)\n  at (*mux).ServeHTTP (net/http/server.go:2938)"),
	)
	second := producer.Record(
		otlpgen.WithRecordUID("0194f0a0-0000-7000-8000-000000000022"),
		otlpgen.WithBody("payment failed"),
		otlpgen.WithException("PaymentDeclined", "provider declined", "  at authorize (internal/payment/provider.go:77:4)\n  at (*mux).ServeHTTP (net/http/server.go:2938)"),
	)

	records := normalizeRecords(t, producer, first, second)
	for i, record := range records {
		if record.Exception == nil || len(record.Exception.StackFrames) != 2 || !record.Exception.StackFrames[0].InApplication {
			t.Fatalf("record %d did not promote typed application frames: %+v", i, record.Exception)
		}
		if record.Exception.StackFrames[1].InApplication {
			t.Fatalf("record %d classified net/http as application code: %+v", i, record.Exception.StackFrames[1])
		}
	}
	if left, right := fingerprintError(t, records[0]), fingerprintError(t, records[1]); left.Digest == right.Digest {
		t.Fatalf("real OTLP application-frame change did not split fingerprint %s", left.Digest)
	}
}

func TestTimestampsAndUppercaseRequestIDsDoNotSplitFingerprints(t *testing.T) {
	producer := otlpgen.New()
	first := producer.Record(
		otlpgen.WithRecordUID("0194f0a0-0000-7000-8000-000000000031"),
		otlpgen.WithBody("request REQ-AZ91-KLM2 failed at 2026-08-06T12:03:04.123Z"),
	)
	second := producer.Record(
		otlpgen.WithRecordUID("0194f0a0-0000-7000-8000-000000000032"),
		otlpgen.WithBody("request REQ-QX72-P9RT failed at 2026-08-07T14:05:06.987Z"),
	)
	records := normalizeRecords(t, producer, first, second)

	left, right := fingerprintError(t, records[0]), fingerprintError(t, records[1])
	if left.Digest != right.Digest {
		t.Fatalf("timestamp/request ID split one template:\nfirst  %s (%s)\nsecond %s (%s)",
			left.Digest, left.Explanation(), right.Digest, right.Explanation())
	}
}

func TestLibraryOnlyFramesAndRawLineNumbersDoNotSplitFingerprints(t *testing.T) {
	factory := builders.NewFactory()
	first := factory.Record(t, builders.WithException(
		"PaymentDeclined", "card declined",
		model.StackFrame{Function: "charge", Module: "payment/charge", File: "payment/charge.go:118:9", InApplication: true},
		builders.LibraryFrame("ServeHTTP", "net/http"),
	))
	second := factory.Record(t, builders.WithException(
		"PaymentDeclined", "card declined",
		model.StackFrame{Function: "charge", Module: "payment/charge", File: "payment/charge.go:991:27", InApplication: true},
		builders.LibraryFrame("run", "runtime"),
	))

	left := fingerprintError(t, first)
	right := fingerprintError(t, second)
	if left.Digest != right.Digest {
		t.Fatalf("excluded frame details split the fingerprint:\n first  %s\n second %s", left.Digest, right.Digest)
	}
}

func TestFingerprintIsVersionedAndExplainsItsSafeInputs(t *testing.T) {
	record := builders.NewFactory().Record(t,
		builders.WithException("PaymentDeclined", "card declined", builders.AppFrame("charge", "payment/charge")),
	)

	got := fingerprintError(t, record)
	if got.Version != fingerprint.ErrorV1 {
		t.Fatalf("want version %s, got %s", fingerprint.ErrorV1, got.Version)
	}
	if len(got.Digest) != 64 || got.Digest != strings.ToLower(got.Digest) {
		t.Fatalf("want a lowercase SHA-256 digest, got %q", got.Digest)
	}
	for _, safeInput := range []string{"paymentservice", "PaymentDeclined", "payment/charge"} {
		if !strings.Contains(got.Explanation(), safeInput) {
			t.Errorf("want explanation to contain safe input %q, got %q", safeInput, got.Explanation())
		}
	}
}

func TestErrorV1DigestIsPinned(t *testing.T) {
	record := builders.NewFactory().Record(t,
		builders.WithException(
			"PaymentDeclined",
			"card declined for request <identifier>",
			model.StackFrame{Function: "charge", Module: "payment/charge", File: "payment/charge.go:118:9", InApplication: true},
		),
	)
	got := fingerprintError(t, record)
	const want = "40383a88da768ea6b92a020030dbc2227f93d0e8429aa59af042924119cb9167"
	if got.Digest != want {
		t.Fatalf("error:v1 digest changed: want %s, got %s; explanation: %s", want, got.Digest, got.Explanation())
	}
}

func normalizeRecords(t *testing.T, producer *otlpgen.Producer, records ...*otlpgen.Record) []model.NormalizedLog {
	t.Helper()
	envelope := builders.NewFactory().Envelope(t)
	result, err := normalize.New().Request(envelope, producer.Request(records...))
	if err != nil {
		t.Fatalf("normalize request: %v", err)
	}
	if len(result.Rejected) != 0 {
		t.Fatalf("records rejected: %+v", result.Rejected)
	}
	if len(result.Records) != len(records) {
		t.Fatalf("want %d records, got %d", len(records), len(result.Records))
	}
	return result.Records
}

func fingerprintError(t *testing.T, record model.NormalizedLog) fingerprint.Result {
	t.Helper()
	result, err := fingerprint.Error(record)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return result
}
