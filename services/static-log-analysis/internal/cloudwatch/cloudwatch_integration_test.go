//go:build integration

package cloudwatch_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/clock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch/cwcheckpoint"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/ids"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/localstacktest"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/pebbletest"
)

const (
	integrationService     = "paymentservice"
	integrationEnvironment = "production"
)

// liveHarness wires the adapter to a real CloudWatch Logs endpoint and a real
// Pebble checkpoint store. Only the ingestion sink is a double, because the
// journal has its own contract tests and is not what these tests are about.
type liveHarness struct {
	admin       *cloudwatchlogs.Client
	group       string
	stream      string
	sink        *recordingSink
	checkpoints cloudwatch.CheckpointStore
	config      cloudwatch.Config
}

func newLiveHarness(t *testing.T) *liveHarness {
	t.Helper()
	admin := localstacktest.Admin(t)
	client := localstacktest.Client(t)
	group := localstacktest.LogGroup(t, admin)
	stream := "ecs/paymentservice/0e1f2a3b"
	localstacktest.LogStream(t, admin, group, stream)

	pebble := pebbletest.New(t)
	store, err := cwcheckpoint.New(pebble.DB())
	if err != nil {
		t.Fatalf("opening a checkpoint store: %v", err)
	}
	h := &liveHarness{admin: admin, group: group, stream: stream, sink: newSink(), checkpoints: store}
	h.config = cloudwatch.Config{
		// Real time, because CloudWatch stamps its own ingestion times and a
		// fake clock would put the adapter's window somewhere the source has
		// never heard of.
		Clock:               clock.System(),
		IDs:                 ids.System(),
		Policy:              redact.MinimalPolicy(),
		API:                 client,
		Checkpoints:         store,
		Sink:                h.sink,
		Account:             localstacktest.Account,
		Region:              localstacktest.Region,
		SourceInstance:      "cloudwatch-adapter-us-east-1-0",
		CredentialIdentity:  "arn:aws:iam::000000000000:role/log-analysis-reader",
		AllowedServices:     []string{integrationService},
		AllowedEnvironments: []string{integrationEnvironment},
		Classification:      "INTERNAL",
		Groups: []cloudwatch.GroupConfig{{
			LogGroup: group, Service: integrationService, Environment: integrationEnvironment,
		}},
		Lookback:        5 * time.Minute,
		MaxLookback:     time.Hour,
		InitialLookback: 15 * time.Minute,
		MaxWindow:       15 * time.Minute,
		PageLimit:       50,
	}
	return h
}

func (h *liveHarness) adapter(t *testing.T) *cloudwatch.Adapter {
	t.Helper()
	adapter, err := cloudwatch.New(h.config)
	if err != nil {
		t.Fatalf("building an adapter: %v", err)
	}
	return adapter
}

// put writes events at real timestamps, which is the only way CloudWatch will
// accept and index them.
func (h *liveHarness) put(t *testing.T, stream string, at time.Time, messages ...string) {
	t.Helper()
	events := make([]cwtypes.InputLogEvent, 0, len(messages))
	for i, message := range messages {
		events = append(events, cwtypes.InputLogEvent{
			Message:   awssdk.String(message),
			Timestamp: awssdk.Int64(at.Add(time.Duration(i) * time.Millisecond).UnixMilli()),
		})
	}
	if _, err := h.admin.PutLogEvents(context.Background(), &cloudwatchlogs.PutLogEventsInput{
		LogGroupName: awssdk.String(h.group), LogStreamName: awssdk.String(stream), LogEvents: events,
	}); err != nil {
		t.Fatalf("writing log events: %v", err)
	}
}

func (h *liveHarness) loadCheckpoints(t *testing.T) []cloudwatch.Checkpoint {
	t.Helper()
	loaded, err := h.checkpoints.LoadGroup(context.Background(), cloudwatch.Group{
		Account: localstacktest.Account, Region: localstacktest.Region, LogGroup: h.group,
	})
	if err != nil {
		t.Fatalf("loading checkpoints: %v", err)
	}
	return loaded
}

func errorMessage(order int) string {
	return fmt.Sprintf(`{"level":"error","msg":"charge failed for order %d: card declined"}`, order)
}

