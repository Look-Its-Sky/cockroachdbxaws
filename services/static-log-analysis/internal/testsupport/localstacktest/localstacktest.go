// Package localstacktest runs tests against a real CloudWatch Logs API.
//
// The CloudWatch adapter's behaviour is decided by things a hand-written double
// cannot be trusted to reproduce: which events a millisecond-resolution,
// inclusive-end time filter returns, how pagination tokens behave across an
// overlapping window, and what a native event ID looks like. A double would
// agree with whatever the adapter expected, so these tests use LocalStack's
// CloudWatch Logs implementation in a container.
//
// One container is shared by every test in a process, because starting it is
// the expensive part. Each test creates its own log group inside it, so tests
// remain independent.
package localstacktest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/localstack"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch/cwaws"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/outbox/sqsaws"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/tb"
)

const (
	// imageEnv overrides the pinned image, for trying a new LocalStack version
	// before pinning it.
	imageEnv = "LOCALSTACK_TEST_IMAGE"
	// requireEnv makes an unavailable container a failure rather than a skip.
	// Continuous integration sets it, so a broken container cannot quietly turn
	// the source gate into no gate at all.
	requireEnv = "REQUIRE_DOCKER"
	// disableEnv skips these tests without attempting to start anything.
	disableEnv = "LOCALSTACK_TEST"

	// Region is the one region these tests run in. The adapter is regional, so
	// crossing this value is the boundary violation, not a configuration knob.
	Region = "us-east-1"
	// Account is the account LocalStack attributes resources to.
	Account = "000000000000"

	// PinnedImage is pinned by digest, not only by tag. A tag moves between
	// patch releases, so integration behaviour and CI results could change with
	// no commit in this repository.
	PinnedImage = "localstack/localstack:4.0.3@sha256:" +
		"17c2f79ca4e1f804eb912291a19713d4134806325ef0d21d4c1053161dfa72d0"
)

// shared is the process-wide container, started at most once.
var shared struct {
	once      sync.Once
	container *localstack.LocalStackContainer
	endpoint  string
	err       error
}

// groupCounter names each test's log group uniquely within a process.
var groupCounter atomic.Uint64

// Endpoint returns the base endpoint of the shared LocalStack container.
//
// When Docker is unavailable the test is skipped, unless REQUIRE_DOCKER=1 is
// set, in which case it fails.
func Endpoint(t tb.TB) string {
	t.Helper()
	if os.Getenv(disableEnv) == "0" {
		t.Skipf("localstacktest: skipping because %s=0", disableEnv)
		return ""
	}
	endpoint, err := sharedEndpoint(context.Background())
	if err != nil {
		unavailable(t, err)
		return ""
	}
	return endpoint
}

// Credentials returns the static credentials LocalStack accepts. They are not
// secrets: LocalStack accepts any well-formed pair, and using a fixed one keeps
// a test from picking up a developer's real profile by accident.
func Credentials() awssdk.CredentialsProvider {
	return credentials.NewStaticCredentialsProvider("test", "test", "")
}

// Client returns a cwaws client pointed at the shared container.
func Client(t tb.TB) *cwaws.Client {
	t.Helper()
	endpoint := Endpoint(t)
	if endpoint == "" {
		return nil
	}
	client, err := cwaws.New(context.Background(), Region,
		cwaws.WithBaseEndpoint(endpoint), cwaws.WithCredentials(Credentials()))
	if err != nil {
		t.Fatalf("localstacktest: building a regional client: %v", err)
	}
	return client
}

// Admin returns a raw CloudWatch Logs client for arranging a scenario: creating
// groups and streams and writing events. The adapter under test never writes.
func Admin(t tb.TB) *cloudwatchlogs.Client {
	t.Helper()
	endpoint := Endpoint(t)
	if endpoint == "" {
		return nil
	}
	return cloudwatchlogs.New(cloudwatchlogs.Options{
		Region:       Region,
		Credentials:  Credentials(),
		BaseEndpoint: awssdk.String(endpoint),
	})
}

// LogGroup creates a log group unique to this test and deletes it afterwards.
func LogGroup(t tb.TB, admin *cloudwatchlogs.Client) string {
	t.Helper()
	name := fmt.Sprintf("/static-log-analysis/%s/%d", sanitize(t.Name()), groupCounter.Add(1))
	ctx := context.Background()
	if _, err := admin.CreateLogGroup(ctx, &cloudwatchlogs.CreateLogGroupInput{
		LogGroupName: awssdk.String(name),
	}); err != nil {
		t.Fatalf("localstacktest: creating log group %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.DeleteLogGroup(context.Background(), &cloudwatchlogs.DeleteLogGroupInput{
			LogGroupName: awssdk.String(name),
		}); err != nil {
			t.Logf("localstacktest: deleting log group %s: %v", name, err)
		}
	})
	return name
}

