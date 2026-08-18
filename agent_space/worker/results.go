package worker

import (
	"context"
	"log"
	"sync"
	"time"

	"agent_space/agent"
	"agent_space/utils/queue"
)

// how many finished investigations to keep. A run's trace is a few KB, so this
// is small; it exists to bound a process that runs for days, not to be a cache.
const defaultCapacity = 200

// where an investigation has got to
type Status string

const (
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
	// the attempt failed for a reason that redelivery may fix, so the message
	// was left on the queue and this investigation will be tried again
	StatusRetrying Status = "retrying"
)

// one investigation, as returned by GET /agent/:id
type Record struct {
	InvestigationID string       `json:"investigation_id"`
	IncidentID      string       `json:"incident_id"`
	CorrelationID   string       `json:"correlation_id,omitempty"`
	ServiceID       string       `json:"service_id,omitempty"`
	Status          Status       `json:"status"`
	Result          agent.Result `json:"result,omitempty"`
	Error           string       `json:"error,omitempty"`
	StartedAt       time.Time    `json:"started_at"`
	FinishedAt      *time.Time   `json:"finished_at,omitempty"`
}

// verdicts by investigation id: a bounded map for a running process, in front
// of an optional journal that outlives it. Failures there are swallowed, since
// losing durability must not cost an investigation.
type Store struct {
	mu       sync.RWMutex
	byID     map[string]*Record
	order    []string // insertion order, for eviction
	capacity int

	journal Journal
}

func NewStore() *Store {
	return &Store{byID: make(map[string]*Record), capacity: defaultCapacity}
}

// NewDurableStore mirrors every record into journal.
func NewDurableStore(journal Journal) *Store {
	s := NewStore()
	s.journal = journal
	return s
}

// how long a mirror write or a read-through may take. Short: the answer is
// already in hand, this is only about keeping it.
const journalTimeout = 10 * time.Second

// mirror a record, best effort. Called with the lock released.
func (s *Store) mirror(rec Record) {
	if s.journal == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), journalTimeout)
	defer cancel()

	if err := s.journal.Save(ctx, rec); err != nil {
		log.Printf("worker: could not persist investigation %s, it will not survive a restart: %v",
			rec.InvestigationID, err)
	}
}

// look a record up in the journal, best effort.
func (s *Store) recall(investigationID string) (Record, bool) {
	if s.journal == nil {
		return Record{}, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), journalTimeout)
	defer cancel()

	rec, found, err := s.journal.Load(ctx, investigationID)
	if err != nil {
		log.Printf("worker: could not read investigation %s from the journal: %v", investigationID, err)
		return Record{}, false
	}
	return rec, found
}

// records an investigation as running
func (s *Store) Start(a queue.Assignment) {
	s.put(&Record{
		InvestigationID: a.InvestigationID,
		IncidentID:      a.IncidentID,
		CorrelationID:   a.CorrelationID,
		ServiceID:       a.ServiceID,
		Status:          StatusRunning,
		StartedAt:       time.Now().UTC(),
	})
}

// the outcome, whether or not the run reached an answer
func (s *Store) Finish(a queue.Assignment, res agent.Result, runErr error) {
	s.mu.Lock()

	rec, ok := s.byID[a.InvestigationID]
	if !ok {
		rec = &Record{
			InvestigationID: a.InvestigationID,
			IncidentID:      a.IncidentID,
			CorrelationID:   a.CorrelationID,
			ServiceID:       a.ServiceID,
			StartedAt:       time.Now().UTC(),
		}
		s.insertLocked(rec)
	}

	now := time.Now().UTC()
	rec.FinishedAt = &now
	// the trace is worth keeping even on failure: it shows how far the run got
	progress := append([]agent.Progress(nil), rec.Result.Progress...)
	rec.Result = res
	rec.Result.Progress = progress
	rec.Status = StatusDone
	if runErr != nil {
		rec.Status = StatusFailed
		rec.Error = runErr.Error()
	}

	snapshot := *rec
	s.mu.Unlock()

	s.mirror(snapshot)
}

// Progress appends one content-free event to a running investigation and
// mirrors it durably. Events for a terminal or unknown record are ignored.
func (s *Store) Progress(a queue.Assignment, event agent.Progress) {
	s.mu.Lock()
	rec, ok := s.byID[a.InvestigationID]
	if !ok || rec.Status != StatusRunning {
		s.mu.Unlock()
		return
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	rec.Result.Progress = append(rec.Result.Progress, event)
	if extra := len(rec.Result.Progress) - agent.MaxProgressEvents; extra > 0 {
		rec.Result.Progress = append([]agent.Progress(nil), rec.Result.Progress[extra:]...)
	}
	snapshot := *rec
	s.mu.Unlock()

	s.mirror(snapshot)
}

// an attempt whose message was left on the queue, kept so a poller can see why
// nothing came back; Seen reports false, since the next delivery must retry
func (s *Store) Retry(a queue.Assignment, reason string) {
	now := time.Now().UTC()
	s.put(&Record{
		InvestigationID: a.InvestigationID,
		IncidentID:      a.IncidentID,
		CorrelationID:   a.CorrelationID,
		ServiceID:       a.ServiceID,
		Status:          StatusRetrying,
		Error:           reason,
		StartedAt:       now,
	})
}

// an investigation that never started, e.g. a message that could
// not be parsed or one given up on after too many deliveries.
func (s *Store) Fail(a queue.Assignment, reason string) {
	now := time.Now().UTC()
	s.put(&Record{
		InvestigationID: a.InvestigationID,
		IncidentID:      a.IncidentID,
		CorrelationID:   a.CorrelationID,
		ServiceID:       a.ServiceID,
		Status:          StatusFailed,
		Error:           reason,
		StartedAt:       now,
		FinishedAt:      &now,
	})
}

// the record for an investigation, falling back to the journal for one this
// process never ran or has since evicted
func (s *Store) Get(investigationID string) (Record, bool) {
	rec, found, _ := s.get(investigationID)
	return rec, found
}

// get, plus whether this process holds the record: one from the journal alone
// may belong to a process that has since exited, which Seen depends on
func (s *Store) get(investigationID string) (rec Record, found, live bool) {
	s.mu.RLock()
	held, ok := s.byID[investigationID]
	if ok {
		rec = *held
		s.mu.RUnlock()
		return rec, true, true
	}
	s.mu.RUnlock()

	rec, found = s.recall(investigationID)
	return rec, found, false
}

// whether this investigation was already paid for, so a redelivery is not run
// twice. Neither a retrying record nor a running one this process does not hold
// counts: that is the crash case, and calling it seen loses the incident.
func (s *Store) Seen(investigationID string) bool {
	rec, found, live := s.get(investigationID)
	switch {
	case !found:
		return false
	case rec.Status == StatusRetrying:
		return false
	case rec.Status == StatusRunning && !live:
		return false
	}
	return true
}

func (s *Store) put(rec *Record) {
	s.mu.Lock()
	s.insertLocked(rec)
	snapshot := *rec
	s.mu.Unlock()

	s.mirror(snapshot)
}

func (s *Store) insertLocked(rec *Record) {
	if _, exists := s.byID[rec.InvestigationID]; !exists {
		s.order = append(s.order, rec.InvestigationID)
	}
	s.byID[rec.InvestigationID] = rec

	for len(s.order) > s.capacity {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.byID, oldest)
	}
}
