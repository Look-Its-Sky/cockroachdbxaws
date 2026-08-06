package capture_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/queue"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/capture"
)

func message(key string) queue.Message {
	return queue.Message{
		MessageID:        "0194f0a0-0000-7000-8000-" + strings.Repeat("0", 11) + key,
		DeduplicationKey: "assignment:" + key,
		Type:             "agent.assignment.v1",
		Body:             []byte(`{"investigation_id":"` + key + `"}`),
		Attributes:       map[string]string{"region": "us-east-1"},
	}
}

func TestPublishRecordsMessagesInOrder(t *testing.T) {
	publisher := capture.New()

	if err := publisher.Publish(context.Background(), []queue.Message{message("1"), message("2")}); err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	if err := publisher.Publish(context.Background(), []queue.Message{message("3")}); err != nil {
		t.Fatalf("publish failed: %v", err)
	}

	want := []string{"assignment:1", "assignment:2", "assignment:3"}
	if got := publisher.DeduplicationKeys(); !equal(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	if got := publisher.Calls(); got != 2 {
		t.Fatalf("want 2 publish calls, got %d", got)
	}
}

func TestRecordedMessagesAreIndependentOfTheCaller(t *testing.T) {
	publisher := capture.New()
	sent := message("1")
	if err := publisher.Publish(context.Background(), []queue.Message{sent}); err != nil {
		t.Fatalf("publish failed: %v", err)
	}

	// A caller that reuses its buffers must not be able to rewrite what the
	// harness recorded, or an assertion would describe the caller's last state
	// rather than what was published.
	sent.Body[0] = 'X'
	sent.Attributes["region"] = "eu-west-1"

	recorded := publisher.Messages()
	if recorded[0].Body[0] == 'X' {
		t.Error("recorded body changed when the caller mutated its slice")
	}
	if recorded[0].Attributes["region"] != "us-east-1" {
		t.Errorf("recorded attributes changed when the caller mutated its map: %v", recorded[0].Attributes)
	}

	// The returned copy must be independent too.
	recorded[0].Attributes["region"] = "ap-south-1"
	if publisher.Messages()[0].Attributes["region"] != "us-east-1" {
		t.Error("mutating a returned message changed the recorded message")
	}
}

func TestFailBeforeDeliveryRecordsNothing(t *testing.T) {
	publisher := capture.New()
	wantErr := errors.New("sqs unavailable")
	publisher.FailNext(1, capture.FailBeforeDelivery, wantErr)

	err := publisher.Publish(context.Background(), []queue.Message{message("1")})

	if !errors.Is(err, wantErr) {
		t.Fatalf("want %v, got %v", wantErr, err)
	}
	if got := publisher.Len(); got != 0 {
		t.Fatalf("want nothing delivered, got %d", got)
	}
	if got := publisher.Calls(); got != 1 {
		t.Fatalf("want the failed attempt counted, got %d calls", got)
	}
}

func TestFailAfterDeliveryRecordsTheMessage(t *testing.T) {
	publisher := capture.New()
	wantErr := errors.New("acknowledgement lost")
	publisher.FailNext(1, capture.FailAfterDelivery, wantErr)

	// The caller sees a failure and republishes, so the queue holds the message
	// twice. This is the duplicate delivery consumers must tolerate.
	if err := publisher.Publish(context.Background(), []queue.Message{message("1")}); !errors.Is(err, wantErr) {
		t.Fatalf("want %v, got %v", wantErr, err)
	}
	if err := publisher.Publish(context.Background(), []queue.Message{message("1")}); err != nil {
		t.Fatalf("republish failed: %v", err)
	}

	want := []string{"assignment:1", "assignment:1"}
	if got := publisher.DeduplicationKeys(); !equal(got, want) {
		t.Fatalf("want the message delivered twice %v, got %v", want, got)
	}
}

func TestFailNextAppliesToEachOfTheNextCalls(t *testing.T) {
	publisher := capture.New()
	wantErr := errors.New("throttled")
	publisher.FailNext(2, capture.FailBeforeDelivery, wantErr)

	for attempt := 1; attempt <= 2; attempt++ {
		if err := publisher.Publish(context.Background(), []queue.Message{message("1")}); !errors.Is(err, wantErr) {
			t.Fatalf("attempt %d: want %v, got %v", attempt, wantErr, err)
		}
	}
	if err := publisher.Publish(context.Background(), []queue.Message{message("1")}); err != nil {
		t.Fatalf("want the third attempt to succeed, got %v", err)
	}
	if got := publisher.Len(); got != 1 {
		t.Fatalf("want 1 delivered message, got %d", got)
	}
}

func TestFailNextRejectsArgumentsThatWouldInjectNothing(t *testing.T) {
	tests := []struct {
		name    string
		call    func(*capture.Publisher)
		because string
	}{
		{
			name:    "zero count",
			call:    func(p *capture.Publisher) { p.FailNext(0, capture.FailBeforeDelivery, errors.New("boom")) },
			because: "a scenario that asked for a failure and got none would test the success path",
		},
		{
			name:    "negative count",
			call:    func(p *capture.Publisher) { p.FailNext(-3, capture.FailBeforeDelivery, errors.New("boom")) },
			because: "a negative count silently schedules nothing",
		},
		{
			name:    "unknown failure mode",
			call:    func(p *capture.Publisher) { p.FailNext(1, capture.FailureMode(99), errors.New("boom")) },
			because: "an unrecognized mode would fall through to one of the real ones",
		},
		{
			name:    "nil error",
			call:    func(p *capture.Publisher) { p.FailNext(1, capture.FailBeforeDelivery, nil) },
			because: "a failure with no error is indistinguishable from success",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("want a panic because %s, got none", test.because)
				}
			}()
			test.call(capture.New())
		})
	}
}

