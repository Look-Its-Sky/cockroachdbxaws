package runtime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/pipeline"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
)

// scriptedProcessor answers one outcome per cycle and records the context each
// cycle ran under, so a test can tell a cycle that was allowed to finish from
// one that was cut short.
type scriptedProcessor struct {
	mu       sync.Mutex
	script   []func() (pipeline.ProcessResult, error)
	calls    int
	limits   []int
	contexts []context.Context
	entered  chan struct{}
	release  chan struct{}
}

func (p *scriptedProcessor) Process(ctx context.Context, limit int) (pipeline.ProcessResult, error) {
	p.mu.Lock()
	index := p.calls
	p.calls++
	p.limits = append(p.limits, limit)
	p.contexts = append(p.contexts, ctx)
	script := p.script
	p.mu.Unlock()
	if p.entered != nil {
		p.entered <- struct{}{}
		<-p.release
	}
	if index < len(script) {
		return script[index]()
	}
	return pipeline.ProcessResult{}, nil
}

func (p *scriptedProcessor) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func testWorkerConfig() WorkerConfig {
	return WorkerConfig{Cohort: 50, Interval: 250 * time.Millisecond, IdleInterval: time.Second,
		BackoffMin: time.Second, BackoffMax: 8 * time.Second}
}

func silentLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// nextPause runs the worker up to the point where it is waiting again and
// returns how long it decided to wait.
func nextPause(t *testing.T, clock *fakeclock.Clock, pending int) time.Duration {
	t.Helper()
	clock.BlockUntil(pending)
	deadlines := clock.PendingDeadlines()
	if len(deadlines) == 0 {
		t.Fatal("the worker is not waiting at all")
	}
	return deadlines[len(deadlines)-1].Sub(clock.Now())
}

func TestWorkerBacksOffExponentiallyWhileTheDatabaseIsUnavailable(t *testing.T) {
	failing := func() (pipeline.ProcessResult, error) { return pipeline.ProcessResult{}, persistence.ErrUnavailable }
	processor := &scriptedProcessor{script: []func() (pipeline.ProcessResult, error){failing, failing, failing, failing, failing}}
	clock := fakeclock.NewAtOrigin()
	worker := newWorker(processor, testWorkerConfig(), clock, silentLogger())
	worker.start()
	t.Cleanup(func() { _ = worker.stopAndWait(context.Background()) })

	// operations.md budgets a 30-minute CockroachDB outage. The worker must
	// retry the whole time without hammering an unavailable database.
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
	for cycle, expected := range want {
		if pause := nextPause(t, clock, 1); pause != expected {
			t.Fatalf("cycle %d waited %s, want %s", cycle, pause, expected)
		}
		clock.Advance(expected)
	}
}

func TestWorkerForgetsItsBackoffOnceACycleSucceeds(t *testing.T) {
	failing := func() (pipeline.ProcessResult, error) { return pipeline.ProcessResult{}, persistence.ErrUnavailable }
	working := func() (pipeline.ProcessResult, error) { return pipeline.ProcessResult{Claimed: 3, Processed: 3}, nil }
	processor := &scriptedProcessor{script: []func() (pipeline.ProcessResult, error){failing, failing, working, failing}}
	clock := fakeclock.NewAtOrigin()
	config := testWorkerConfig()
	worker := newWorker(processor, config, clock, silentLogger())
	worker.start()
	t.Cleanup(func() { _ = worker.stopAndWait(context.Background()) })

	for _, expected := range []time.Duration{config.BackoffMin, 2 * config.BackoffMin, config.Interval, config.BackoffMin} {
		if pause := nextPause(t, clock, 1); pause != expected {
			t.Fatalf("waited %s, want %s", pause, expected)
		}
		clock.Advance(expected)
	}
}

// A worker that claimed nothing must wait, not spin: an empty journal is the
// normal state between bursts and would otherwise pin a core and hammer Pebble.
func TestWorkerWaitsTheIdleIntervalWhenThereIsNothingToClaim(t *testing.T) {
	processor := &scriptedProcessor{}
	clock := fakeclock.NewAtOrigin()
	config := testWorkerConfig()
	worker := newWorker(processor, config, clock, silentLogger())
	worker.start()
	t.Cleanup(func() { _ = worker.stopAndWait(context.Background()) })

	if pause := nextPause(t, clock, 1); pause != config.IdleInterval {
		t.Fatalf("an idle cycle waited %s, want %s", pause, config.IdleInterval)
	}
	before := processor.callCount()
	time.Sleep(20 * time.Millisecond)
	if after := processor.callCount(); after != before {
		t.Fatalf("the worker ran %d further cycles without the clock moving", after-before)
	}
}

