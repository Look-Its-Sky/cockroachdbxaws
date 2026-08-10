package worker

import (
	"context"
	"errors"
	"sync"
	"testing"

	"agent_space/agent"
	"agent_space/utils/queue"
)

// an in-memory Journal, optionally broken
type fakeJournal struct {
	mu    sync.Mutex
	saved map[string]Record
	saves int

	saveErr error
	loadErr error
}

func newFakeJournal() *fakeJournal {
	return &fakeJournal{saved: make(map[string]Record)}
}

func (j *fakeJournal) Save(_ context.Context, rec Record) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.saves++
	if j.saveErr != nil {
		return j.saveErr
	}
	j.saved[rec.InvestigationID] = rec
	return nil
}

func (j *fakeJournal) Load(_ context.Context, id string) (Record, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.loadErr != nil {
		return Record{}, false, j.loadErr
	}
	rec, ok := j.saved[id]
	return rec, ok, nil
}

func (j *fakeJournal) saveCount() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.saves
}

func TestDurableStoreMirrorsVerdicts(t *testing.T) {
	j := newFakeJournal()
	s := NewDurableStore(j)
	a := queue.Assignment{InvestigationID: "v1", IncidentID: "i1", ServiceID: "payment"}

	s.Start(a)
	s.Finish(a, agent.Result{Answer: "ROLLBACK 7e91d04", Iterations: 3}, nil)

	rec, ok, err := j.Load(context.Background(), "v1")
	if err != nil || !ok {
		t.Fatalf("verdict was not persisted (ok=%v, err=%v)", ok, err)
	}
	if rec.Status != StatusDone {
		t.Errorf("persisted status = %q, want %q", rec.Status, StatusDone)
	}
	// the trace is the audit trail; a verdict without it is worth much less
	if rec.Result.Answer != "ROLLBACK 7e91d04" || rec.Result.Iterations != 3 {
		t.Errorf("persisted result = %+v", rec.Result)
	}
	if rec.FinishedAt == nil {
		t.Error("persisted record has no finished_at")
	}
}

func TestDurableStoreReadsThroughAfterRestart(t *testing.T) {
	j := newFakeJournal()

	// a process that ran an investigation, then went away
	before := NewDurableStore(j)
	a := queue.Assignment{InvestigationID: "v1", IncidentID: "i1"}
	before.Start(a)
	before.Finish(a, agent.Result{Answer: "HOTFIX"}, nil)

	// a fresh process with an empty map, same journal
	after := NewDurableStore(j)

	rec, ok := after.Get("v1")
	if !ok {
		t.Fatal("the verdict did not survive the restart")
	}
	if rec.Result.Answer != "HOTFIX" {
		t.Errorf("answer = %q", rec.Result.Answer)
	}
	// and the point of it: a redelivery must not be investigated again
	if !after.Seen("v1") {
		t.Error("Seen() is false after a restart; the message would be re-investigated")
	}
}

func TestDurableStoreEvictedRecordIsStillReadable(t *testing.T) {
	j := newFakeJournal()
	s := NewDurableStore(j)
	s.capacity = 1

	first := queue.Assignment{InvestigationID: "v1", IncidentID: "i1"}
	s.Start(first)
	s.Finish(first, agent.Result{Answer: "ROLLBACK"}, nil)
	s.Fail(queue.Assignment{InvestigationID: "v2", IncidentID: "i2"}, "nope")

	if _, inMemory := s.byID["v1"]; inMemory {
		t.Fatal("v1 was not evicted; the test proves nothing")
	}
	rec, ok := s.Get("v1")
	if !ok || rec.Result.Answer != "ROLLBACK" {
		t.Errorf("an evicted verdict should still be readable from the journal, got ok=%v rec=%+v", ok, rec)
	}
}

func TestRetryingRecordIsNotTreatedAsSeenAcrossRestart(t *testing.T) {
	j := newFakeJournal()

	before := NewDurableStore(j)
	a := queue.Assignment{InvestigationID: "v1", IncidentID: "i1"}
	before.Retry(a, "no context row yet")

	// the work still has to happen, so a new process must pick it up
	after := NewDurableStore(j)
	if after.Seen("v1") {
		t.Error("a persisted retrying record suppressed the redelivery")
	}
}

func TestBrokenJournalDoesNotBreakARun(t *testing.T) {
	j := newFakeJournal()
	j.saveErr = errors.New("connection refused")
	j.loadErr = errors.New("connection refused")

	q := &fakeQueue{}
	a := &fakeAgent{result: agent.Result{Answer: "ROLLBACK 7e91d04"}}
	w := newWorker(q, fakeResolver{ctx: resolvedContext()}, a)
	w.Results = NewDurableStore(j)

	w.handle(context.Background(), message(sampleBody, 1))

	// losing durability must cost durability and nothing else
	if a.callCount() != 1 {
		t.Errorf("agent ran %d times, want 1", a.callCount())
	}
	if q.deletedCount() != 1 {
		t.Errorf("message deleted %d times, want 1", q.deletedCount())
	}

	rec, ok := w.Results.Get(sampleInvestigation)
	if !ok || rec.Status != StatusDone {
		t.Errorf("in-memory record should still be correct, got ok=%v status=%q", ok, rec.Status)
	}
	if j.saveCount() == 0 {
		t.Error("no save was attempted")
	}
}

func TestStoreWithoutJournalIsUnchanged(t *testing.T) {
	s := NewStore()
	a := queue.Assignment{InvestigationID: "v1", IncidentID: "i1"}

	s.Start(a)
	s.Finish(a, agent.Result{Answer: "ROLLBACK"}, nil)

	if !s.Seen("v1") {
		t.Error("Seen() should be true for a completed investigation")
	}
	if _, ok := s.Get("nope"); ok {
		t.Error("Get() should miss for an unknown id")
	}
}