// pollUntil retries a cycle until it delivers what the test wrote. CloudWatch
// indexes asynchronously, so a first empty answer is a valid empty answer and
// not a failure, which is the very distinction the adapter is required to make.
func pollUntil(t *testing.T, adapter *cloudwatch.Adapter, want int) cloudwatch.PollResult {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	total := cloudwatch.PollResult{}
	for time.Now().Before(deadline) {
		result, err := adapter.Poll(context.Background())
		if err != nil {
			t.Fatalf("polling: %v", err)
		}
		total.Delivered += result.Delivered
		total.Duplicates += result.Duplicates
		total.EventsRead += result.EventsRead
		total.Pages += result.Pages
		total.Checkpoints += result.Checkpoints
		if total.Delivered >= want {
			return total
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("want %d records delivered within the deadline, got %d", want, total.Delivered)
	return total
}

func TestLiveCloudWatchEventsBecomeRecordsAndAdvanceACheckpoint(t *testing.T) {
	h := newLiveHarness(t)
	adapter := h.adapter(t)
	at := time.Now().Add(-time.Minute).UTC()
	h.put(t, h.stream, at, errorMessage(1), errorMessage(2), errorMessage(3))

	pollUntil(t, adapter, 3)

	if h.sink.distinct() != 3 {
		t.Fatalf("want three distinct records, got %d", h.sink.distinct())
	}
	if h.sink.total() != 3 {
		t.Fatalf("want each record delivered once, got %d", h.sink.total())
	}
	checkpoints := h.loadCheckpoints(t)
	if len(checkpoints) != 1 {
		t.Fatalf("want one checkpoint, got %d", len(checkpoints))
	}
	if checkpoints[0].EventID == "" {
		t.Fatal("want the native event id recorded at the boundary")
	}
	if checkpoints[0].Stream.Name != h.stream {
		t.Fatalf("want the checkpoint on the written stream, got %q", checkpoints[0].Stream.Name)
	}
	for _, batch := range h.sink.batches {
		for _, record := range batch {
			if record.RecordIDVersion != "cw:v1" {
				t.Fatalf("want cw:v1 identity, got %s", record.RecordIDVersion)
			}
			if err := record.Validate(); err != nil {
				t.Fatalf("a record read from real CloudWatch is invalid: %v", err)
			}
			if err := redact.MinimalPolicy().ValidateRecord(record); err != nil {
				t.Fatalf("a record read from real CloudWatch is unsafe: %v", err)
			}
		}
	}
}

// The acceptance test source-adapters.md asks for: a crash between the journal
// acknowledgement and the checkpoint commit makes the replacement replica
// reread an overlap of real CloudWatch events, and each source event still
// contributes exactly one logical record.
func TestLiveOverlapReplayAfterALostCheckpointContributesEachEventOnce(t *testing.T) {
	h := newLiveHarness(t)
	at := time.Now().Add(-time.Minute).UTC()
	h.put(t, h.stream, at, errorMessage(1), errorMessage(2), errorMessage(3), errorMessage(4), errorMessage(5))

	crashConfig := h.config
	crashConfig.Checkpoints = &failingCheckpoints{
		CheckpointStore: h.checkpoints,
		fail:            errors.New("power loss before the checkpoint was written"),
	}
	crashing, err := cloudwatch.New(crashConfig)
	if err != nil {
		t.Fatalf("building the adapter that will crash: %v", err)
	}

	// The first replica delivers, is acknowledged, and dies before its
	// checkpoint reaches disk.
	deadline := time.Now().Add(30 * time.Second)
	for h.sink.distinct() < 5 && time.Now().Before(deadline) {
		if _, err := crashing.Poll(context.Background()); err == nil {
			t.Fatal("want the failed checkpoint commit reported")
		}
		if h.sink.distinct() < 5 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if h.sink.distinct() != 5 {
		t.Fatalf("want five records to have reached the journal before the crash, got %d", h.sink.distinct())
	}
	if got := h.loadCheckpoints(t); len(got) != 0 {
		t.Fatalf("the crash was supposed to lose the checkpoint, found %+v", got)
	}
	before := h.sink.recordIDs()

	// The replacement replica has no memory of the first pass and no
	// checkpoint, so it rereads the whole overlap from real CloudWatch.
	replacement := h.adapter(t)
	result := pollUntil(t, replacement, 5)

	if result.Delivered < 5 {
		t.Fatalf("want the overlap reread, got %d", result.Delivered)
	}
	if h.sink.distinct() != 5 {
		t.Fatalf("the replay produced new identities: want 5 distinct records, got %d", h.sink.distinct())
	}
	after := h.sink.recordIDs()
	if len(before) != len(after) {
		t.Fatalf("want the same record ids on both passes, got %v then %v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("record id changed across the replay: %s then %s", before[i], after[i])
		}
	}
	if got := h.loadCheckpoints(t); len(got) != 1 {
		t.Fatalf("want the replacement to checkpoint, got %+v", got)
	}
}

func TestLiveQuietLogGroupIsAnEmptyResultAndNotAFailure(t *testing.T) {
	h := newLiveHarness(t)
	adapter := h.adapter(t)

	result, err := adapter.Poll(context.Background())

	if err != nil {
		t.Fatalf("a quiet log group is a valid empty answer, got %v", err)
	}
	if !result.Empty || result.Delivered != 0 {
		t.Fatalf("want an empty result, got %+v", result)
	}
	if adapter.Counters().SourceFailures != 0 {
		t.Fatal("an empty result was counted as a source failure")
	}
	if adapter.Counters().EmptyResults == 0 {
		t.Fatal("want the empty result counted separately")
	}
	if got := h.loadCheckpoints(t); len(got) != 0 {
		t.Fatalf("an empty result advanced a checkpoint: %+v", got)
	}
}

// A log group that does not exist is an outage-shaped answer, not silence. If
// the adapter reported it as empty, an operator would see a healthy adapter
// reading nothing forever.
func TestLiveMissingLogGroupIsReportedAsASourceFailure(t *testing.T) {
	h := newLiveHarness(t)
	h.config.Groups = []cloudwatch.GroupConfig{{
		LogGroup: h.group + "-does-not-exist", Service: integrationService, Environment: integrationEnvironment,
	}}
	adapter := h.adapter(t)

	_, err := adapter.Poll(context.Background())

	if !errors.Is(err, cloudwatch.ErrSourceUnavailable) {
		t.Fatalf("want a source failure, got %v", err)
	}
	if adapter.Counters().EmptyResults != 0 {
		t.Fatal("a missing log group was counted as an empty result")
	}
}

func TestLivePaginationWalksEveryPageToTheHorizon(t *testing.T) {
	h := newLiveHarness(t)
	h.config.PageLimit = 3
	adapter := h.adapter(t)
	at := time.Now().Add(-2 * time.Minute).UTC()
	messages := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		messages = append(messages, errorMessage(i))
	}
	h.put(t, h.stream, at, messages...)

	result := pollUntil(t, adapter, 12)

	if h.sink.distinct() != 12 {
		t.Fatalf("want every event across every page, got %d", h.sink.distinct())
	}
	if result.Pages < 4 {
		t.Fatalf("want the pages actually walked, got %d", result.Pages)
	}
}

func TestLiveSeveralStreamsEachKeepTheirOwnCheckpoint(t *testing.T) {
	h := newLiveHarness(t)
	second := "ecs/paymentservice/deadbeef"
	localstacktest.LogStream(t, h.admin, h.group, second)
	adapter := h.adapter(t)
	at := time.Now().Add(-time.Minute).UTC()
	h.put(t, h.stream, at, errorMessage(1))
	h.put(t, second, at.Add(30*time.Second), errorMessage(2))

	pollUntil(t, adapter, 2)

	checkpoints := h.loadCheckpoints(t)
	if len(checkpoints) != 2 {
		t.Fatalf("want a checkpoint per stream, got %d: %+v", len(checkpoints), checkpoints)
	}
	names := map[string]bool{}
	for _, checkpoint := range checkpoints {
		names[checkpoint.Stream.Name] = true
	}
	if !names[h.stream] || !names[second] {
		t.Fatalf("want both streams checkpointed, got %+v", names)
	}
}