func TestWorkerReturnsPromptlyAfterACycleThatDidWork(t *testing.T) {
	working := func() (pipeline.ProcessResult, error) {
		return pipeline.ProcessResult{Claimed: 50, Processed: 50}, nil
	}
	processor := &scriptedProcessor{script: []func() (pipeline.ProcessResult, error){working}}
	clock := fakeclock.NewAtOrigin()
	config := testWorkerConfig()
	worker := newWorker(processor, config, clock, silentLogger())
	worker.start()
	t.Cleanup(func() { _ = worker.stopAndWait(context.Background()) })

	if pause := nextPause(t, clock, 1); pause != config.Interval {
		t.Fatalf("a productive cycle waited %s, want %s", pause, config.Interval)
	}
	if limits := processor.limits; len(limits) == 0 || limits[0] != config.Cohort {
		t.Fatalf("the worker claimed %v rather than the configured cohort %d", limits, config.Cohort)
	}
}

// A cycle that only quarantined records still did work: it changed durable
// state, and there may be more of the same waiting.
func TestWorkerTreatsAQuarantineOnlyCycleAsWork(t *testing.T) {
	quarantining := func() (pipeline.ProcessResult, error) {
		return pipeline.ProcessResult{Claimed: 1, Quarantined: 1}, nil
	}
	processor := &scriptedProcessor{script: []func() (pipeline.ProcessResult, error){quarantining}}
	clock := fakeclock.NewAtOrigin()
	config := testWorkerConfig()
	worker := newWorker(processor, config, clock, silentLogger())
	worker.start()
	t.Cleanup(func() { _ = worker.stopAndWait(context.Background()) })

	if pause := nextPause(t, clock, 1); pause != config.Interval {
		t.Fatalf("a quarantining cycle waited %s, want %s", pause, config.Interval)
	}
}

func TestWorkerStopsClaimingOnceStopIsRequested(t *testing.T) {
	processor := &scriptedProcessor{}
	clock := fakeclock.NewAtOrigin()
	worker := newWorker(processor, testWorkerConfig(), clock, silentLogger())
	worker.start()
	nextPause(t, clock, 1)
	// The budget is generous, so anything other than a clean stop means the loop
	// did not leave its pause when it was asked to.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := worker.stopAndWait(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	before := processor.callCount()
	clock.Advance(time.Hour)
	time.Sleep(20 * time.Millisecond)
	if after := processor.callCount(); after != before {
		t.Fatalf("the worker claimed %d more cohorts after it was stopped", after-before)
	}
}

// operations.md: shutdown finishes or checkpoints active transactions. A cycle
// already inside a CockroachDB transaction is given the whole shutdown budget
// before anything cancels it.
func TestWorkerLetsAnActiveCycleFinishWithinTheShutdownBudget(t *testing.T) {
	processor := &scriptedProcessor{entered: make(chan struct{}), release: make(chan struct{})}
	worker := newWorker(processor, testWorkerConfig(), fakeclock.NewAtOrigin(), silentLogger())
	worker.start()
	<-processor.entered

	stopped := make(chan error, 1)
	go func() { stopped <- worker.stopAndWait(context.Background()) }()
	select {
	case err := <-stopped:
		t.Fatalf("stop abandoned an active cycle: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := processor.contexts[0].Err(); err != nil {
		t.Fatalf("an active cycle was cancelled while the budget remained: %v", err)
	}
	close(processor.release)
	if err := <-stopped; err != nil {
		t.Fatalf("stop after the cycle finished: %v", err)
	}
}

// A forced stop stays safe: the claims of an abandoned cycle simply lapse and
// the work is claimed again, because nothing is committed to the journal until
// after the database transaction returns.
func TestWorkerForcesAStuckCycleWhenTheBudgetExpires(t *testing.T) {
	processor := &scriptedProcessor{entered: make(chan struct{}), release: make(chan struct{})}
	worker := newWorker(processor, testWorkerConfig(), fakeclock.NewAtOrigin(), silentLogger())
	worker.start()
	<-processor.entered

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := worker.stopAndWait(ctx)
	if !errors.Is(err, ErrForcedStop) {
		t.Fatalf("a stuck cycle stopped cleanly: %v", err)
	}
	if cycleErr := processor.contexts[0].Err(); cycleErr == nil {
		t.Fatal("the stuck cycle's context was never cancelled, so nothing would ever release it")
	}
	close(processor.release)
}