// LogStream creates a stream inside a group.
func LogStream(t tb.TB, admin *cloudwatchlogs.Client, group, stream string) {
	t.Helper()
	if _, err := admin.CreateLogStream(context.Background(), &cloudwatchlogs.CreateLogStreamInput{
		LogGroupName: awssdk.String(group), LogStreamName: awssdk.String(stream),
	}); err != nil {
		t.Fatalf("localstacktest: creating log stream %s: %v", stream, err)
	}
}

// SQSAdmin returns a raw SQS client for arranging a scenario: creating queues
// and reading back what the transport published. The transport under test never
// receives.
func SQSAdmin(t tb.TB) *sqs.Client {
	t.Helper()
	endpoint := Endpoint(t)
	if endpoint == "" {
		return nil
	}
	return sqs.New(sqs.Options{
		Region:       Region,
		Credentials:  Credentials(),
		BaseEndpoint: awssdk.String(endpoint),
	})
}

// Queue creates a queue unique to this test and deletes it afterwards. It
// returns the queue URL the transport publishes to.
func Queue(t tb.TB, admin *sqs.Client, suffix string) string {
	t.Helper()
	name := fmt.Sprintf("sla-%s-%d-%s", sanitize(t.Name()), groupCounter.Add(1), suffix)
	ctx := context.Background()
	created, err := admin.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: awssdk.String(name)})
	if err != nil {
		t.Fatalf("localstacktest: creating queue %s: %v", name, err)
	}
	url := awssdk.ToString(created.QueueUrl)
	t.Cleanup(func() {
		if _, err := admin.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{
			QueueUrl: awssdk.String(url),
		}); err != nil {
			t.Logf("localstacktest: deleting queue %s: %v", name, err)
		}
	})
	return url
}

// Transport returns an sqsaws client pointed at the shared container.
func Transport(t tb.TB, queueURL, deadLetterQueueURL string) *sqsaws.Client {
	t.Helper()
	endpoint := Endpoint(t)
	if endpoint == "" {
		return nil
	}
	transport, err := sqsaws.New(context.Background(), sqsaws.Config{
		Region: Region, QueueURL: queueURL, DeadLetterQueueURL: deadLetterQueueURL,
		BaseEndpoint: endpoint, Credentials: Credentials(),
	})
	if err != nil {
		t.Fatalf("localstacktest: building a regional transport: %v", err)
	}
	return transport
}

// Drain reads everything currently on a queue, with its attributes. SQS
// Standard is at-least-once and unordered, so a caller compares sets rather
// than sequences.
func Drain(t tb.TB, admin *sqs.Client, queueURL string, expected int) []sqstypes.Message {
	t.Helper()
	var received []sqstypes.Message
	ctx := context.Background()
	// Long polling, repeated: one ReceiveMessage returns whatever happens to be
	// on one SQS host, so a single call is not evidence a queue is empty.
	for attempt := 0; attempt < 5 && len(received) < expected; attempt++ {
		out, err := admin.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:              awssdk.String(queueURL),
			MaxNumberOfMessages:   10,
			WaitTimeSeconds:       1,
			MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			t.Fatalf("localstacktest: receiving from %s: %v", queueURL, err)
		}
		received = append(received, out.Messages...)
	}
	return received
}

func sharedEndpoint(ctx context.Context) (string, error) {
	shared.once.Do(func() {
		image := os.Getenv(imageEnv)
		if image == "" {
			image = PinnedImage
		}
		container, err := localstack.Run(ctx, image,
			testcontainers.WithEnv(map[string]string{"SERVICES": "logs,sqs"}))
		if err != nil {
			shared.err = fmt.Errorf("starting localstack: %w", err)
			return
		}
		shared.container = container
		port, err := container.MappedPort(ctx, "4566/tcp")
		if err != nil {
			shared.err = fmt.Errorf("resolving the localstack port: %w", err)
			return
		}
		host, err := container.Host(ctx)
		if err != nil {
			shared.err = fmt.Errorf("resolving the localstack host: %w", err)
			return
		}
		shared.endpoint = fmt.Sprintf("http://%s:%s", host, port.Port())
	})
	return shared.endpoint, shared.err
}

func unavailable(t tb.TB, err error) {
	t.Helper()
	if os.Getenv(requireEnv) == "1" {
		t.Fatalf("localstacktest: LocalStack is required because %s=1, but it is unavailable: %v", requireEnv, err)
		return
	}
	t.Skipf("localstacktest: skipping, LocalStack is unavailable: %v.\n"+
		"Start Docker to run this test, or set %s=1 to make its absence a failure.", err, requireEnv)
}

func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, name)
}
