package cloudwatch

import (
	"fmt"
	"math/rand/v2"
	"time"
)

// Defaults for AWS throttling. They are starting points, not measurements: a
// production value belongs to the account's real request quota.
const (
	DefaultBackoffBase = 200 * time.Millisecond
	DefaultBackoffMax  = 20 * time.Second
	// DefaultBackoffAttempts bounds one page read. Retrying forever inside a
	// cycle would hold the whole group's progress behind one throttled page,
	// and the caller's next cycle is itself a retry.
	DefaultBackoffAttempts = 5
)

// Backoff is exponential backoff with full jitter.
//
// Full jitter draws the delay from the whole window rather than from its upper
// half. Every replica throttled at the same instant would otherwise wake at the
// same instant and be throttled together again, which is the failure the jitter
// exists to break up.
type Backoff struct {
	Base        time.Duration
	Max         time.Duration
	MaxAttempts int
	// Jitter returns a delay inside [0, window]. A test injects a deterministic
	// one; production uses the package's own source.
	Jitter func(window time.Duration) time.Duration
}

func (b Backoff) base() time.Duration {
	if b.Base <= 0 {
		return DefaultBackoffBase
	}
	return b.Base
}

func (b Backoff) max() time.Duration {
	if b.Max <= 0 {
		return DefaultBackoffMax
	}
	return b.Max
}

// Attempts is how many times one read may be tried before it is reported as
// unavailable.
func (b Backoff) Attempts() int {
	if b.MaxAttempts <= 0 {
		return DefaultBackoffAttempts
	}
	return b.MaxAttempts
}

// Window is the upper bound on the delay before attempt n, counting from 1. It
// doubles per attempt and is capped, so a long outage does not produce a delay
// measured in hours.
func (b Backoff) Window(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	window := b.base()
	maximum := b.max()
	for i := 1; i < attempt; i++ {
		if window >= maximum/2 {
			return maximum
		}
		window *= 2
	}
	if window > maximum {
		return maximum
	}
	return window
}

// Delay is the jittered wait before attempt n.
func (b Backoff) Delay(attempt int) time.Duration {
	window := b.Window(attempt)
	if b.Jitter != nil {
		return b.Jitter(window)
	}
	if window <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(window) + 1))
}

// Validate rejects a configuration that would either never wait or never stop.
func (b Backoff) Validate() error {
	if b.Base < 0 || b.Max < 0 {
		return fmt.Errorf("%w: a backoff duration cannot be negative", ErrInvalidConfig)
	}
	if b.Base > 0 && b.Max > 0 && b.Max < b.Base {
		return fmt.Errorf("%w: the backoff cap is below its first delay", ErrInvalidConfig)
	}
	if b.MaxAttempts < 0 {
		return fmt.Errorf("%w: a negative attempt budget never retries", ErrInvalidConfig)
	}
	return nil
}
