package sqsaws_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/outbox"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/outbox/sqsaws"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/queue"
)

const (
	region   = "us-east-1"
	queueURL = "https://sqs.us-east-1.amazonaws.com/000000000000/agent-assignments"
	dlqURL   = "https://sqs.us-east-1.amazonaws.com/000000000000/agent-assignments-dlq"
)

// fakeSQS answers one SendMessage however a test asks and records what it was
// given. What SQS accepts is a property of SQS and is covered against a real
// endpoint; what this pins is the mapping and the classification.
type fakeSQS struct {
	err   error
	calls []*sqs.SendMessageInput
}

func (f *fakeSQS) SendMessage(_ context.Context, in *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	f.calls = append(f.calls, in)
	if f.err != nil {
		return nil, f.err
	}
	return &sqs.SendMessageOutput{MessageId: awssdk.String("sqs-assigned-id")}, nil
}

func newClient(t *testing.T, api *fakeSQS) *sqsaws.Client {
	t.Helper()
	client, err := sqsaws.NewFromAPI(sqsaws.Config{
		Region: region, QueueURL: queueURL, DeadLetterQueueURL: dlqURL,
	}, api)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// validMessage is a publishable assignment. It deliberately goes through
// queue.Message.Validate in the tests that need it, because the transport must
// never be the first place a structural problem is noticed.
func validMessage() queue.Message {
	return queue.Message{
		MessageID:        "0192f0a0-0000-7000-8000-000000000001",
		DeduplicationKey: "dedup-key-1",
		Type:             "agent.assignment.v1",
		Body:             []byte(`{"schema_version":"1.0"}`),
		Attributes:       map[string]string{"region": region},
	}
}

func TestPublishMapsTheMessageOntoTheConfiguredQueue(t *testing.T) {
	api := &fakeSQS{}
	client := newClient(t, api)
	message := validMessage()

	if err := client.Publish(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if len(api.calls) != 1 {
		t.Fatalf("expected one SendMessage, got %d", len(api.calls))
	}
	call := api.calls[0]
	if awssdk.ToString(call.QueueUrl) != queueURL {
		t.Fatalf("published to %q, not the configured queue", awssdk.ToString(call.QueueUrl))
	}
	if awssdk.ToString(call.MessageBody) != string(message.Body) {
		t.Fatal("the published body is not the committed body; a replay must be a replay of exactly what was committed")
	}
	// Every mapped attribute is a String attribute; nothing binary and nothing
	// log-derived travels here.
	for name, attribute := range call.MessageAttributes {
		if awssdk.ToString(attribute.DataType) != queue.SQSStringAttributeDataType {
			t.Fatalf("attribute %q has data type %q", name, awssdk.ToString(attribute.DataType))
		}
	}
	for name, want := range message.SQSAttributes() {
		got, ok := call.MessageAttributes[name]
		if !ok {
			t.Fatalf("attribute %q was not published", name)
		}
		if awssdk.ToString(got.StringValue) != want {
			t.Fatalf("attribute %q published as %q, want %q", name, awssdk.ToString(got.StringValue), want)
		}
	}
}

func TestDeadLetterGoesToTheDeadLetterQueueWithItsCategoricalReason(t *testing.T) {
	api := &fakeSQS{}
	client := newClient(t, api)
	message := validMessage()

	if err := client.DeadLetter(context.Background(), message, outbox.ReasonRejected); err != nil {
		t.Fatal(err)
	}
	call := api.calls[0]
	if awssdk.ToString(call.QueueUrl) != dlqURL {
		t.Fatalf("dead-lettered to %q, not the dead-letter queue", awssdk.ToString(call.QueueUrl))
	}
	if awssdk.ToString(call.MessageBody) != string(message.Body) {
		t.Fatal("the dead-lettered body was altered; a replay from the dead-letter queue must be a replay of what was committed")
	}
	reason, ok := call.MessageAttributes[sqsaws.AttributeFailureReason]
	if !ok {
		t.Fatalf("no %s attribute; an operator cannot tell why it is here", sqsaws.AttributeFailureReason)
	}
	if awssdk.ToString(reason.StringValue) != string(outbox.ReasonRejected) {
		t.Fatalf("reason published as %q", awssdk.ToString(reason.StringValue))
	}
}

// TestADeadLetterReasonOutsideTheClosedSetIsNeverPublished pins that the
// attribute cannot carry arbitrary text. It is written to a queue an operator
// reads, so a caller bug must not be able to put record-derived content there.
func TestADeadLetterReasonOutsideTheClosedSetIsNeverPublished(t *testing.T) {
	api := &fakeSQS{}
	client := newClient(t, api)

	err := client.DeadLetter(context.Background(), validMessage(), outbox.Reason("card number 4111111111111111"))
	if err == nil {
		t.Fatal("an unknown reason was accepted")
	}
	if !errors.Is(err, outbox.ErrMessageRejected) {
		t.Fatalf("an unknown reason was classified as %v; it is a caller defect, not a transport failure", err)
	}
	if len(api.calls) != 0 {
		t.Fatal("an unknown reason reached the network")
	}
}

func TestAStructurallyInvalidMessageIsRejectedBeforeAnyNetworkCall(t *testing.T) {
	api := &fakeSQS{}
	client := newClient(t, api)
	message := validMessage()
	message.Body = nil

	err := client.Publish(context.Background(), message)
	if !errors.Is(err, outbox.ErrMessageRejected) {
		t.Fatalf("an unpublishable message was classified as %v; no retry makes an empty body publishable", err)
	}
	if len(api.calls) != 0 {
		t.Fatal("an unpublishable message reached the network")
	}
}

// TestEveryTransportFailureLandsInTheClassItsRecoveryNeeds is the heart of this
// adapter. A permanent failure reported as retryable makes the publisher replay
// a poisoned message forever and starve its siblings; a retryable one reported
// as permanent moves a committed assignment to the dead-letter queue that a
// second attempt would have delivered.
func TestEveryTransportFailureLandsInTheClassItsRecoveryNeeds(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		err   error
		class error
	}{
		{"throttled", &sqstypes.RequestThrottled{}, outbox.ErrRetryable},
		{"kms throttled", &sqstypes.KmsThrottled{}, outbox.ErrRetryable},
		{"over limit", &sqstypes.OverLimit{}, outbox.ErrRetryable},
		{"service unavailable", responseError(http.StatusServiceUnavailable), outbox.ErrRetryable},
		{"internal error", responseError(http.StatusInternalServerError), outbox.ErrRetryable},
		{"too many requests", responseError(http.StatusTooManyRequests), outbox.ErrRetryable},
		{"unrecognized", errors.New("some new failure nobody has classified"), outbox.ErrRetryable},

		{"queue missing", &sqstypes.QueueDoesNotExist{}, outbox.ErrDeploymentFault},
		{"queue deleted recently", &sqstypes.QueueDeletedRecently{}, outbox.ErrDeploymentFault},
		{"invalid address", &sqstypes.InvalidAddress{}, outbox.ErrDeploymentFault},
		{"invalid security", &sqstypes.InvalidSecurity{}, outbox.ErrDeploymentFault},
		{"kms access denied", &sqstypes.KmsAccessDenied{}, outbox.ErrDeploymentFault},
		{"kms disabled", &sqstypes.KmsDisabled{}, outbox.ErrDeploymentFault},
		{"forbidden", responseError(http.StatusForbidden), outbox.ErrDeploymentFault},
		{"unauthorized", responseError(http.StatusUnauthorized), outbox.ErrDeploymentFault},

		{"invalid contents", &sqstypes.InvalidMessageContents{}, outbox.ErrMessageRejected},
		{"too long", &sqstypes.BatchRequestTooLong{}, outbox.ErrMessageRejected},
		{"invalid attribute name", &sqstypes.InvalidAttributeName{}, outbox.ErrMessageRejected},
		{"invalid attribute value", &sqstypes.InvalidAttributeValue{}, outbox.ErrMessageRejected},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			api := &fakeSQS{err: testCase.err}
			client := newClient(t, api)
			err := client.Publish(context.Background(), validMessage())
			if !errors.Is(err, testCase.class) {
				t.Fatalf("%v was classified as %v, want %v", testCase.err, err, testCase.class)
			}
		})
	}
}

