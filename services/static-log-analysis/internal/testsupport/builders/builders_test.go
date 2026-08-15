package builders_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/builders"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/golden"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/tb"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/testids"
)

func TestDefaultRecordIsValid(t *testing.T) {
	// Record fails the test itself when the record does not validate, so
	// reaching the assertion is most of the proof; validating again states the
	// property rather than relying on a side effect.
	record := builders.NewFactory().Record(t)

	if err := record.Validate(); err != nil {
		t.Fatalf("default record is invalid: %v", err)
	}
}

func TestDefaultRecordMatchesItsFixture(t *testing.T) {
	// Every scenario that says nothing about a field inherits this record. A
	// change to any default has to be a reviewed diff, not a surprise in an
	// unrelated rule test.
	golden.JSON(t, "default_record.json", builders.NewFactory().Record(t))
}

func TestTwoFactoriesProduceIdenticalRecords(t *testing.T) {
	first := string(golden.Encode(t, builders.NewFactory().Record(t)))
	second := string(golden.Encode(t, builders.NewFactory().Record(t)))

	if first != second {
		t.Fatalf("factories with the same configuration disagree:\n%s\nand\n%s", first, second)
	}
}

func TestRecordsFromOneFactoryHaveDistinctIdentities(t *testing.T) {
	factory := builders.NewFactory()

	first := factory.Record(t)
	second := factory.Record(t)

	if first.RecordID == second.RecordID {
		t.Fatalf("want distinct record ids, both were %s", first.RecordID)
	}
	if first.BatchID == second.BatchID {
		t.Fatalf("want a separate batch per record, both were %s", first.BatchID)
	}
}

func TestRecordAttributesAreIndependentBetweenRecords(t *testing.T) {
	factory := builders.NewFactory()
	first := factory.Record(t)
	second := factory.Record(t)

	first.Attributes["payment.provider"] = model.SafeString("changed")

	if got := second.Attributes["payment.provider"]; got.String != "acme" {
		t.Fatalf("records share an attribute map: second record now reads %q", got.String)
	}
}

func TestObservedTimeFollowsTheScenarioClock(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	factory := builders.NewFactory(builders.WithClock(c), builders.WithIDs(testids.New(testids.WithClock(c))))

	first := factory.Record(t)
	c.Advance(90 * time.Second)
	second := factory.Record(t)

	if !first.ObservedTime.Equal(fakeclock.Origin) {
		t.Fatalf("want the first record observed at %s, got %s", fakeclock.Origin, first.ObservedTime)
	}
	if want := fakeclock.Origin.Add(90 * time.Second); !second.ObservedTime.Equal(want) {
		t.Fatalf("want the second record observed at %s, got %s", want, second.ObservedTime)
	}
	if want := second.ObservedTime.Add(-builders.DefaultEventLag); !second.EventTime.Equal(want) {
		t.Fatalf("want event time %s before observed time, got %s", want, second.EventTime)
	}
}

func TestBatchSharesOneTransportIdentity(t *testing.T) {
	factory := builders.NewFactory()

	records := factory.Batch(t, 5)

	if len(records) != 5 {
		t.Fatalf("want 5 records, got %d", len(records))
	}
	seen := map[string]bool{}
	for i, record := range records {
		if record.BatchID != records[0].BatchID {
			t.Errorf("record %d has batch id %s, want %s", i, record.BatchID, records[0].BatchID)
		}
		if seen[record.RecordID] {
			t.Errorf("record %d repeats record id %s", i, record.RecordID)
		}
		seen[record.RecordID] = true
	}
	// A batch identifies a transport attempt, so it never stands in for
	// per-record identity.
	if len(seen) != 5 {
		t.Fatalf("want 5 distinct record ids, got %d", len(seen))
	}
}

func TestBatchRejectsANonPositiveCount(t *testing.T) {
	recorder := tb.NewRecorder(t.Name())

	fataled := recorder.ExpectFatal(func() { builders.NewFactory().Batch(recorder, 0) })

	if !fataled {
		t.Fatal("want an empty batch rejected, got no failure")
	}
}

