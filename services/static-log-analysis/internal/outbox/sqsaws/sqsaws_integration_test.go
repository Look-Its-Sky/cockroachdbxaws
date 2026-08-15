//go:build integration

package sqsaws_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/outbox"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/outbox/sqsaws"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/queue"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/localstacktest"
)

// These tests run against a real SQS API because what they are about is SQS's:
// which attribute shapes it accepts, what it does with a body it will not take,
// and what a missing queue actually returns. A hand-written double would agree
// with whatever this adapter expected, which is the one thing that would make
// the classification table worthless.

func TestLivePublishPutsExactlyTheCommittedBodyAndItsMetadataOnTheQueue(t *testing.T) {
	admin := localstacktest.SQSAdmin(t)
	main := localstacktest.Queue(t, admin, "main")
	dead := localstacktest.Queue(t, admin, "dlq")
	transport := localstacktest.Transport(t, main, dead)

	message := liveMessage()
	if err := transport.Publish(context.Background(), message); err != nil {
		t.Fatal(err)
	}

	received := localstacktest.Drain(t, admin, main, 1)
	if len(received) != 1 {
		t.Fatalf("expected one message on the queue, got %d", len(received))
	}
	got := received[0]
	if awssdk.ToString(got.Body) != string(message.Body) {
		t.Fatalf("body arrived as %q, want %q", awssdk.ToString(got.Body), message.Body)
	}
	// SQS Standard has no native deduplication key or application message type,
	// so a consumer can only identify a message by these attributes. If they do
	// not survive the round trip, at-least-once delivery stops being safe.
	for name, want := range message.SQSAttributes() {
		attribute, ok := got.MessageAttributes[name]
		if !ok {
			t.Fatalf("attribute %q did not survive the round trip", name)
		}
		if awssdk.ToString(attribute.StringValue) != want {
			t.Fatalf("attribute %q arrived as %q, want %q", name, awssdk.ToString(attribute.StringValue), want)
		}
	}
	if len(localstacktest.Drain(t, admin, dead, 0)) != 0 {
		t.Fatal("a successful publish also reached the dead-letter queue")
	}
}

func TestLiveDeadLetterReachesTheOtherQueueCarryingItsReason(t *testing.T) {
	admin := localstacktest.SQSAdmin(t)
	main := localstacktest.Queue(t, admin, "main")
	dead := localstacktest.Queue(t, admin, "dlq")
	transport := localstacktest.Transport(t, main, dead)

	message := liveMessage()
	if err := transport.DeadLetter(context.Background(), message, outbox.ReasonAttemptsExhausted); err != nil {
		t.Fatal(err)
	}

	received := localstacktest.Drain(t, admin, dead, 1)
	if len(received) != 1 {
		t.Fatalf("expected one message on the dead-letter queue, got %d", len(received))
	}
	if body := awssdk.ToString(received[0].Body); body != string(message.Body) {
		t.Fatalf("the dead-lettered body was altered: %q", body)
	}
	reason, ok := received[0].MessageAttributes[sqsaws.AttributeFailureReason]
	if !ok {
		t.Fatal("the dead-lettered message carries no reason an operator can act on")
	}
	if awssdk.ToString(reason.StringValue) != string(outbox.ReasonAttemptsExhausted) {
		t.Fatalf("reason arrived as %q", awssdk.ToString(reason.StringValue))
	}
	if len(localstacktest.Drain(t, admin, main, 0)) != 0 {
		t.Fatal("a dead-lettered message was also published to the main queue")
	}
}

// TestLiveAMissingQueueIsADeploymentFaultAndNeverADeadLetter is the
// classification that matters most in production. Dead-lettering here would
// move a committed assignment into a queue that may not exist either, on the
// strength of a configuration mistake no message caused.
func TestLiveAMissingQueueIsADeploymentFaultAndNeverADeadLetter(t *testing.T) {
	admin := localstacktest.SQSAdmin(t)
	main := localstacktest.Queue(t, admin, "main")
	dead := localstacktest.Queue(t, admin, "dlq")
	// A well-formed URL inside the same account that names a queue nobody made.
	missing := main + "-does-not-exist"
	transport := localstacktest.Transport(t, missing, dead)

	err := transport.Publish(context.Background(), liveMessage())
	if err == nil {
		t.Fatal("publishing to a queue that does not exist succeeded")
	}
	if !errors.Is(err, outbox.ErrDeploymentFault) {
		t.Fatalf("a missing queue was classified as %v; it is not record-local and must not be dead-lettered", err)
	}
	if errors.Is(err, outbox.ErrMessageRejected) {
		t.Fatal("a missing queue was reported as a permanently rejected message")
	}
}

// TestLiveARealFailureCarriesNoTransportTextIntoDurableState pins that whatever
// SQS says about a failure does not become the detail recorded on an outbox row
// or attached to a dead-lettered message.
func TestLiveARealFailureCarriesNoTransportTextIntoDurableState(t *testing.T) {
	admin := localstacktest.SQSAdmin(t)
	main := localstacktest.Queue(t, admin, "main")
	transport := localstacktest.Transport(t, main+"-does-not-exist", main)

	err := transport.Publish(context.Background(), liveMessage())
	var failure *outbox.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("a live failure did not carry a classified reason: %v", err)
	}
	if !failure.Reason.Valid() {
		t.Fatalf("reason %q is outside the closed set", failure.Reason)
	}
	// Detail is either empty or a short identifier-shaped API error code. It is
	// never a sentence, a URL, or anything containing the queue name.
	if strings.ContainsAny(failure.Detail, " /:") {
		t.Fatalf("detail %q carries transport text", failure.Detail)
	}
}

func liveMessage() queue.Message {
	return queue.Message{
		MessageID:        "0192f0a0-0000-7000-8000-00000000beef",
		DeduplicationKey: "live-dedup-key",
		Type:             "agent.assignment.v1",
		Body:             []byte(`{"schema_version":"1.0","investigation":"live"}`),
		Attributes:       map[string]string{"region": localstacktest.Region},
	}
}
