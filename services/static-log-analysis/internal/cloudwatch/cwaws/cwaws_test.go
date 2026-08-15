package cwaws_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch/cwaws"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
)

type stubAPI struct {
	inputs []*cloudwatchlogs.FilterLogEventsInput
	output *cloudwatchlogs.FilterLogEventsOutput
	err    error
}

func (s *stubAPI) FilterLogEvents(_ context.Context, in *cloudwatchlogs.FilterLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	s.inputs = append(s.inputs, in)
	if s.err != nil {
		return nil, s.err
	}
	if s.output != nil {
		return s.output, nil
	}
	return &cloudwatchlogs.FilterLogEventsOutput{}, nil
}

func newClient(t *testing.T, api cwaws.FilterLogEventsAPI) *cwaws.Client {
	t.Helper()
	client, err := cwaws.NewFromAPI("us-east-1", api)
	if err != nil {
		t.Fatalf("building a client: %v", err)
	}
	return client
}

func TestAFilteredLogEventBecomesANativeEvent(t *testing.T) {
	occurred := fakeclock.Origin
	ingested := fakeclock.Origin.Add(90 * time.Second)
	api := &stubAPI{output: &cloudwatchlogs.FilterLogEventsOutput{
		Events: []cwtypes.FilteredLogEvent{{
			EventId:       awssdk.String("39518522779705548990893699571816765783291046441449848832"),
			LogStreamName: awssdk.String("ecs/paymentservice/0e1f"),
			Message:       awssdk.String(`{"level":"error","msg":"charge failed"}`),
			Timestamp:     awssdk.Int64(occurred.UnixMilli()),
			IngestionTime: awssdk.Int64(ingested.UnixMilli()),
		}},
		NextToken: awssdk.String("token-1"),
	}}

	page, err := newClient(t, api).FilterEvents(context.Background(), cloudwatch.Query{
		LogGroup: "/aws/ecs/paymentservice", Start: occurred, End: ingested,
	})
	if err != nil {
		t.Fatalf("filtering: %v", err)
	}

	if page.Region != "us-east-1" || page.LogGroup != "/aws/ecs/paymentservice" {
		t.Fatalf("a page must say which boundary it came from, got %+v", page)
	}
	if len(page.Events) != 1 {
		t.Fatalf("want one event, got %d", len(page.Events))
	}
	event := page.Events[0]
	if event.EventID != "39518522779705548990893699571816765783291046441449848832" {
		t.Fatalf("want the native event id preserved, got %q", event.EventID)
	}
	if event.StreamName != "ecs/paymentservice/0e1f" {
		t.Fatalf("want the stream name, got %q", event.StreamName)
	}
	if !event.Timestamp.Equal(occurred) || !event.IngestionTime.Equal(ingested) {
		t.Fatalf("want both times mapped, got %s and %s", event.Timestamp, event.IngestionTime)
	}
	if page.NextToken != "token-1" {
		t.Fatalf("want pagination to continue, got %q", page.NextToken)
	}
}

// CloudWatch works in milliseconds and its end bound is inclusive, while a
// cloudwatch.Query is half-open nanoseconds. Rounding must widen the window,
// never narrow it: a wide window returns a duplicate that stable identity makes
// harmless, and a narrow one drops an event nothing will ever read again.
func TestTheMillisecondWindowIsWidenedRatherThanNarrowed(t *testing.T) {
	api := &stubAPI{}
	start := time.UnixMilli(1_700_000_000_000).UTC().Add(400 * time.Microsecond)
	end := time.UnixMilli(1_700_000_060_000).UTC().Add(600 * time.Microsecond)

	if _, err := newClient(t, api).FilterEvents(context.Background(), cloudwatch.Query{
		LogGroup: "/aws/ecs/paymentservice", Start: start, End: end,
	}); err != nil {
		t.Fatalf("filtering: %v", err)
	}

	input := api.inputs[0]
	if got, want := *input.StartTime, int64(1_700_000_000_000); got != want {
		t.Fatalf("want the start rounded down to %d, got %d", want, got)
	}
	if got, want := *input.EndTime, int64(1_700_000_060_001); got != want {
		t.Fatalf("want the end rounded up to %d, got %d", want, got)
	}
}

