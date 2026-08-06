package fakeclock_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
)

func TestNowStartsAtTheGivenTimeAndOnlyMovesWhenAdvanced(t *testing.T) {
	c := fakeclock.NewAtOrigin()

	if got := c.Now(); !got.Equal(fakeclock.Origin) {
		t.Fatalf("want start at %s, got %s", fakeclock.Origin, got)
	}
	time.Sleep(2 * time.Millisecond)
	if got := c.Now(); !got.Equal(fakeclock.Origin) {
		t.Fatalf("fake time moved on its own: %s", got)
	}

	c.Advance(90 * time.Second)
	want := fakeclock.Origin.Add(90 * time.Second)
	if got := c.Now(); !got.Equal(want) {
		t.Fatalf("want %s after advancing, got %s", want, got)
	}
}

func TestNowIsUTC(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	if loc := c.Now().Location(); loc != time.UTC {
		t.Fatalf("want UTC, got %s", loc)
	}
}

func TestNewRejectsANonUTCStart(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want a panic for a non-UTC start, got none")
		}
	}()
	fakeclock.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("CET", 3600)))
}

func TestTimerFiresExactlyAtItsDeadline(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	timer := c.NewTimer(5 * time.Minute)

	// One nanosecond short of the deadline the timer must not fire. Window
	// boundaries are decided at exact instants, so a timer that fires early
	// would let an off-by-one boundary bug pass.
	c.Advance(5*time.Minute - time.Nanosecond)
	select {
	case at := <-timer.C():
		t.Fatalf("timer fired early at %s", at)
	default:
	}

	c.Advance(time.Nanosecond)
	select {
	case at := <-timer.C():
		if want := fakeclock.Origin.Add(5 * time.Minute); !at.Equal(want) {
			t.Fatalf("want the timer to report its deadline %s, got %s", want, at)
		}
	default:
		t.Fatal("want the timer to fire at its exact deadline, got nothing")
	}
}

func TestAdvanceFiresExactlyTheTimersItReaches(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	timers := map[time.Duration]clockTimer{}
	for _, delay := range []time.Duration{3 * time.Minute, time.Minute, 2 * time.Minute} {
		timers[delay] = c.NewTimer(delay)
	}

	c.Advance(2 * time.Minute)

	// After advancing to T, exactly the timers with a deadline at or before T
	// have fired. This is what a scenario relies on when it steps a watermark
	// across a boundary one interval at a time.
	assertFired(t, timers[time.Minute], fakeclock.Origin.Add(time.Minute))
	assertFired(t, timers[2*time.Minute], fakeclock.Origin.Add(2*time.Minute))
	assertNotFired(t, timers[3*time.Minute])

	c.Advance(time.Minute)
	assertFired(t, timers[3*time.Minute], fakeclock.Origin.Add(3*time.Minute))
}

// clockTimer is the timer shape the fake returns, named locally so the table
// above stays readable.
type clockTimer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(time.Duration) bool
}

func assertFired(t *testing.T, timer clockTimer, want time.Time) {
	t.Helper()
	select {
	case at := <-timer.C():
		// The instant is carried on the channel rather than read from the
		// clock: a handler on another goroutine may not run until the whole
		// advance has finished.
		if !at.Equal(want) {
			t.Fatalf("want the timer to report %s, got %s", want, at)
		}
	default:
		t.Fatalf("want a timer with deadline %s to have fired, got nothing", want)
	}
}

func assertNotFired(t *testing.T, timer clockTimer) {
	t.Helper()
	select {
	case at := <-timer.C():
		t.Fatalf("timer fired early at %s", at)
	default:
	}
}

func TestAdvanceLeavesTimeAtTheRequestedInstant(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	c.NewTimer(time.Minute)

	c.Advance(10 * time.Minute)

	want := fakeclock.Origin.Add(10 * time.Minute)
	if got := c.Now(); !got.Equal(want) {
		t.Fatalf("want time to end at %s after firing an earlier timer, got %s", want, got)
	}
}

