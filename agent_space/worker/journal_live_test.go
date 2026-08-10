package worker

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"agent_space/agent"
	"agent_space/utils"
)

// The rest of the suite needs no credentials and no network. This one talks to
// a real cluster, so it is opt-in:
//
//	JOURNAL_LIVE_TEST=1 go test ./worker/ -run Live -v
//
// It creates the investigations table if it is missing — the same call the
// server makes at boot — writes one row under an obviously synthetic id, reads
// it back, and deletes it.
func TestLiveJournalRoundTrip(t *testing.T) {
	if os.Getenv("JOURNAL_LIVE_TEST") == "" {
		t.Skip("set JOURNAL_LIVE_TEST=1 to run against DATABASE_URL")
	}

	utils.LoadConfig()
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		t.Skip("DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Cleanup rather than defer, and registered first: cleanups run last-in
	// first-out, so the delete below still has a live pool to run on
	t.Cleanup(pool.Close)

	journal, err := NewPGJournal(ctx, pool)
	if err != nil {
		t.Fatalf("NewPGJournal: %v", err)
	}

	const id = "test-live-journal-round-trip"
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM investigations WHERE investigation_id = $1`, id); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})

	started := time.Now().UTC().Truncate(time.Microsecond)
	finished := started.Add(89 * time.Second)
	want := Record{
		InvestigationID: id,
		IncidentID:      "test-incident",
		CorrelationID:   "test-correlation",
		ServiceID:       "payment",
		Status:          StatusDone,
		Result: agent.Result{
			Answer:     "ROLLBACK 7e91d04",
			Sources:    4,
			Iterations: 3,
			Trace: []agent.Step{{
				Iteration: 1, Tool: "select_query", Output: "rows", DurationMS: 42,
			}},
		},
		StartedAt:  started,
		FinishedAt: &finished,
	}

	if err := journal.Save(ctx, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, ok, err := journal.Load(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}

	if got.Status != want.Status || got.IncidentID != want.IncidentID || got.ServiceID != want.ServiceID {
		t.Errorf("scalars did not round-trip: %+v", got)
	}
	if got.Result.Answer != want.Result.Answer || got.Result.Iterations != 3 || len(got.Result.Trace) != 1 {
		t.Errorf("result JSON did not round-trip: %+v", got.Result)
	}
	if got.Result.Trace[0].Tool != "select_query" {
		t.Errorf("trace did not round-trip: %+v", got.Result.Trace[0])
	}
	if got.FinishedAt == nil || !got.FinishedAt.Equal(finished) {
		t.Errorf("finished_at = %v, want %v", got.FinishedAt, finished)
	}

	// Save is an upsert: a second write replaces rather than duplicating
	want.Status = StatusFailed
	if err := journal.Save(ctx, want); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM investigations WHERE investigation_id = $1`, id).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("investigation has %d rows after two saves, want 1", rows)
	}

	// a miss must be a clean miss, not an error
	if _, ok, err := journal.Load(ctx, "no-such-investigation"); err != nil || ok {
		t.Errorf("Load of an unknown id: ok=%v err=%v", ok, err)
	}

	// and a Store in front of it behaves the same way
	s := NewDurableStore(journal)
	if !s.Seen(id) {
		t.Error("Seen() is false for a persisted completed investigation")
	}
	if s.Seen("no-such-investigation") {
		t.Error("Seen() is true for an unknown investigation")
	}
}