func TestAnEmptyAnswerIsAnEmptyPageAndNotAnError(t *testing.T) {
	page, err := newClient(t, &stubAPI{}).FilterEvents(context.Background(), cloudwatch.Query{
		LogGroup: "/aws/ecs/paymentservice", Start: fakeclock.Origin, End: fakeclock.Origin.Add(time.Minute),
	})

	if err != nil {
		t.Fatalf("a quiet log group is not a failure, got %v", err)
	}
	if len(page.Events) != 0 || page.NextToken != "" {
		t.Fatalf("want an empty finished page, got %+v", page)
	}
}

// A permanent failure reported as retryable makes every cycle spend its whole
// attempt budget on something waiting cannot fix. The reverse ends a cycle a
// short wait would have completed.
func TestThrottlingIsSeparatedFromEveryOtherFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"throttling exception", &cwtypes.ThrottlingException{}, cloudwatch.ErrThrottled},
		{"limit exceeded", &cwtypes.LimitExceededException{}, cloudwatch.ErrThrottled},
		{"service quota exceeded", &cwtypes.ServiceQuotaExceededException{}, cloudwatch.ErrThrottled},
		{"http 429", &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusTooManyRequests}},
			Err:      errors.New("too many requests"),
		}, cloudwatch.ErrThrottled},
		{"http 503", &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusServiceUnavailable}},
			Err:      errors.New("unavailable"),
		}, cloudwatch.ErrThrottled},
		{"access denied", &cwtypes.AccessDeniedException{}, cloudwatch.ErrSourceUnavailable},
		{"log group missing", &cwtypes.ResourceNotFoundException{}, cloudwatch.ErrSourceUnavailable},
		{"invalid parameter", &cwtypes.InvalidParameterException{}, cloudwatch.ErrSourceUnavailable},
		{"transport failure", errors.New("connection reset"), cloudwatch.ErrSourceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newClient(t, &stubAPI{err: test.err}).FilterEvents(context.Background(), cloudwatch.Query{
				LogGroup: "/aws/ecs/paymentservice", Start: fakeclock.Origin, End: fakeclock.Origin.Add(time.Minute),
			})

			if !errors.Is(err, test.want) {
				t.Fatalf("want %v, got %v", test.want, err)
			}
			if test.want == cloudwatch.ErrThrottled && errors.Is(err, cloudwatch.ErrSourceUnavailable) {
				t.Fatal("a rate refusal must not also read as an outage")
			}
			if test.want == cloudwatch.ErrSourceUnavailable && errors.Is(err, cloudwatch.ErrThrottled) {
				t.Fatal("an outage must not be waited out as if it were a rate refusal")
			}
		})
	}
}

func TestAClientNeedsARegionAndAnAPI(t *testing.T) {
	if _, err := cwaws.NewFromAPI("", &stubAPI{}); err == nil {
		t.Fatal("want a refusal without a region")
	}
	if _, err := cwaws.NewFromAPI("us-east-1", nil); err == nil {
		t.Fatal("want a refusal without an api")
	}
}

func TestAQueryWithoutALogGroupIsRefusedBeforeTheNetwork(t *testing.T) {
	api := &stubAPI{}

	if _, err := newClient(t, api).FilterEvents(context.Background(), cloudwatch.Query{}); err == nil {
		t.Fatal("want a refusal")
	}
	if len(api.inputs) != 0 {
		t.Fatal("want nothing sent")
	}
}

// The adapter's regional guard is only as good as what the client reports, so
// the client reports the region it was built for and nothing else.
func TestAClientReportsTheRegionItWasBuiltFor(t *testing.T) {
	client, err := cwaws.NewFromAPI("eu-west-1", &stubAPI{})
	if err != nil {
		t.Fatalf("building: %v", err)
	}
	if client.Region() != "eu-west-1" {
		t.Fatalf("want eu-west-1, got %s", client.Region())
	}
	var _ cloudwatch.LogsAPI = client
}