// TestAFailureCarriesACategoricalReasonAndNoTransportText pins that nothing the
// dependency said reaches the database or a dead-letter attribute verbatim.
func TestAFailureCarriesACategoricalReasonAndNoTransportText(t *testing.T) {
	api := &fakeSQS{err: errors.New("connection to 10.0.0.5 failed while sending card 4111111111111111")}
	client := newClient(t, api)

	err := client.Publish(context.Background(), validMessage())
	var failure *outbox.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("failure %v does not carry a classified reason", err)
	}
	if !failure.Reason.Valid() {
		t.Fatalf("reason %q is outside the closed set", failure.Reason)
	}
	if strings.Contains(failure.Detail, "4111111111111111") || strings.Contains(failure.Detail, "10.0.0.5") {
		t.Fatalf("detail %q echoed dependency text", failure.Detail)
	}
}

func TestRegionIsTheOneTheClientWasBuiltFor(t *testing.T) {
	if got := newClient(t, &fakeSQS{}).Region(); got != region {
		t.Fatalf("Region() reported %q", got)
	}
}

func TestNewRefusesAClientThatCouldNotPublishSafely(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		config sqsaws.Config
	}{
		{"no region", sqsaws.Config{QueueURL: queueURL, DeadLetterQueueURL: dlqURL}},
		{"no queue", sqsaws.Config{Region: region, DeadLetterQueueURL: dlqURL}},
		{"no dead-letter queue", sqsaws.Config{Region: region, QueueURL: queueURL}},
		{"same queue twice", sqsaws.Config{Region: region, QueueURL: queueURL, DeadLetterQueueURL: queueURL}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := sqsaws.NewFromAPI(testCase.config, &fakeSQS{}); err == nil {
				t.Fatal("a client that could not publish safely was built")
			}
		})
	}
}

func responseError(status int) error {
	return &smithyhttp.ResponseError{Response: &smithyhttp.Response{
		Response: &http.Response{StatusCode: status}}, Err: errors.New("http error")}
}
