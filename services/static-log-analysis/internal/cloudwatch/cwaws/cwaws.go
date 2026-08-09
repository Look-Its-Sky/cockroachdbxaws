// Package cwaws implements cloudwatch.LogsAPI against the AWS SDK.
//
// It is a separate package so that internal/cloudwatch, which decides what to
// read and when a checkpoint may move, does not import an AWS client. The two
// fail in different ways and are tested at different boundaries: everything
// here is transport, and everything there is ordering.
package cwaws

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch"
)

// FilterLogEventsAPI is the one operation this adapter needs. Depending on the
// operation rather than on the concrete client keeps the conversion testable
// without a network or a container.
type FilterLogEventsAPI interface {
	FilterLogEvents(ctx context.Context, in *cloudwatchlogs.FilterLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error)
}

// Client is a region-local cloudwatch.LogsAPI.
type Client struct {
	region string
	api    FilterLogEventsAPI
}

var _ cloudwatch.LogsAPI = (*Client)(nil)

// Option configures the AWS client built by New.
type Option func(*settings)

type settings struct {
	// baseEndpoint overrides the regional endpoint. It exists for LocalStack
	// and other local test doubles. In a deployed region the resolved regional
	// endpoint is the only correct one, so this is never set from production
	// configuration.
	baseEndpoint string
	credentials  awssdk.CredentialsProvider
}

// WithBaseEndpoint points the client at a local CloudWatch Logs implementation.
// It is for tests: pointing a regional adapter at another region's endpoint is
// exactly the boundary crossing the adapter refuses.
func WithBaseEndpoint(endpoint string) Option {
	return func(s *settings) { s.baseEndpoint = endpoint }
}

// WithCredentials supplies an explicit credential provider, which a local test
// endpoint needs because it has no instance role to fall back on.
func WithCredentials(provider awssdk.CredentialsProvider) Option {
	return func(s *settings) { s.credentials = provider }
}

// New builds a client bound to one region.
//
// Credentials and endpoint are both region-local. The returned client reports
// its own region so that the adapter can refuse to read through a client built
// for somewhere else.
func New(ctx context.Context, region string, opts ...Option) (*Client, error) {
	if strings.TrimSpace(region) == "" {
		return nil, fmt.Errorf("%w: a regional client needs its region", cloudwatch.ErrInvalidConfig)
	}
	applied := &settings{}
	for _, opt := range opts {
		if opt == nil {
			return nil, fmt.Errorf("%w: a nil option is a configuration mistake", cloudwatch.ErrInvalidConfig)
		}
		opt(applied)
	}
	loadOptions := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if applied.credentials != nil {
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(applied.credentials))
	}
	awsConfig, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("%w: loading regional aws configuration: %v", cloudwatch.ErrInvalidConfig, err)
	}
	// A profile or environment variable can name a different region than the one
	// asked for. Reading another region's logs is a boundary crossing, so it is
	// refused here rather than discovered from the data later.
	if awsConfig.Region != region {
		return nil, fmt.Errorf("%w: the resolved aws configuration is for %q, not %q",
			cloudwatch.ErrRegionalBoundary, awsConfig.Region, region)
	}
	clientOptions := []func(*cloudwatchlogs.Options){}
	if applied.baseEndpoint != "" {
		endpoint := applied.baseEndpoint
		clientOptions = append(clientOptions, func(o *cloudwatchlogs.Options) {
			o.BaseEndpoint = awssdk.String(endpoint)
		})
	}
	return &Client{region: region, api: cloudwatchlogs.NewFromConfig(awsConfig, clientOptions...)}, nil
}

// NewFromAPI wraps an existing operation implementation, for tests that supply
// their own.
func NewFromAPI(region string, api FilterLogEventsAPI) (*Client, error) {
	if strings.TrimSpace(region) == "" || api == nil {
		return nil, fmt.Errorf("%w: a region and an api are both required", cloudwatch.ErrInvalidConfig)
	}
	return &Client{region: region, api: api}, nil
}