func TestOptionsApply(t *testing.T) {
	factory := builders.NewFactory()

	record := factory.Record(t,
		builders.WithService("cartservice"),
		builders.WithEnvironment("staging"),
		builders.SeverityFatal(),
		builders.WithBody("cart could not be emptied"),
		builders.WithCorrelation("4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"),
		builders.WithDeployment("cartservice-abc", "2026.3.5"),
		builders.WithAttribute("cart.size", model.SafeInt(3)),
		builders.WithException("CartUnavailable", "upstream refused",
			builders.AppFrame("emptyCart", "cart/handler"),
			builders.LibraryFrame("roundTrip", "net/http")),
	)

	if record.Service.Name != "cartservice" {
		t.Errorf("want service cartservice, got %s", record.Service.Name)
	}
	if record.Service.Environment != "staging" {
		t.Errorf("want environment staging, got %s", record.Service.Environment)
	}
	if record.SeverityClass != model.SeverityClassFatal || record.SeverityNumber != 21 {
		t.Errorf("want a FATAL severity, got %s/%d", record.SeverityClass, record.SeverityNumber)
	}
	if record.Body.String != "cart could not be emptied" {
		t.Errorf("want the given body, got %q", record.Body.String)
	}
	if record.Correlation.SpanID != "00f067aa0ba902b7" {
		t.Errorf("want the given span, got %q", record.Correlation.SpanID)
	}
	if record.Deployment.ID != "cartservice-abc" {
		t.Errorf("want the given deployment, got %q", record.Deployment.ID)
	}
	if got := record.Attributes["cart.size"]; got.Int != 3 {
		t.Errorf("want the given attribute, got %+v", got)
	}
	if record.Exception == nil || len(record.Exception.StackFrames) != 2 {
		t.Fatalf("want two stack frames, got %+v", record.Exception)
	}
	if !record.Exception.StackFrames[0].InApplication {
		t.Error("want the first frame marked in-application")
	}
	if record.Exception.StackFrames[1].InApplication {
		t.Error("want the library frame not marked in-application")
	}
	// Resource attributes are what a normalizer would have produced, so they
	// must follow the identity options rather than keep the defaults.
	if got := record.ResourceAttributes["service.name"]; got.String != "cartservice" {
		t.Errorf("want the resource attribute to follow the service, got %q", got.String)
	}
}

func TestWithEventNameReplacesTheBody(t *testing.T) {
	record := builders.NewFactory().Record(t, builders.WithEventName("payment.declined"))

	if !record.Body.IsZero() {
		t.Errorf("want no body alongside an event name, got %+v", record.Body)
	}
	if record.EventName != "payment.declined" {
		t.Errorf("want the event name, got %q", record.EventName)
	}
}

func TestWithInferredTimestampRecordsWhy(t *testing.T) {
	record := builders.NewFactory().Record(t, builders.WithInferredTimestamp("event_time missing"))

	if !record.TimestampInferred {
		t.Error("want the timestamp marked inferred")
	}
	if !record.EventTime.Equal(record.ObservedTime) {
		t.Errorf("want an inferred event time to equal observed time, got %s and %s",
			record.EventTime, record.ObservedTime)
	}
	if record.TimestampInferenceReason == "" {
		t.Error("want the reason recorded")
	}
}

func TestAnOptionThatProducesAnInvalidRecordFailsTheTest(t *testing.T) {
	recorder := tb.NewRecorder(t.Name())

	// Moving the record's region away from its authenticated envelope is a
	// regional boundary violation. A builder that returned it anyway would let
	// a scenario assert on a record production would never see.
	fataled := recorder.ExpectFatal(func() {
		builders.NewFactory().Record(recorder, builders.WithRegion("eu-west-1"))
	})

	if !fataled {
		t.Fatal("want an invalid record to fail the test, got no failure")
	}
	failure := recorder.FailureText()
	if !strings.Contains(failure, "region") {
		t.Errorf("want the failure to name the offending field, got:\n%s", failure)
	}
	if !strings.Contains(failure, "AllowInvalid") {
		t.Errorf("want the failure to say how to build a malformed record on purpose, got:\n%s", failure)
	}
}

