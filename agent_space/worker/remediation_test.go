package worker

import (
	"context"
	"sync"
	"testing"

	"agent_space/agent"
	"agent_space/remediation"
)

// records what it was handed, and how far the worker had got when it was
type fakeRemediator struct {
	mu       sync.Mutex
	requests []remediation.Request
	// deletedWhenCalled is how many messages had been acknowledged by the time
	// the handoff happened, which is the ordering that matters here
	deletedWhenCalled []int
	queue             *fakeQueue

	accept bool
}

func (r *fakeRemediator) Enqueue(req remediation.Request) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.requests = append(r.requests, req)
	r.deletedWhenCalled = append(r.deletedWhenCalled, r.queue.deletedCount())
	return r.accept
}

func (r *fakeRemediator) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func hotfixResult() agent.Result {
	return agent.Result{
		Answer:     "HOTFIX the discount clamp introduced in 0c6f0ae.",
		Verdict:    agent.Verdict{Decision: agent.DecisionHotfix, CommitSHA: "0c6f0ae", Source: agent.SourceDeclared},
		Iterations: 3,
	}
}

func TestHandleHandsAHotfixToRemediation(t *testing.T) {
	q := &fakeQueue{}
	rem := &fakeRemediator{queue: q, accept: true}

	w := newWorker(q, fakeResolver{ctx: resolvedContext()}, &fakeAgent{result: hotfixResult()})
	w.Remediation = rem

	w.handle(context.Background(), message(sampleBody, 1))

	if rem.count() != 1 {
		t.Fatalf("remediation was handed %d requests, want 1", rem.count())
	}

	req := rem.requests[0]
	if req.InvestigationID != sampleInvestigation {
		t.Errorf("investigation_id = %q", req.InvestigationID)
	}
	if req.CommitSHA != "0c6f0ae" {
		t.Errorf("commit_sha = %q, want the one the verdict implicated", req.CommitSHA)
	}
	// the stored context wins over the envelope, the same way the question does
	if req.ServiceID != "payment" {
		t.Errorf("service_id = %q", req.ServiceID)
	}
	if req.IncidentSummary == "" || req.VerdictAnswer == "" {
		t.Errorf("a fix would be written with no incident to go on: %+v", req)
	}
}

// remediation is N containers at a fifteen-minute ceiling; holding the message
// for it would stop the queue being consumed while the detector keeps producing
func TestHandleAcknowledgesBeforeHandingOver(t *testing.T) {
	q := &fakeQueue{}
	rem := &fakeRemediator{queue: q, accept: true}

	w := newWorker(q, fakeResolver{ctx: resolvedContext()}, &fakeAgent{result: hotfixResult()})
	w.Remediation = rem

	w.handle(context.Background(), message(sampleBody, 1))

	if len(rem.deletedWhenCalled) != 1 {
		t.Fatalf("remediation was handed %d requests, want 1", len(rem.deletedWhenCalled))
	}
	if rem.deletedWhenCalled[0] != 1 {
		t.Error("the message had not been acknowledged when remediation was handed the verdict")
	}
}

// a rollback is not a code change, and neither is a run that never decided
func TestHandleDoesNotRemediateWhatIsNotAFix(t *testing.T) {
	cases := []struct {
		name   string
		result agent.Result
	}{
		{
			"rollback",
			agent.Result{
				Answer:  "ROLLBACK 7e91d04 immediately.",
				Verdict: agent.Verdict{Decision: agent.DecisionRollback, CommitSHA: "7e91d04"},
			},
		},
		{
			"no decision",
			agent.Result{
				Answer:  "The evidence is inconclusive.",
				Verdict: agent.Verdict{Decision: agent.DecisionUnknown, Source: agent.SourceAbsent},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQueue{}
			rem := &fakeRemediator{queue: q, accept: true}

			w := newWorker(q, fakeResolver{ctx: resolvedContext()}, &fakeAgent{result: tc.result})
			w.Remediation = rem

			w.handle(context.Background(), message(sampleBody, 1))

			if rem.count() != 0 {
				t.Errorf("a %s was handed to remediation", tc.name)
			}
			// and it is still an ordinary success from the queue's point of view
			if q.deletedCount() != 1 {
				t.Errorf("the message was not acknowledged")
			}
		})
	}
}

// a refused handoff costs the fixes and nothing else: the verdict is already
// persisted and the message is already gone
func TestHandleSurvivesARefusedHandoff(t *testing.T) {
	q := &fakeQueue{}
	rem := &fakeRemediator{queue: q, accept: false}

	w := newWorker(q, fakeResolver{ctx: resolvedContext()}, &fakeAgent{result: hotfixResult()})
	w.Remediation = rem

	w.handle(context.Background(), message(sampleBody, 1))

	if q.deletedCount() != 1 {
		t.Errorf("the message was not acknowledged after remediation refused it")
	}
	if rec, ok := w.Results.Get(sampleInvestigation); !ok || rec.Status != StatusDone {
		t.Errorf("the verdict was lost when remediation refused the handoff: %+v", rec)
	}
}

// the worker without a remediator is the worker as it was
func TestHandleWithoutARemediatorIsUnchanged(t *testing.T) {
	q := &fakeQueue{}
	w := newWorker(q, fakeResolver{ctx: resolvedContext()}, &fakeAgent{result: hotfixResult()})

	w.handle(context.Background(), message(sampleBody, 1))

	if q.deletedCount() != 1 {
		t.Errorf("the message was not acknowledged")
	}
	if rec, ok := w.Results.Get(sampleInvestigation); !ok || rec.Status != StatusDone {
		t.Errorf("record = %+v", rec)
	}
}
