package runtime

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/clock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/cloudwatch"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/outbox"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/pipeline"
)

// ErrForcedStop means a component was still busy when the shutdown budget ran
// out and was cut off. It stays safe by construction: an acknowledged record is
// already synchronized in the journal, and an abandoned cohort's claims lapse
// and are claimed again.
var ErrForcedStop = errors.New("runtime: forced stop")

// processor is the coordinator half the worker drives. *pipeline.Service
// satisfies it.
type processor interface {
	Process(context.Context, int) (pipeline.ProcessResult, error)
}

// publisher is the outbox half the worker drives. *outbox.Publisher satisfies
// it.
type publisher interface {
	PublishCycle(context.Context) (outbox.Result, error)
}

// poller is the source half a source replica drives. *cloudwatch.Adapter
// satisfies it.
type poller interface {
	Poll(context.Context) (cloudwatch.PollResult, error)
}

// cycleFunc is one unit of periodic work. worked reports that durable state
// changed, which is what tells a productive cadence from an idle one.
//
// The process worker and the outbox publisher have the same shape here on
// purpose: both claim a bounded batch, both must not spin when there is nothing
// to claim, and both must widen their pause while their dependency is down.
type cycleFunc func(context.Context) (worked bool, err error)

func processCycle(p processor, cohort int) cycleFunc {
	return func(ctx context.Context) (bool, error) {
		result, err := p.Process(ctx, cohort)
		// A cycle that only quarantined records still did work: it changed
		// durable state, and there may be more like it waiting.
		return result.Claimed > 0 || result.Processed > 0 || result.Quarantined > 0, err
	}
}

func pollCycle(p poller) cycleFunc {
	return func(ctx context.Context) (bool, error) {
		result, err := p.Poll(ctx)
		// Reading events that all turned out to be duplicates is not work: the
		// overlap reread them on purpose and nothing changed. Delivering a
		// record or advancing a checkpoint is.
		return result.Delivered > 0 || result.Checkpoints > 0, err
	}
}

func publishCycle(p publisher) cycleFunc {
	return func(ctx context.Context) (bool, error) {
		result, err := p.PublishCycle(ctx)
		// Claiming anything is work even when every message then failed: the
		// outbox changed, and the next batch may be waiting behind it.
		return result.Claimed > 0, err
	}
}

// worker advances the journal on a bounded cadence. It never runs two cycles at
// once and never runs one back to back, so an idle journal costs one wakeup per
// idle interval rather than a spinning core, and a failing database is retried
// with a widening pause instead of continuously.
type worker struct {
	cycle  cycleFunc
	name   string
	config WorkerConfig
	clock  clock.Clock
	logger *slog.Logger

	// cycleCtx is deliberately not derived from any request or signal context.
	// A cycle inside a CockroachDB transaction is cancelled only by cancel,
	// which stopAndWait calls only after the shutdown budget is spent.
	cycleCtx context.Context
	cancel   context.CancelFunc
	// stop is closed to ask the loop to finish after its current cycle. It also
	// wakes the loop out of its pause.
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

func newWorker(p processor, config WorkerConfig, clk clock.Clock, logger *slog.Logger) *worker {
	return newCycleWorker(processCycle(p, config.Cohort), "process", config, clk, logger)
}

func newPublishWorker(p publisher, config WorkerConfig, clk clock.Clock, logger *slog.Logger) *worker {
	return newCycleWorker(publishCycle(p), "outbox", config, clk, logger)
}

func newPollWorker(p poller, config WorkerConfig, clk clock.Clock, logger *slog.Logger) *worker {
	return newCycleWorker(pollCycle(p), "source", config, clk, logger)
}

func newCycleWorker(cycle cycleFunc, name string, config WorkerConfig, clk clock.Clock, logger *slog.Logger) *worker {
	ctx, cancel := context.WithCancel(context.Background())
	return &worker{cycle: cycle, name: name, config: config, clock: clk, logger: logger,
		cycleCtx: ctx, cancel: cancel, stop: make(chan struct{}), done: make(chan struct{})}
}

func (w *worker) start() { go w.run() }

func (w *worker) run() {
	defer close(w.done)
	backoff := time.Duration(0)
	for {
		select {
		case <-w.stop:
			return
		default:
		}
		worked, err := w.cycle(w.cycleCtx)
		pause := w.config.IdleInterval
		switch {
		case err != nil:
			if backoff == 0 {
				backoff = w.config.BackoffMin
			} else if backoff = backoff * 2; backoff > w.config.BackoffMax {
				backoff = w.config.BackoffMax
			}
			pause = backoff
			// The error text is a categorical failure from the coordinator; no
			// record content ever reaches it.
			w.logger.Error(w.name+" cycle failed", slog.String("error", err.Error()), slog.Duration("retry_in", pause))
		case worked:
			backoff = 0
			pause = w.config.Interval
		default:
			backoff = 0
		}
		if err := w.pause(pause); err != nil {
			return
		}
	}
}

// pause waits out the cadence, returning early only when a stop is requested.
func (w *worker) pause(d time.Duration) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-w.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	return w.clock.Sleep(ctx, d)
}

// stopAndWait stops claiming new work and waits for the cycle in flight. It
// returns ErrForcedStop if the budget expired first, having cancelled the cycle
// so an unresponsive database cannot hold the process open forever.
func (w *worker) stopAndWait(ctx context.Context) error {
	w.stopOnce.Do(func() { close(w.stop) })
	select {
	case <-w.done:
		w.cancel()
		return nil
	case <-ctx.Done():
		// The budget is spent. Cancelling is all a forced stop can do: waiting
		// for the cycle here would make an unresponsive database able to hold
		// the process open past its own shutdown deadline, which is exactly the
		// situation the deadline exists for.
		w.cancel()
		return ErrForcedStop
	}
}
