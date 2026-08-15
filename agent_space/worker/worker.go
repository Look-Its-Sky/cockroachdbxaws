package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent_space/agent"
	"agent_space/incident"
	"agent_space/remediation"
	"agent_space/utils/queue"
)

const (
	resolveTimeout = 15 * time.Second
	ackTimeout     = 10 * time.Second

	// receive error backoff
	minBackoff = 2 * time.Second
	maxBackoff = 30 * time.Second
)

// the part of agent.Runner the worker uses, named so tests can substitute it
type Investigator interface {
	Run(ctx context.Context, question string, limit int) (agent.Result, error)
}

// ContextResolver fetches the prose an assignment points at.
type ContextResolver interface {
	Resolve(ctx context.Context, a queue.Assignment) (incident.Context, error)
}

// where a verdict goes next. Accepting or refusing has to be immediate: this is
// called on the receive loop, and proposing fixes is minutes of container time.
type Remediator interface {
	Enqueue(req remediation.Request) bool
}

// Worker polls one queue and investigates one message at a time.
type Worker struct {
	Queue    queue.Receiver
	Resolver ContextResolver
	Agent    Investigator
	Results  *Store
	Config   queue.Config
	// proposes fixes for a verdict; nil stops at the verdict, which
	// is what running without a container runtime looks like.
	Remediation Remediator

	// how many past incidents to recall per run; 0 uses the agent's default
	Sources int
}

// polls until ctx is cancelled, never returning an error: the API it runs
// beside must stay up regardless of a bad receive
func (w *Worker) Run(ctx context.Context) {
	log.Printf("worker: polling %s every %s (visibility %s, giving up after %d deliveries)",
		w.Config.QueueURL, w.Config.WaitTime, w.Config.VisibilityTimeout, w.Config.MaxAttempts)

	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			log.Println("worker: stopped")
			return
		}

		msgs, err := w.Queue.Receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				log.Println("worker: stopped")
				return
			}
			log.Printf("worker: receive failed, retrying in %s: %v", backoff, err)
			if !sleep(ctx, backoff) {
				log.Println("worker: stopped")
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = minBackoff

		for _, msg := range msgs {
			w.handle(ctx, msg)
		}
	}
}

// one message, start to finish. Every path either acknowledges the message or
// deliberately leaves it for redelivery; there is no path that does neither.
func (w *Worker) handle(ctx context.Context, msg queue.Message) {
	a, err := queue.ParseAssignment(msg.Body)
	if err != nil {
		// a body that will not parse now will not parse in five minutes
		log.Printf("worker: dropping unusable message %s: %v", msg.MessageID, err)
		if a.InvestigationID != "" {
			w.Results.Fail(a, err.Error())
		}
		w.ack(msg)
		return
	}

	if w.Results.Seen(a.InvestigationID) {
		log.Printf("worker: %s already investigated (dedup key %s), acknowledging the duplicate",
			a.InvestigationID, msg.DeduplicationKey())
		w.ack(msg)
		return
	}

	// SQS counts this delivery, so at MaxAttempts the message has already had
	// that many chances. Stop, rather than let it cycle until the DLQ takes it.
	if attempts := msg.ReceiveCount(); attempts >= w.Config.MaxAttempts {
		reason := fmt.Sprintf("gave up after %d deliveries", attempts)
		log.Printf("worker: %s: %s", a, reason)
		w.Results.Fail(a, reason)
		w.ack(msg)
		return
	}

	resolveCtx, cancelResolve := context.WithTimeout(ctx, resolveTimeout)
	inc, err := w.Resolver.Resolve(resolveCtx, a)
	cancelResolve()
	if err != nil {
		// the producer may enqueue before committing the context row, so a
		// missing one is a race to wait out; anything else is a real fault,
		// which from the queue's side looks identical unless it is said
		if errors.Is(err, incident.ErrNotFound) {
			log.Printf("worker: %s: no context row yet, leaving for redelivery", a)
		} else {
			log.Printf("worker: %s: could not read incident context, leaving for redelivery: %v", a, err)
		}
		w.Results.Retry(a, err.Error())
		return
	}

	log.Printf("worker: %s: starting", a)
	w.Results.Start(a)

	stopHeartbeat := w.heartbeat(ctx, msg.ReceiptHandle)

	// derived from the worker's context, never from a request: a run must not
	// die because whoever asked for it went away
	runCtx, cancelRun := context.WithTimeout(ctx, w.Config.RunTimeout)
	res, runErr := w.Agent.Run(runCtx, incident.BuildQuestion(a, inc), w.Sources)
	cancelRun()
	stopHeartbeat()

	switch {
	case runErr != nil && errors.Is(runErr, agent.ErrTransport):
		// MCP was unreachable, which the next delivery may well not hit
		log.Printf("worker: %s: MCP unreachable after %d iterations, leaving for redelivery: %v",
			a, res.Iterations, runErr)
		w.Results.Retry(a, runErr.Error())
		return

	case runErr != nil:
		// re-running a failing model call five times costs money and fails the
		// same way, so this is terminal and the trace is what is left
		log.Printf("worker: %s: failed after %d iterations: %v", a, res.Iterations, runErr)
		w.Results.Finish(a, res, runErr)
		w.ack(msg)
		return
	}

	log.Printf("worker: %s: done in %d iterations, %d sources%s",
		a, res.Iterations, res.Sources, truncatedNote(res.Truncated))
	w.Results.Finish(a, res, nil)

	// acked before the handoff, never after: holding the message through N
	// fifteen-minute containers would stop the queue being consumed
	w.ack(msg)
	w.remediate(a, inc, res)
}