func TestPublishRejectsAStructurallyInvalidMessage(t *testing.T) {
	publisher := capture.New()
	invalid := message("1")
	invalid.DeduplicationKey = "  "

	err := publisher.Publish(context.Background(), []queue.Message{invalid})

	if err == nil {
		t.Fatal("want a message with a blank deduplication key rejected, got no error")
	}
	if got := publisher.Len(); got != 0 {
		t.Fatalf("want nothing recorded for an invalid message, got %d", got)
	}
}

func TestPublishHonoursACancelledContext(t *testing.T) {
	publisher := capture.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := publisher.Publish(ctx, []queue.Message{message("1")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if got := publisher.Len(); got != 0 {
		t.Fatalf("want nothing recorded, got %d", got)
	}
}

func TestWaitForReturnsOnceEnoughMessagesArrive(t *testing.T) {
	publisher := capture.New()
	go func() {
		for i := 0; i < 3; i++ {
			time.Sleep(time.Millisecond)
			_ = publisher.Publish(context.Background(), []queue.Message{message("1")})
		}
	}()

	messages, err := publisher.WaitForDuration(5*time.Second, 3)
	if err != nil {
		t.Fatalf("wait failed: %v", err)
	}
	if len(messages) < 3 {
		t.Fatalf("want at least 3 messages, got %d", len(messages))
	}
}

func TestWaitForReportsWhatItWasStillWaitingFor(t *testing.T) {
	publisher := capture.New()
	if err := publisher.Publish(context.Background(), []queue.Message{message("1")}); err != nil {
		t.Fatalf("publish failed: %v", err)
	}

	_, err := publisher.WaitForDuration(20*time.Millisecond, 5)

	if err == nil {
		t.Fatal("want a timeout waiting for messages that never arrive, got none")
	}
	// A bare deadline error would leave the reader guessing how far the
	// scenario actually got.
	if !strings.Contains(err.Error(), "1 delivered") {
		t.Fatalf("want the error to report progress, got %v", err)
	}
}

func TestResetClearsRecordedStateAndQueuedFailures(t *testing.T) {
	publisher := capture.New()
	publisher.FailNext(1, capture.FailBeforeDelivery, errors.New("boom"))
	publisher.Reset()

	if err := publisher.Publish(context.Background(), []queue.Message{message("1")}); err != nil {
		t.Fatalf("want the queued failure discarded by Reset, got %v", err)
	}
	if got := publisher.Len(); got != 1 {
		t.Fatalf("want 1 message after reset, got %d", got)
	}
}

func TestConcurrentPublishersRecordEveryMessageExactlyOnce(t *testing.T) {
	publisher := capture.New()
	const publishers, each = 8, 100

	var wg sync.WaitGroup
	for i := 0; i < publishers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < each; j++ {
				if err := publisher.Publish(context.Background(), []queue.Message{message("1")}); err != nil {
					t.Errorf("publish failed: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	if got := publisher.Len(); got != publishers*each {
		t.Fatalf("want %d messages, got %d", publishers*each, got)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
