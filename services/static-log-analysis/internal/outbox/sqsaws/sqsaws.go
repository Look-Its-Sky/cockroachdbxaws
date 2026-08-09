// Package sqsaws implements outbox.Transport against Amazon SQS Standard.
//
// It is a separate package so that internal/outbox, which decides what each
// delivery outcome earns a message, does not import an AWS client. The two fail
// in different ways and are tested at different boundaries: everything here is
// transport and classification, and everything there is recovery policy.
//
// ADR 0005 chooses SQS Standard, so delivery is at-least-once and unordered.
// Nothing here tries to hide that: the deduplication key travels as a message
// attribute and the consumer deduplicates, which is what makes a republished
// message harmless.
package sqsaws

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/outbox"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/queue"
)

// AttributeFailureReason carries the categorical reason a message could not be
// published. It is the only thing this package adds to a dead-lettered message:
// the body is unchanged, so a replay from the dead-letter queue is a replay of
// exactly what was committed.
const AttributeFailureReason = "failure_reason"

// SendMessageAPI is the one operation this adapter needs. Depending on the
// operation rather than the concrete client keeps the mapping and the
// classification testable without a network or a container.
type SendMessageAPI interface {
	SendMessage(ctx context.Context, in *sqs.SendMessageInput, opts ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

// Config names the region and the two queues this transport publishes to.
type Config struct {
	Region             string
	QueueURL           string
	DeadLetterQueueURL string
	// BaseEndpoint overrides the resolved regional endpoint. It exists for
	// LocalStack and other SQS-compatible test endpoints; in a deployed region
	// the resolved regional endpoint is the only correct one.
	BaseEndpoint string
	// Credentials supplies an explicit provider, which a local endpoint needs
	// because it has no instance role to fall back on.
	Credentials awssdk.CredentialsProvider
}

// Client is a region-local outbox.Transport.
type Client struct {
	config Config
	api    SendMessageAPI
}

var _ outbox.Transport = (*Client)(nil)

// New builds a client bound to one region.
func New(ctx context.Context, config Config) (*Client, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	loadOptions := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(config.Region)}
	if config.Credentials != nil {
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(config.Credentials))
	}
	awsConfig, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("%w: loading regional aws configuration: %v", outbox.ErrInvalidConfig, err)
	}
	// A profile or environment variable can name a different region than the one
	// asked for. Publishing a regional assignment through another region's
	// endpoint is the boundary crossing security.md forbids, so it is refused
	// here rather than discovered per message.
	if awsConfig.Region != config.Region {
		return nil, fmt.Errorf("%w: the resolved aws configuration is for %q, not %q",
			outbox.ErrInvalidConfig, awsConfig.Region, config.Region)
	}
	clientOptions := []func(*sqs.Options){}
	if config.BaseEndpoint != "" {
		endpoint := config.BaseEndpoint
		clientOptions = append(clientOptions, func(o *sqs.Options) { o.BaseEndpoint = awssdk.String(endpoint) })
	}
	return &Client{config: config, api: sqs.NewFromConfig(awsConfig, clientOptions...)}, nil
}

// NewFromAPI wraps an existing operation implementation, for tests that supply
// their own.
func NewFromAPI(config Config, api SendMessageAPI) (*Client, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if api == nil {
		return nil, fmt.Errorf("%w: a transport needs an api", outbox.ErrInvalidConfig)
	}
	return &Client{config: config, api: api}, nil
}

func (c Config) validate() error {
	if strings.TrimSpace(c.Region) == "" {
		return fmt.Errorf("%w: a regional transport needs its region", outbox.ErrInvalidConfig)
	}
	if strings.TrimSpace(c.QueueURL) == "" || strings.TrimSpace(c.DeadLetterQueueURL) == "" {
		return fmt.Errorf("%w: both a queue and a dead-letter queue are required; without the second a message that can never be published would be retried forever",
			outbox.ErrInvalidConfig)
	}
	if c.QueueURL == c.DeadLetterQueueURL {
		return fmt.Errorf("%w: the queue and the dead-letter queue are the same, so a poisoned message would be republished to itself",
			outbox.ErrInvalidConfig)
	}
	return nil
}

// Region reports the region this client's endpoint and credentials belong to.
func (c *Client) Region() string { return c.config.Region }

// Publish delivers one message to the agent queue.
func (c *Client) Publish(ctx context.Context, message queue.Message) error {
	return c.send(ctx, c.config.QueueURL, message, nil)
}

// DeadLetter delivers one undeliverable message to the dead-letter queue with
// the categorical reason it could not be published.
func (c *Client) DeadLetter(ctx context.Context, message queue.Message, reason outbox.Reason) error {
	if !reason.Valid() {
		// This value is written to a queue an operator reads. A caller bug must
		// not be able to put arbitrary, possibly record-derived, text there, and
		// no retry turns an unknown reason into a known one.
		return outbox.NewFailure(outbox.ErrMessageRejected, outbox.ReasonRejected, "unknown_reason")
	}
	return c.send(ctx, c.config.DeadLetterQueueURL, message, map[string]string{
		AttributeFailureReason: string(reason),
	})
}

