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

// The decision write is a transaction across two tables, and the parts that
// matter cannot be unit tested: the candidate statuses moving, a pull request
// surviving, and a second decision replacing the first.
func TestLiveDecisionRoundTrip(t *testing.T) {
	solutions, pool, ctx := liveStore(t)

	const (
		inv      = "test-live-decision"
		chosenID = "test-live-cand-chosen"
		plainID  = "test-live-cand-plain"
		withPRID = "test-live-cand-withpr"
	)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, q := range []string{
			`DELETE FROM ` + decisionTable + ` WHERE investigation_id = $1`,
			`DELETE FROM ` + solutionTable + ` WHERE investigation_id = $1`,
			`DELETE FROM ` + remediationTable + ` WHERE investigation_id = $1`,
		} {
			if _, err := pool.Exec(cleanupCtx, q, inv); err != nil {
				t.Logf("cleanup: %v", err)
			}
		}
	})

	passing := Verification{Applied: true, BuildRan: true, Built: true, TestRan: true, Tested: true}
	outcome := Outcome{
		InvestigationID: inv,
		IncidentID:      "test-incident",
		ServiceID:       "checkout",
		Status:          RemediationDone,
		StartedAt:       time.Now().UTC(),
		Candidates: []Candidate{
			{ID: chosenID, InvestigationID: inv, Strategy: "defensive", Diff: "d", Verification: passing, Status: CandidateProposed, CreatedAt: time.Now().UTC()},
			{ID: plainID, InvestigationID: inv, Strategy: "minimal", Diff: "d", Verification: passing, Status: CandidateProposed, CreatedAt: time.Now().UTC()},
			{ID: withPRID, InvestigationID: inv, Strategy: "root-cause", Diff: "d", Verification: passing, Status: CandidateProposed, CreatedAt: time.Now().UTC()},
		},
	}
	if err := solutions.Save(ctx, outcome); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// one candidate already has a draft open, which a decision must not undo
	if err := solutions.MarkOpened(ctx, withPRID, "https://github.com/example/repo/pull/9"); err != nil {
		t.Fatalf("MarkOpened: %v", err)
	}

	stored, _, err := solutions.Load(ctx, inv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	decision, err := NewDecision(stored, DecisionInput{
		ChosenCandidateID: chosenID,
		Reasons:           map[string]string{plainID: "too narrow to hold"},
		IncidentSummary:   "Checkout returned 500s.",
	})
	if err != nil {
		t.Fatalf("NewDecision: %v", err)
	}

	superseded, err := solutions.SaveDecision(ctx, decision)
	if err != nil {
		t.Fatalf("SaveDecision: %v", err)
	}
	if len(superseded) != 0 {
		t.Errorf("a first decision superseded %v", superseded)
	}

	got, found, err := solutions.Decision(ctx, inv)
	if err != nil || !found {
		t.Fatalf("Decision: found=%v err=%v", found, err)
	}
	if got.ChosenCandidateID != chosenID || len(got.Rejections) != 2 {
		t.Errorf("decision did not round-trip: %+v", got)
	}
	if got.Document == "" {
		t.Error("the document did not round-trip, so it could never be re-indexed")
	}
	if got.IndexedAt != nil {
		t.Error("indexed_at is set before anything indexed it")
	}

	// the picker makes one call and has to be able to tell a decided run from an
	// undecided one; without this a reload cannot show what was chosen, and
	// posting again silently supersedes the record
	reloaded, _, err := solutions.Load(ctx, inv)
	if err != nil {
		t.Fatalf("Load after deciding: %v", err)
	}
	if reloaded.Decision == nil {
		t.Fatal("a decided remediation reads back as undecided")
	}
	if reloaded.Decision.ChosenCandidateID != chosenID || len(reloaded.Decision.Rejections) != 2 {
		t.Errorf("decision did not survive the round trip: %+v", reloaded.Decision)
	}

	candidates, err := solutions.ListCandidates(ctx, inv)
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	for _, c := range candidates {
		switch c.ID {
		case chosenID:
			if c.Status != CandidateSelected {
				t.Errorf("chosen candidate status = %q, want %q", c.Status, CandidateSelected)
			}
		case plainID:
			if c.Status != CandidateRejected || c.RejectionReason != "too narrow to hold" {
				t.Errorf("rejected candidate = %q / %q", c.Status, c.RejectionReason)
			}
		case withPRID:
			// the URL is the only link from the incident to the change
			if c.Status != CandidatePROpen || c.PRURL == "" {
				t.Errorf("a candidate with a PR open was moved to %q (url %q)", c.Status, c.PRURL)
			}
		}
	}

	// changing your mind replaces the decision rather than adding a second one
	second, err := NewDecision(stored, DecisionInput{ChosenCandidateID: plainID, Notes: "actually the narrow one"})
	if err != nil {
		t.Fatalf("NewDecision (second): %v", err)
	}
	superseded, err = solutions.SaveDecision(ctx, second)
	if err != nil {
		t.Fatalf("SaveDecision (second): %v", err)
	}
	if len(superseded) != 1 || superseded[0] != decision.ID {
		t.Errorf("superseded = %v, want the first decision %s", superseded, decision.ID)
	}

	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM `+decisionTable+` WHERE investigation_id = $1`, inv).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("%d decisions on file, want 1: two contradictory precedents are worse than either", rows)
	}
}
