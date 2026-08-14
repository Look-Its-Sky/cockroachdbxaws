package remediation

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"agent_space/utils"
)

// The rest of the suite needs no credentials and no network. This one talks to
// a real cluster, so it is opt-in:
//
//	REMEDIATION_LIVE_TEST=1 go test ./remediation/ -run Live -v
//
// It creates the remediations and solutions tables if they are missing — the
// same call the server makes at boot — writes rows under obviously synthetic
// ids, and deletes them.
func liveStore(t *testing.T) (*Solutions, *pgxpool.Pool, context.Context) {
	t.Helper()

	if os.Getenv("REMEDIATION_LIVE_TEST") == "" {
		t.Skip("set REMEDIATION_LIVE_TEST=1 to run against DATABASE_URL")
	}

	utils.LoadConfig()
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		t.Skip("DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Cleanup rather than defer, and registered before any delete: cleanups run
	// last-in first-out, so those still need a live pool to run on
	t.Cleanup(pool.Close)

	solutions, err := NewSolutions(ctx, pool)
	if err != nil {
		t.Fatalf("NewSolutions: %v", err)
	}
	return solutions, pool, ctx
}

// A remediation left at "running" by a process that exited has no owner: the
// pool is per-process and the SQS message was acknowledged long before the
// handoff, so nothing will ever finish it. Without this it reports "running" to
// the UI forever, and blocks the retry route as well.
func TestLiveAbandonStaleReclaimsRunningRemediations(t *testing.T) {
	solutions, pool, ctx := liveStore(t)

	const id = "test-live-abandon-remediation"
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx,
			`DELETE FROM `+remediationTable+` WHERE investigation_id = $1`, id); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})

	// exactly what Runner.Run writes before it does any work
	if err := solutions.Save(ctx, Outcome{
		InvestigationID: id,
		IncidentID:      "test-incident",
		ServiceID:       "checkout",
		Status:          RemediationRunning,
		StartedAt:       time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := solutions.AbandonStale(ctx); err != nil {
		t.Fatalf("AbandonStale: %v", err)
	}

	got, found, err := solutions.Load(ctx, id)
	if err != nil || !found {
		t.Fatalf("Load: found=%v err=%v", found, err)
	}
	if got.Status != RemediationFailed {
		t.Errorf("status = %q, want %q", got.Status, RemediationFailed)
	}
	if got.Error == "" {
		t.Error("a reclaimed remediation should say why it was abandoned")
	}
	if got.FinishedAt == nil {
		t.Error("a reclaimed remediation should have a finished_at")
	}

	// Unconditional, but still idempotent: a second sweep must leave a row it
	// already failed exactly as it was, or every boot would rewrite finished_at
	// and the record would drift away from when the run actually died.
	//
	// Asserted on this row rather than on the returned count, which is global:
	// a dev server starting a remediation mid-test would make a count assertion
	// fail for a reason that has nothing to do with idempotency.
	if _, err := solutions.AbandonStale(ctx); err != nil {
		t.Fatalf("second AbandonStale: %v", err)
	}

	again, found, err := solutions.Load(ctx, id)
	if err != nil || !found {
		t.Fatalf("Load after second sweep: found=%v err=%v", found, err)
	}
	if again.Status != RemediationFailed || again.Error != got.Error {
		t.Errorf("second sweep changed the row: status=%q error=%q", again.Status, again.Error)
	}
	if again.FinishedAt == nil || !again.FinishedAt.Equal(*got.FinishedAt) {
		t.Errorf("second sweep moved finished_at from %v to %v", got.FinishedAt, again.FinishedAt)
	}
}