// Region reports the region this client's endpoint and credentials belong to.
func (c *Client) Region() string { return c.region }

// FilterEvents reads one page.
//
// An empty page is returned as an empty page. Reporting "nothing new" as an
// error would make the adapter back off against a healthy quiet source and
// would make a real outage indistinguishable from silence.
func (c *Client) FilterEvents(ctx context.Context, query cloudwatch.Query) (cloudwatch.Page, error) {
	if c == nil || c.api == nil {
		return cloudwatch.Page{}, fmt.Errorf("%w: client is not built", cloudwatch.ErrInvalidConfig)
	}
	if strings.TrimSpace(query.LogGroup) == "" {
		return cloudwatch.Page{}, fmt.Errorf("%w: a query needs a log group", cloudwatch.ErrInvalidConfig)
	}
	input := &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName: awssdk.String(query.LogGroup),
		StartTime:    awssdk.Int64(floorMillis(query.Start)),
		// CloudWatch's endTime is inclusive while a cloudwatch.Query is
		// half-open, and both bounds are milliseconds. Rounding the start down
		// and the end up widens the window by under a millisecond at each end.
		// That direction is deliberate: a slightly wide window returns a
		// duplicate, which stable record identity makes harmless, and a
		// slightly narrow one drops an event nothing would ever read again.
		EndTime: awssdk.Int64(ceilMillis(query.End)),
	}
	if query.NextToken != "" {
		input.NextToken = awssdk.String(query.NextToken)
	}
	if query.Limit > 0 {
		input.Limit = awssdk.Int32(int32(query.Limit))
	}

	output, err := c.api.FilterLogEvents(ctx, input)
	if err != nil {
		return cloudwatch.Page{}, classify(err)
	}
	page := cloudwatch.Page{
		Region:   c.region,
		LogGroup: query.LogGroup,
		Events:   make([]cloudwatch.Event, 0, len(output.Events)),
	}
	if output.NextToken != nil {
		page.NextToken = *output.NextToken
	}
	for _, event := range output.Events {
		page.Events = append(page.Events, cloudwatch.Event{
			StreamName:    awssdk.ToString(event.LogStreamName),
			EventID:       awssdk.ToString(event.EventId),
			Timestamp:     fromMillis(event.Timestamp),
			IngestionTime: fromMillis(event.IngestionTime),
			Message:       awssdk.ToString(event.Message),
		})
	}
	return page, nil
}

// classify separates a rate refusal from every other failure.
//
// A permanent failure reported as retryable makes the adapter wait out its
// whole attempt budget on every cycle for something waiting cannot fix. A
// retryable one reported as permanent ends a cycle that a short wait would
// have completed.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var throttling *cwtypes.ThrottlingException
	var limitExceeded *cwtypes.LimitExceededException
	var quotaExceeded *cwtypes.ServiceQuotaExceededException
	if errors.As(err, &throttling) || errors.As(err, &limitExceeded) || errors.As(err, &quotaExceeded) {
		return fmt.Errorf("%w: %v", cloudwatch.ErrThrottled, err)
	}
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) &&
		(response.HTTPStatusCode() == http.StatusTooManyRequests ||
			response.HTTPStatusCode() == http.StatusServiceUnavailable) {
		return fmt.Errorf("%w: %v", cloudwatch.ErrThrottled, err)
	}
	return fmt.Errorf("%w: %v", cloudwatch.ErrSourceUnavailable, err)
}

func floorMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	nanos := t.UnixNano()
	millis := nanos / int64(time.Millisecond)
	if nanos < 0 && nanos%int64(time.Millisecond) != 0 {
		millis--
	}
	return millis
}

func ceilMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	nanos := t.UnixNano()
	millis := nanos / int64(time.Millisecond)
	if nanos > 0 && nanos%int64(time.Millisecond) != 0 {
		millis++
	}
	return millis
}

func fromMillis(value *int64) time.Time {
	if value == nil || *value == 0 {
		return time.Time{}
	}
	return time.UnixMilli(*value).UTC()
}
