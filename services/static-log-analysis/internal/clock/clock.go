// Package clock provides the injectable time source used everywhere durations
// matter. Window arithmetic, lease expiry, claim expiry, and retry backoff all
// read time through this interface so that tests drive them deterministically
// rather than by sleeping.
//
// All times are UTC with nanosecond resolution. Local timezone rules never
// participate in duration arithmetic.
package clock

import (
	"context"
	"time"
)

// Clock is a wall clock. The same interface serves as the event clock in
// components that derive event time from arrival rather than record content;
// which role a clock plays is a property of where it is injected.
type Clock interface {
	// Now returns the current time in UTC.
	Now() time.Time
	// NewTimer returns a timer that fires once after d has elapsed.
	NewTimer(d time.Duration) Timer
	// Sleep waits for d or until ctx is done, returning ctx.Err() if it was
	// interrupted.
	Sleep(ctx context.Context, d time.Duration) error
}

// Timer mirrors time.Timer with the same single-fire semantics.
//
// Callers that need the current time should read Clock.Now rather than the
// value delivered on the channel: for the system clock that value comes from
// the runtime and carries a monotonic reading and local location.
type Timer interface {
	// C returns the channel the timer fires on.
	C() <-chan time.Time
	// Stop prevents the timer from firing and reports whether it was still
	// pending.
	Stop() bool
	// Reset reschedules the timer for d from now and reports whether it was
	// still pending.
	Reset(d time.Duration) bool
}

// System returns the production clock.
func System() Clock { return systemClock{} }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

func (systemClock) NewTimer(d time.Duration) Timer { return &systemTimer{timer: time.NewTimer(d)} }

func (systemClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type systemTimer struct{ timer *time.Timer }

func (t *systemTimer) C() <-chan time.Time        { return t.timer.C }
func (t *systemTimer) Stop() bool                 { return t.timer.Stop() }
func (t *systemTimer) Reset(d time.Duration) bool { return t.timer.Reset(d) }