func (c *Client) send(ctx context.Context, queueURL string, message queue.Message, extra map[string]string) error {
	if c == nil || c.api == nil {
		return outbox.NewFailure(outbox.ErrDeploymentFault, outbox.ReasonDeploymentFault, "client_not_built")
	}
	// The transport must never be the first place a structural problem is
	// noticed: the store validates the same thing before it hands out a claim,
	// so reaching this means the message could never have been published and no
	// retry changes that.
	if err := message.Validate(); err != nil {
		return outbox.NewFailure(outbox.ErrMessageRejected, outbox.ReasonRejected, "invalid_message")
	}
	attributes := map[string]sqstypes.MessageAttributeValue{}
	for name, value := range message.SQSAttributes() {
		attributes[name] = stringAttribute(value)
	}
	for name, value := range extra {
		attributes[name] = stringAttribute(value)
	}
	_, err := c.api.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:          awssdk.String(queueURL),
		MessageBody:       awssdk.String(string(message.Body)),
		MessageAttributes: attributes,
	})
	if err != nil {
		return classify(err)
	}
	return nil
}

func stringAttribute(value string) sqstypes.MessageAttributeValue {
	return sqstypes.MessageAttributeValue{
		DataType:    awssdk.String(queue.SQSStringAttributeDataType),
		StringValue: awssdk.String(value),
	}
}

// classify maps an SQS failure onto the three classes outbox recovers by.
//
// The default is deliberately retryable. An unrecognized failure is a gap in
// this function, not a verdict about the message, and calling it permanent
// would move a committed assignment to the dead-letter queue on the strength of
// a bug. The cost of the opposite mistake is bounded by the publisher's attempt
// limit, which dead-letters a message that has exhausted its whole schedule.
func classify(err error) error {
	if err == nil {
		return nil
	}
	switch {
	// The message itself can never be accepted, however often it is offered.
	// This is the only class that may reach the dead-letter path.
	case as[*sqstypes.InvalidMessageContents](err),
		as[*sqstypes.BatchRequestTooLong](err),
		as[*sqstypes.InvalidAttributeName](err),
		as[*sqstypes.InvalidAttributeValue](err),
		as[*sqstypes.InvalidIdFormat](err):
		return outbox.NewFailure(outbox.ErrMessageRejected, outbox.ReasonRejected, errorCode(err))

	// Nothing about any single message caused this and nothing about any single
	// message can fix it. Messages keep their claims and the operator is told.
	case as[*sqstypes.QueueDoesNotExist](err),
		as[*sqstypes.QueueDeletedRecently](err),
		as[*sqstypes.InvalidAddress](err),
		as[*sqstypes.InvalidSecurity](err),
		as[*sqstypes.ResourceNotFoundException](err),
		as[*sqstypes.UnsupportedOperation](err),
		as[*sqstypes.KmsAccessDenied](err),
		as[*sqstypes.KmsDisabled](err),
		as[*sqstypes.KmsInvalidState](err),
		as[*sqstypes.KmsNotFound](err),
		as[*sqstypes.KmsOptInRequired](err),
		as[*sqstypes.KmsInvalidKeyUsage](err):
		return outbox.NewFailure(outbox.ErrDeploymentFault, outbox.ReasonDeploymentFault, errorCode(err))

	case as[*sqstypes.RequestThrottled](err),
		as[*sqstypes.KmsThrottled](err),
		as[*sqstypes.OverLimit](err):
		return outbox.NewFailure(outbox.ErrRetryable, outbox.ReasonQueueUnavailable, errorCode(err))

	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return outbox.NewFailure(outbox.ErrRetryable, outbox.ReasonTimeout, "")
	}

	var response *smithyhttp.ResponseError
	if errors.As(err, &response) {
		switch status := response.HTTPStatusCode(); {
		case status == http.StatusForbidden, status == http.StatusUnauthorized:
			// A credential that may not write to this queue is a deployment
			// fault: no message can fix it and retrying it forever would hide it.
			return outbox.NewFailure(outbox.ErrDeploymentFault, outbox.ReasonDeploymentFault, "http_"+strconv.Itoa(status))
		case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests, status >= 500:
			return outbox.NewFailure(outbox.ErrRetryable, outbox.ReasonQueueUnavailable, "http_"+strconv.Itoa(status))
		case status >= 400:
			// A 4xx that is not one of the modelled shapes above is a request
			// this service composed and the queue refused. It is reported as a
			// deployment fault rather than dead-lettered, because the modelled
			// message-local refusals are enumerated above and anything left is
			// more likely to be the request than the payload.
			return outbox.NewFailure(outbox.ErrDeploymentFault, outbox.ReasonDeploymentFault, "http_"+strconv.Itoa(status))
		}
	}
	return outbox.NewFailure(outbox.ErrRetryable, outbox.ReasonQueueUnavailable, "")
}

// as reports whether err unwraps to T. It exists so the classification above
// reads as a table rather than as twenty variable declarations.
func as[T error](err error) bool {
	var target T
	return errors.As(err, &target)
}

// errorCode returns the dependency's own short code, which is safe to record:
// it is a modelled API error name, never a message this service composed from
// record content. Anything longer or oddly shaped is dropped by outbox.
func errorCode(err error) string {
	var api interface{ ErrorCode() string }
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}