func TestAllowInvalidBuildsAMalformedRecordOnPurpose(t *testing.T) {
	record := builders.NewFactory().Record(t,
		builders.WithRegion("eu-west-1"),
		builders.AllowInvalid(),
	)

	if record.Region != "eu-west-1" {
		t.Fatalf("want the malformed region kept, got %s", record.Region)
	}
	if record.Validate() == nil {
		t.Fatal("want the record to be invalid, got a valid one")
	}
}

func TestAllowInvalidOnAValidRecordFailsTheTest(t *testing.T) {
	recorder := tb.NewRecorder(t.Name())

	// A scenario that asks for a malformed record and gets a valid one has
	// stopped testing what it claims to, usually because validation moved.
	fataled := recorder.ExpectFatal(func() {
		builders.NewFactory().Record(recorder, builders.AllowInvalid())
	})

	if !fataled {
		t.Fatal("want a stale AllowInvalid to fail the test, got no failure")
	}
}

func TestEnvelopeOptionsApply(t *testing.T) {
	factory := builders.NewFactory()

	envelope := factory.Envelope(t,
		builders.EnvelopeInRegion("eu-west-1"),
		builders.EnvelopeFromSource(model.SourceTypeCloudWatch, "cloudwatch-eu-west-1"),
		builders.EnvelopeAllowingServices("adservice", "cartservice"),
		builders.EnvelopeAllowingEnvironments("production", "staging"),
	)

	if envelope.Region != "eu-west-1" {
		t.Errorf("want region eu-west-1, got %s", envelope.Region)
	}
	if envelope.SourceType != model.SourceTypeCloudWatch {
		t.Errorf("want a CloudWatch envelope, got %s", envelope.SourceType)
	}
	if !envelope.AllowsService("cartservice") || envelope.AllowsService("paymentservice") {
		t.Errorf("want only the listed services allowed, got %v", envelope.AllowedServices)
	}
	// Two allowed environments means the source is not dedicated to one, so it
	// may not supply a default for a missing claim.
	if _, ok := envelope.DefaultEnvironment(); ok {
		t.Error("want no default environment for a multi-environment source")
	}
}

func TestTheDefaultEnvelopeIsDedicatedToOneEnvironment(t *testing.T) {
	envelope := builders.NewFactory().Envelope(t)

	environment, ok := envelope.DefaultEnvironment()
	if !ok {
		t.Fatal("want the default envelope dedicated to one environment")
	}
	if environment != builders.DefaultEnvironment {
		t.Fatalf("want %s, got %s", builders.DefaultEnvironment, environment)
	}
}

func TestAnInvalidEnvelopeFailsTheTest(t *testing.T) {
	recorder := tb.NewRecorder(t.Name())

	fataled := recorder.ExpectFatal(func() {
		builders.NewFactory().Envelope(recorder, builders.EnvelopeAllowingServices())
	})

	if !fataled {
		t.Fatal("want an envelope that allows no service to fail the test, got no failure")
	}
	if failure := recorder.FailureText(); !strings.Contains(failure, "allowed_services") {
		t.Errorf("want the failure to name the offending field, got:\n%s", failure)
	}
}

func TestRecordsCanBeAttributedToAnotherEnvelope(t *testing.T) {
	factory := builders.NewFactory()
	envelope := factory.Envelope(t,
		builders.EnvelopeInRegion("eu-west-1"),
		builders.EnvelopeAllowingServices("adservice"),
	)

	record := factory.Record(t,
		builders.WithSourceEnvelope(envelope),
		builders.WithService("adservice"),
	)

	if record.Region != "eu-west-1" {
		t.Fatalf("want the record to move to the envelope's region, got %s", record.Region)
	}
}
