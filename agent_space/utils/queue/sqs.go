package queue

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// the three operations the worker needs. Narrow on purpose: the worker's tests
// implement this in a dozen lines and never reach the network.
type Receiver interface {
	// Receive long-polls for work. An empty slice and a nil error is the
	// normal idle case, not a failure.
	Receive(ctx context.Context) ([]Message, error)
	// Delete acknowledges a message. Anything not deleted is redelivered.
	Delete(ctx context.Context, receiptHandle string) error
	// ExtendVisibility renews the lease on an in-flight message.
	ExtendVisibility(ctx context.Context, receiptHandle string, seconds int32) error
}

// the SQS-backed Receiver
type Client struct {
	sqs *sqs.Client
	cfg Config
}

var _ Receiver = (*Client)(nil)

// a client from the standard AWS credential chain; Config.Endpoint repoints it
// at LocalStack, and nothing else here knows the difference
func New(ctx context.Context, cfg Config) (*Client, error) {
	if !cfg.Configured() {
		return nil, fmt.Errorf("queue: SQS_QUEUE_URL is not set")
	}
	cfg = cfg.normalised()

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("queue: load AWS config: %w", err)
	}

	opts := []func(*sqs.Options){}
	if cfg.Endpoint != "" {
		opts = append(opts, func(o *sqs.Options) { o.BaseEndpoint = aws.String(cfg.Endpoint) })
	}

	return &Client{sqs: sqs.NewFromConfig(awsCfg, opts...), cfg: cfg}, nil
}

// Endpoint describes where this client is pointed, for the boot log.
func (c *Client) Endpoint() string {
	if c.cfg.Endpoint != "" {
		return c.cfg.Endpoint + " (" + c.cfg.QueueURL + ")"
	}
	return c.cfg.QueueURL
}

// one message at a time: an investigation costs real tokens, and the worker is
// deliberately serial.
func (c *Client) Receive(ctx context.Context) ([]Message, error) {
	out, err := c.sqs.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(c.cfg.QueueURL),
		MaxNumberOfMessages: 1,
		WaitTimeSeconds:     int32(c.cfg.WaitTime.Seconds()),
		VisibilityTimeout:   int32(c.cfg.VisibilityTimeout.Seconds()),
		// the receive count decides when to give up on a poison message
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{
			types.MessageSystemAttributeNameApproximateReceiveCount,
		},
		MessageAttributeNames: []string{"All"},
	})
	if err != nil {
		return nil, fmt.Errorf("queue: receive: %w", err)
	}

	msgs := make([]Message, 0, len(out.Messages))
	for _, m := range out.Messages {
		msgs = append(msgs, Message{
			MessageID:         aws.ToString(m.MessageId),
			ReceiptHandle:     aws.ToString(m.ReceiptHandle),
			Body:              aws.ToString(m.Body),
			Attributes:        m.Attributes,
			MessageAttributes: stringAttributes(m.MessageAttributes),
		})
	}
	return msgs, nil
}

func (c *Client) Delete(ctx context.Context, receiptHandle string) error {
	_, err := c.sqs.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.cfg.QueueURL),
		ReceiptHandle: aws.String(receiptHandle),
	})
	if err != nil {
		return fmt.Errorf("queue: delete: %w", err)
	}
	return nil
}

func (c *Client) ExtendVisibility(ctx context.Context, receiptHandle string, seconds int32) error {
	_, err := c.sqs.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(c.cfg.QueueURL),
		ReceiptHandle:     aws.String(receiptHandle),
		VisibilityTimeout: seconds,
	})
	if err != nil {
		return fmt.Errorf("queue: extend visibility: %w", err)
	}
	return nil
}

// keep the string-typed attributes and drop binary ones, which no producer
// here sends and the worker could not read anyway
func stringAttributes(in map[string]types.MessageAttributeValue) map[string]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]string, len(in))
	for k, v := range in {
		if v.StringValue != nil {
			out[k] = *v.StringValue
		}
	}
	return out
}