// hands the verdict to the remediation pool, best effort: the verdict is
// already persisted, so a refused handoff costs the fixes and nothing else
func (w *Worker) remediate(a queue.Assignment, inc incident.Context, res agent.Result) {
	if w.Remediation == nil {
		return
	}

	// a run that never named a commit gives triage nothing to read, and a
	// rollback is not a code change, so neither is worth a container
	if !res.Verdict.Decided() {
		log.Printf("worker: %s: no decision was reached, not proposing fixes", a)
		return
	}
	if res.Verdict.Decision != agent.DecisionHotfix {
		log.Printf("worker: %s: verdict is %s, which is not a code change; not proposing fixes",
			a, res.Verdict.Decision)
		return
	}

	if w.Remediation.Enqueue(remediation.Request{
		InvestigationID: a.InvestigationID,
		IncidentID:      a.IncidentID,
		ServiceID:       first(inc.ServiceID, a.ServiceID, res.Verdict.Service),
		IncidentSummary: incident.BuildQuestion(a, inc),
		VerdictAnswer:   res.Answer,
		CommitSHA:       res.Verdict.CommitSHA,
	}) {
		log.Printf("worker: %s: handed to remediation", a)
	}
}

func first(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// renews the lease while a run is in flight; without it a 66s run outlives the
// visibility timeout and SQS hands the same investigation to the next poll
func (w *Worker) heartbeat(ctx context.Context, receiptHandle string) (stop func()) {
	timeout := w.Config.VisibilityTimeout
	interval := timeout / 3
	if interval <= 0 {
		interval = queue.DefaultVisibilityTimeout / 3
	}

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := w.Queue.ExtendVisibility(ctx, receiptHandle, int32(timeout.Seconds())); err != nil {
					// losing the lease is not worth abandoning the run over;
					// the duplicate is caught by the dedup check
					log.Printf("worker: could not extend visibility: %v", err)
				}
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

// acknowledge with a context of its own, so a message whose run finished during
// shutdown is still deleted rather than redelivered and investigated twice
func (w *Worker) ack(msg queue.Message) {
	ctx, cancel := context.WithTimeout(context.Background(), ackTimeout)
	defer cancel()

	if err := w.Queue.Delete(ctx, msg.ReceiptHandle); err != nil {
		log.Printf("worker: delete failed for message %s, it will be redelivered: %v", msg.MessageID, err)
	}
}

// sleep reports false if ctx was cancelled before the delay elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func truncatedNote(truncated bool) string {
	if truncated {
		return " (truncated: ran out of iterations)"
	}
	return ""
}