func TestStopPreventsATimerFromFiring(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	timer := c.NewTimer(time.Minute)

	if !timer.Stop() {
		t.Fatal("want Stop to report a pending timer")
	}
	if timer.Stop() {
		t.Fatal("want the second Stop to report the timer was no longer pending")
	}

	c.Advance(time.Hour)
	select {
	case at := <-timer.C():
		t.Fatalf("stopped timer fired at %s", at)
	default:
	}
	if got := c.WaiterCount(); got != 0 {
		t.Fatalf("want a stopped timer removed, got %d pending", got)
	}
}

func TestResetReschedulesOnTheSameChannel(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	timer := c.NewTimer(time.Minute)
	// Captured before the reset: a caller that stored the channel must keep
	// receiving on it.
	fired := timer.C()

	if !timer.Reset(10 * time.Minute) {
		t.Fatal("want Reset to report a pending timer")
	}

	c.Advance(time.Minute)
	select {
	case at := <-fired:
		t.Fatalf("timer fired at its old deadline %s", at)
	default:
	}

	c.Advance(9 * time.Minute)
	select {
	case at := <-fired:
		if want := fakeclock.Origin.Add(10 * time.Minute); !at.Equal(want) {
			t.Fatalf("want the new deadline %s, got %s", want, at)
		}
	default:
		t.Fatal("want the timer to fire at its new deadline, got nothing")
	}
}

func TestSleepReturnsWhenTimeReachesItsDeadline(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	done := make(chan error, 1)
	go func() { done <- c.Sleep(context.Background(), 30*time.Second) }()
	c.BlockUntil(1)

	c.Advance(29 * time.Second)
	select {
	case err := <-done:
		t.Fatalf("sleep returned early: %v", err)
	case <-time.After(5 * time.Millisecond):
	}

	c.Advance(time.Second)
	if err := <-done; err != nil {
		t.Fatalf("want sleep to complete, got %v", err)
	}
}

func TestSleepReturnsWhenTheContextIsCancelled(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Sleep(ctx, time.Hour) }()
	c.BlockUntil(1)

	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	// An abandoned sleep must not stay pending, or BlockUntil counts in a later
	// phase of the same test become wrong.
	deadline := time.Now().Add(time.Second)
	for c.WaiterCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("want the cancelled sleep removed, got %d pending", c.WaiterCount())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBlockUntilWaitsForPendingWaiters(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	go func() {
		time.Sleep(2 * time.Millisecond)
		c.NewTimer(time.Minute)
		c.NewTimer(time.Minute)
	}()

	c.BlockUntil(2)

	if got := c.WaiterCount(); got < 2 {
		t.Fatalf("want BlockUntil to wait for 2 waiters, returned with %d", got)
	}
}

func TestAdvanceRejectsMovingTimeBackward(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	defer func() {
		if recover() == nil {
			t.Fatal("want a panic when moving time backward, got none")
		}
	}()
	// Watermarks never move backward, so neither may the clock a test drives
	// them with.
	c.Advance(-time.Second)
}

func TestAdvanceToRejectsAnEarlierInstant(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	c.Advance(time.Hour)
	defer func() {
		if recover() == nil {
			t.Fatal("want a panic when advancing to an earlier instant, got none")
		}
	}()
	c.AdvanceTo(fakeclock.Origin)
}

func TestPendingDeadlinesAreReportedInFireOrder(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	c.NewTimer(3 * time.Minute)
	c.NewTimer(time.Minute)
	c.NewTimer(2 * time.Minute)

	got := c.PendingDeadlines()
	want := []time.Time{
		fakeclock.Origin.Add(time.Minute),
		fakeclock.Origin.Add(2 * time.Minute),
		fakeclock.Origin.Add(3 * time.Minute),
	}
	if len(got) != len(want) {
		t.Fatalf("want %d pending deadlines, got %d", len(want), len(got))
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("want deadline %d to be %s, got %s", i, want[i], got[i])
		}
	}
}

func TestConcurrentReadersAndTimersAreSafe(t *testing.T) {
	c := fakeclock.NewAtOrigin()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				timer := c.NewTimer(time.Duration(j) * time.Millisecond)
				_ = c.Now()
				timer.Stop()
			}
		}()
	}
	for i := 0; i < 50; i++ {
		c.Advance(time.Millisecond)
	}
	wg.Wait()
}
