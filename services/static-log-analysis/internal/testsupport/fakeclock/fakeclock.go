// Package fakeclock provides a deterministic clock.Clock for tests.
//
// Time moves only when a test advances it, so window boundaries, quiet periods,
// lateness, claim expiry, and reopen behavior are exercised at their exact
// instants instead of approximately, and a test never waits for real time.
package fakeclock

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/clock"
)

// Origin is the instant fake clocks start at unless a test chooses another. It
// is a fixed point so that golden fixtures derived from clock readings do not
// change between runs.
var Origin = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Clock is a clock.Clock whose time only changes through Advance and AdvanceTo.
//
// Now is safe to call from any goroutine. Advance and AdvanceTo are intended for
// the test goroutine that is driving the scenario.
type Clock struct {
	mu      sync.Mutex
	changed *sync.Cond
	now     time.Time
	seq     uint64
	waiters []*waiter
}

// waiter is one pending timer or sleep.
type waiter struct {
	deadline time.Time
	// seq breaks deadline ties so that timers created earlier fire earlier.
	seq     uint64
	ch      chan time.Time
	pending bool
}

// New returns a clock started at start, which must be a non-zero UTC time.
func New(start time.Time) *Clock {
	if start.IsZero() {
		panic("fakeclock: start must not be the zero time")
	}
	if start.Location() != time.UTC {
		panic("fakeclock: start must be UTC, got " + start.Location().String())
	}
	c := &Clock{now: start}
	c.changed = sync.NewCond(&c.mu)
	return c
}

// NewAtOrigin returns a clock started at Origin.
func NewAtOrigin() *Clock { return New(Origin) }

var _ clock.Clock = (*Clock)(nil)

// Now returns the current fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves time forward by d, firing every timer whose deadline is reached.
//
// A timer whose deadline is exactly the instant reached does fire: deadlines are
// inclusive, matching the half-open window convention where a record at the
// watermark is included.
func (c *Clock) Advance(d time.Duration) {
	if d < 0 {
		panic(fmt.Sprintf("fakeclock: cannot advance by negative duration %s; time never moves backward", d))
	}
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	c.AdvanceTo(target)
}

// AdvanceTo moves time forward to target, firing every timer whose deadline it
// reaches, in deadline order.
//
// Each timer is fired while the clock reads exactly its own deadline, so a
// timer created from within a fired handler is scheduled relative to that
// deadline rather than to the end of the advance. A handler running on another
// goroutine may not be scheduled until the advance has finished, so handlers
// should use the instant delivered on the channel rather than reading Now.
func (c *Clock) AdvanceTo(target time.Time) {
	if target.Location() != time.UTC {
		panic("fakeclock: target must be UTC, got " + target.Location().String())
	}
	c.mu.Lock()
	if target.Before(c.now) {
		now := c.now
		c.mu.Unlock()
		panic(fmt.Sprintf("fakeclock: cannot move time backward from %s to %s", now.Format(time.RFC3339Nano), target.Format(time.RFC3339Nano)))
	}

	for {
		next := c.earliestDueLocked(target)
		if next == nil {
			break
		}
		c.now = next.deadline
		c.removeLocked(next)
		next.pending = false
		fireTime := next.deadline
		// Fire without the lock so a handler may create or stop timers.
		c.mu.Unlock()
		select {
		case next.ch <- fireTime:
		default:
		}
		c.mu.Lock()
	}

	if target.After(c.now) {
		c.now = target
	}
	c.changed.Broadcast()
	c.mu.Unlock()
}

// earliestDueLocked returns the pending waiter with the smallest deadline at or
// before target, or nil.
func (c *Clock) earliestDueLocked(target time.Time) *waiter {
	var earliest *waiter
	for _, w := range c.waiters {
		if w.deadline.After(target) {
			continue
		}
		if earliest == nil || w.deadline.Before(earliest.deadline) ||
			(w.deadline.Equal(earliest.deadline) && w.seq < earliest.seq) {
			earliest = w
		}
	}
	return earliest
}

// NewTimer returns a timer that fires once the clock reaches now+d. A
// non-positive duration fires on the next advance of zero or more, matching a
// deadline that has already passed.
func (c *Clock) NewTimer(d time.Duration) clock.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &Timer{clock: c, waiter: c.addWaiterLocked(d)}
}

// Sleep blocks until d has elapsed on the fake clock or ctx is done.
func (c *Clock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	c.mu.Lock()
	w := c.addWaiterLocked(d)
	c.mu.Unlock()

	select {
	case <-ctx.Done():
		c.mu.Lock()
		c.removeLocked(w)
		c.changed.Broadcast()
		c.mu.Unlock()
		return ctx.Err()
	case <-w.ch:
		return nil
	}
}

func (c *Clock) addWaiterLocked(d time.Duration) *waiter {
	c.seq++
	w := &waiter{
		deadline: c.now.Add(d),
		seq:      c.seq,
		ch:       make(chan time.Time, 1),
		pending:  true,
	}
	c.waiters = append(c.waiters, w)
	c.changed.Broadcast()
	return w
}

func (c *Clock) removeLocked(target *waiter) {
	for i, w := range c.waiters {
		if w == target {
			c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
			return
		}
	}
}

// WaiterCount returns how many timers and sleeps are currently pending.
func (c *Clock) WaiterCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// BlockUntil waits until at least n timers or sleeps are pending.
//
// A test uses this to reach a known point before advancing: without it, a test
// can advance past a deadline before the goroutine under test has registered
// its timer, and the scenario silently stops being the one that was intended.
func (c *Clock) BlockUntil(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.waiters) < n {
		c.changed.Wait()
	}
}

// PendingDeadlines returns the pending deadlines in fire order, for diagnostics.
func (c *Clock) PendingDeadlines() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	pending := append([]*waiter(nil), c.waiters...)
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].deadline.Equal(pending[j].deadline) {
			return pending[i].seq < pending[j].seq
		}
		return pending[i].deadline.Before(pending[j].deadline)
	})
	deadlines := make([]time.Time, 0, len(pending))
	for _, w := range pending {
		deadlines = append(deadlines, w.deadline)
	}
	return deadlines
}

// Timer is a clock.Timer bound to a fake Clock.
type Timer struct {
	clock  *Clock
	waiter *waiter
}

var _ clock.Timer = (*Timer)(nil)

func (t *Timer) C() <-chan time.Time { return t.waiter.ch }

// Stop prevents the timer from firing and reports whether it was still pending.
func (t *Timer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if !t.waiter.pending {
		return false
	}
	t.waiter.pending = false
	t.clock.removeLocked(t.waiter)
	t.clock.changed.Broadcast()
	return true
}

// Reset reschedules the timer for d from the current fake time and reports
// whether it was still pending. As with time.Timer, a caller that resets an
// already fired timer is responsible for draining its channel.
func (t *Timer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasPending := t.waiter.pending
	if wasPending {
		t.clock.removeLocked(t.waiter)
	}
	// The waiter is rescheduled in place rather than replaced, so a caller that
	// captured C() before the reset keeps receiving on the same channel.
	t.clock.seq++
	t.waiter.deadline = t.clock.now.Add(d)
	t.waiter.seq = t.clock.seq
	t.waiter.pending = true
	t.clock.waiters = append(t.clock.waiters, t.waiter)
	t.clock.changed.Broadcast()
	return wasPending
}
