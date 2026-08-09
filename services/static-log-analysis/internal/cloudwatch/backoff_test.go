package cloudwatch_test

import (
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch"
)

func TestBackoffWindowDoublesPerAttemptAndIsCapped(t *testing.T) {
	backoff := cloudwatch.Backoff{Base: 100 * time.Millisecond, Max: time.Second, MaxAttempts: 8}

	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		time.Second,
		time.Second,
	}
	for i, expected := range want {
		attempt := i + 1
		if got := backoff.Window(attempt); got != expected {
			t.Fatalf("attempt %d: want window %s, got %s", attempt, expected, got)
		}
	}
}

// Full jitter, not equal jitter: the delay is drawn from the whole window. Every
// adapter replica that was throttled at the same instant would otherwise retry
// at the same instant and be throttled again together.
func TestBackoffDelayStaysInsideItsWindow(t *testing.T) {
	backoff := cloudwatch.Backoff{Base: 100 * time.Millisecond, Max: 4 * time.Second, MaxAttempts: 8}

	distinct := map[time.Duration]bool{}
	for i := 0; i < 500; i++ {
		delay := backoff.Delay(4)
		if delay < 0 || delay > backoff.Window(4) {
			t.Fatalf("delay %s is outside the window %s", delay, backoff.Window(4))
		}
		distinct[delay] = true
	}
	if len(distinct) < 2 {
		t.Fatal("every retry drew the same delay, so replicas would retry in lockstep")
	}
}

func TestBackoffJitterIsInjectableForDeterministicTests(t *testing.T) {
	backoff := cloudwatch.Backoff{
		Base: time.Second, Max: time.Minute, MaxAttempts: 3,
		Jitter: func(window time.Duration) time.Duration { return window / 4 },
	}

	if got, want := backoff.Delay(2), 500*time.Millisecond; got != want {
		t.Fatalf("want an injected delay of %s, got %s", want, got)
	}
}

func TestBackoffDefaultsAreUsableWithoutConfiguration(t *testing.T) {
	var backoff cloudwatch.Backoff

	if backoff.Window(1) <= 0 {
		t.Fatal("a zero-valued backoff must still wait between attempts")
	}
	if backoff.Attempts() < 2 {
		t.Fatal("a zero-valued backoff must still permit a retry")
	}
	if backoff.Window(20) > cloudwatch.DefaultBackoffMax {
		t.Fatalf("want the default cap honoured, got %s", backoff.Window(20))
	}
}

func TestBackoffRefusesToWaitForeverOrNotAtAll(t *testing.T) {
	tests := []struct {
		name    string
		backoff cloudwatch.Backoff
	}{
		{"negative base", cloudwatch.Backoff{Base: -time.Second}},
		{"negative max", cloudwatch.Backoff{Max: -time.Second}},
		{"max below base", cloudwatch.Backoff{Base: time.Minute, Max: time.Second}},
		{"no attempts", cloudwatch.Backoff{MaxAttempts: -1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.backoff.Validate(); err == nil {
				t.Fatal("want a configuration refusal")
			}
		})
	}
}
