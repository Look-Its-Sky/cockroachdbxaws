package clock_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/clock"
)

func TestSystemClockReadsUTC(t *testing.T) {
	// Duration arithmetic never uses local timezone rules, so the production
	// clock must not hand out local times in the first place.
	if location := clock.System().Now().Location(); location != time.UTC {
		t.Fatalf("want UTC, got %s", location)
	}
}

func TestSystemClockMovesForward(t *testing.T) {
	c := clock.System()

	before := c.Now()
	time.Sleep(2 * time.Millisecond)
	after := c.Now()

	if !after.After(before) {
		t.Fatalf("want time to advance, got %s then %s", before, after)
	}
}

func TestSystemTimerFires(t *testing.T) {
	timer := clock.System().NewTimer(time.Millisecond)
	defer timer.Stop()

	select {
	case <-timer.C():
	case <-time.After(time.Second):
		t.Fatal("want the timer to fire within a second")
	}
}

func TestSystemTimerStopPreventsFiring(t *testing.T) {
	timer := clock.System().NewTimer(time.Hour)

	if !timer.Stop() {
		t.Fatal("want Stop to report a pending timer")
	}
	if timer.Stop() {
		t.Fatal("want the second Stop to report the timer was no longer pending")
	}
}

func TestSystemTimerResetReschedules(t *testing.T) {
	timer := clock.System().NewTimer(time.Hour)

	if !timer.Reset(time.Millisecond) {
		t.Fatal("want Reset to report a pending timer")
	}
	select {
	case <-timer.C():
	case <-time.After(time.Second):
		t.Fatal("want the timer to fire at its new deadline")
	}
}

func TestSystemSleepCompletes(t *testing.T) {
	if err := clock.System().Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("want the sleep to complete, got %v", err)
	}
}

func TestSystemSleepStopsWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Retry and lease loops sleep between attempts; a sleep that ignored
	// cancellation would hold shutdown open for its full duration.
	err := clock.System().Sleep(ctx, time.Hour)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestSystemSleepOfNoDurationReportsTheContextState(t *testing.T) {
	if err := clock.System().Sleep(context.Background(), 0); err != nil {
		t.Fatalf("want no error for a zero sleep on a live context, got %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := clock.System().Sleep(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("want a zero sleep to still notice a cancelled context, got %v", err)
	}
}
