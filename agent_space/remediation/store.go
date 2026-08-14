package remediation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Like investigations and service_repositories, these are long-lived and are
// deliberately absent from scripts/seed-cluster.sql, which drops what it
// recreates. Reseeding the demo data must not throw away proposed fixes.
const (
	remediationTable = "remediations"
	solutionTable    = "solutions"
)

// how long a write or a read may take. Short: this is bookkeeping beside work
// that has already been done and is already in hand.
const storeTimeout = 15 * time.Second

// ErrNoCandidate means nothing is stored under that candidate id.
var ErrNoCandidate = errors.New("remediation: no candidate with that id")

// Solutions persists outcomes and the candidates under them.
//
// One row per remediation and one per candidate, rather than a single blob:
// the UI addresses a candidate directly when an engineer picks it, and a status
// that only exists inside a JSON document cannot be updated in place.
type Solutions struct {
	Pool *pgxpool.Pool
}

func NewSolutions(ctx context.Context, pool *pgxpool.Pool) (*Solutions, error) {
	if pool == nil {
		return nil, errors.New("remediation: solutions need a database pool")
	}

	const remediationDDL = `
		CREATE TABLE IF NOT EXISTS ` + remediationTable + ` (
			investigation_id STRING PRIMARY KEY,
			incident_id      STRING,
			service_id       STRING,
			status           STRING NOT NULL,
			repository       JSONB,
			triage           JSONB,
			error            STRING,
			started_at       TIMESTAMPTZ NOT NULL,
			finished_at      TIMESTAMPTZ,
			updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
			INDEX (started_at DESC)
		)`

	const solutionDDL = `
		CREATE TABLE IF NOT EXISTS ` + solutionTable + ` (
			candidate_id     STRING PRIMARY KEY,
			investigation_id STRING NOT NULL,
			incident_id      STRING,
			service_id       STRING,
			strategy         STRING NOT NULL,
			summary          STRING,
			rationale        STRING,
			files            JSONB,
			diff             STRING,
			-- the whole-file contents a pull request is built from. Stored
			-- rather than recomputed: reopening a PR weeks later must not
			-- depend on a model producing the same answer twice.
			edits            JSONB,
			verification     JSONB,
			status           STRING NOT NULL,
			pr_url           STRING,
			error            STRING,
			-- position in the ranking as it stood when the run finished, so the
			-- order an engineer was shown is reproducible later
			rank             INT NOT NULL DEFAULT 0,
			created_at       TIMESTAMPTZ NOT NULL,
			updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
			INDEX (investigation_id, rank)
		)`

	for _, ddl := range []string{remediationDDL, solutionDDL} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			return nil, fmt.Errorf("remediation: create tables: %w", err)
		}
	}

	// CREATE TABLE IF NOT EXISTS does nothing to a table that already exists,
	// so a column added after the first deploy needs saying explicitly. Kept
	// here rather than in a migration file for the same reason the tables are:
	// this has to survive a demo reset that drops everything else.
	const addRepairs = `ALTER TABLE ` + solutionTable + `
		ADD COLUMN IF NOT EXISTS repairs INT NOT NULL DEFAULT 0`

	if _, err := pool.Exec(ctx, addRepairs); err != nil {
		return nil, fmt.Errorf("remediation: add repairs column: %w", err)
	}
	return &Solutions{Pool: pool}, nil
}

// Save writes the outcome and every candidate under it.
//
// Called repeatedly as a run progresses, so it upserts throughout: a reader
// polling mid-run sees triage land before the candidates do, which is the whole
// point of persisting a running record at all.
func (s *Solutions) Save(ctx context.Context, o Outcome) error {
	repository, err := encode(o.Repository)
	if err != nil {
		return err
	}
	triage, err := encode(o.Triage)
	if err != nil {
		return err
	}

	const q = `
		UPSERT INTO ` + remediationTable + ` (
			investigation_id, incident_id, service_id, status,
			repository, triage, error, started_at, finished_at, updated_at
		) VALUES ($1, $2, $3, $4, $5::JSONB, $6::JSONB, $7, $8, $9, now())`

	if _, err := s.Pool.Exec(ctx, q,
		o.InvestigationID, o.IncidentID, o.ServiceID, string(o.Status),
		repository, triage, o.Error, o.StartedAt, o.FinishedAt,
	); err != nil {
		return fmt.Errorf("remediation: save outcome: %w", err)
	}

	for i, c := range o.Candidates {
		if err := s.saveCandidate(ctx, c, i); err != nil {
			return err
		}
	}
	return nil
}

// one candidate at the rank it was shown at. Deliberately does not touch pr_url
// or a status an engineer has since changed — a re-save of a finished run must
// not un-open a pull request.
func (s *Solutions) saveCandidate(ctx context.Context, c Candidate, rank int) error {
	files, err := encode(c.Files)
	if err != nil {
		return err
	}
	verification, err := encode(c.Verification)
	if err != nil {
		return err
	}
	edits, err := encode(c.Edits)
	if err != nil {
		return err
	}

	const q = `
		INSERT INTO ` + solutionTable + ` (
			candidate_id, investigation_id, incident_id, service_id, strategy,
			summary, rationale, files, diff, edits, verification, status, error,
			rank, repairs, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::JSONB, $9, $10::JSONB, $11::JSONB, $12, $13, $14, $15, $16, now())
		ON CONFLICT (candidate_id) DO UPDATE SET
			summary = excluded.summary,
			rationale = excluded.rationale,
			files = excluded.files,
			diff = excluded.diff,
			edits = excluded.edits,
			verification = excluded.verification,
			error = excluded.error,
			rank = excluded.rank,
			repairs = excluded.repairs,
			updated_at = now()`

	if _, err := s.Pool.Exec(ctx, q,
		c.ID, c.InvestigationID, c.IncidentID, c.ServiceID, c.Strategy,
		c.Summary, c.Rationale, files, c.Diff, edits, verification,
		string(c.Status), c.Error, rank, c.Repairs, c.CreatedAt,
	); err != nil {
		return fmt.Errorf("remediation: save candidate: %w", err)
	}
	return nil
}

// Supersede clears the candidates of a previous run for this investigation, so
// re-running does not leave an engineer picking from two generations at once.
//
// Anything with a pull request open is kept: that URL is the only link back
// from the incident to the change, and a re-run must not orphan it.
func (s *Solutions) Supersede(ctx context.Context, investigationID string) error {
	const q = `
		DELETE FROM ` + solutionTable + `
		WHERE investigation_id = $1 AND coalesce(pr_url, '') = ''`

	if _, err := s.Pool.Exec(ctx, q, investigationID); err != nil {
		return fmt.Errorf("remediation: supersede candidates: %w", err)
	}
	return nil
}

// what a run left at "running" by a dead process is recorded as.
const abandonedReason = "abandoned: the process running this remediation exited before it finished"

// AbandonStale marks every running remediation as failed.
//
// Unconditional, unlike the worker's equivalent, because nothing outside this
// process can be working on one: the pool is per-process (inFlight is an
// in-memory map) and the SQS message was acknowledged long before the handoff,
// so there is no redelivery to collide with. A row still at "running" when a
// process boots has no owner by construction.
//
// That also makes it the only mechanism here. An investigation left behind gets
// re-run by SQS; a remediation left behind is simply gone, and would otherwise
// report "running" to the UI forever.
//
// Candidates need no equivalent: Run only saves them at finish, so a remediation
// that died mid-fan-out has none.
func (s *Solutions) AbandonStale(ctx context.Context) (int64, error) {
	const q = `
		UPDATE ` + remediationTable + `
		SET status = $1, error = coalesce(nullif(error, ''), $2),
		    finished_at = now(), updated_at = now()
		WHERE status = $3`

	tag, err := s.Pool.Exec(ctx, q,
		string(RemediationFailed), abandonedReason, string(RemediationRunning))
	if err != nil {
		return 0, fmt.Errorf("remediation: abandon stale runs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Load returns a remediation and its candidates, best first.
func (s *Solutions) Load(ctx context.Context, investigationID string) (Outcome, bool, error) {
	const q = `
		SELECT investigation_id, coalesce(incident_id, ''), coalesce(service_id, ''),
		       status, coalesce(repository::STRING, ''), coalesce(triage::STRING, ''),
		       coalesce(error, ''), started_at, finished_at
		FROM ` + remediationTable + `
		WHERE investigation_id = $1`

	var (
		o          Outcome
		status     string
		repository string
		triage     string
		finishedAt *time.Time
	)

	err := s.Pool.QueryRow(ctx, q, investigationID).Scan(
		&o.InvestigationID, &o.IncidentID, &o.ServiceID, &status,
		&repository, &triage, &o.Error, &o.StartedAt, &finishedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Outcome{}, false, nil
	}
	if err != nil {
		return Outcome{}, false, fmt.Errorf("remediation: load outcome: %w", err)
	}

	o.Status = RemediationStatus(status)
	o.FinishedAt = finishedAt
	// a field that will not decode is not worth losing the outcome over
	_ = decode(repository, &o.Repository)
	_ = decode(triage, &o.Triage)

	o.Candidates, err = s.ListCandidates(ctx, investigationID)
	if err != nil {
		return o, true, err
	}
	return o, true, nil
}

// ListCandidates returns the candidates for one investigation, best first.
func (s *Solutions) ListCandidates(ctx context.Context, investigationID string) ([]Candidate, error) {
	const q = candidateColumns + `
		FROM ` + solutionTable + `
		WHERE investigation_id = $1
		ORDER BY rank`

	rows, err := s.Pool.Query(ctx, q, investigationID)
	if err != nil {
		return nil, fmt.Errorf("remediation: list candidates: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		c, err := scanCandidate(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Candidate returns one candidate by id, file contents included, for the
// pick-and-open-a-PR path.
func (s *Solutions) Candidate(ctx context.Context, candidateID string) (Candidate, error) {
	const q = candidateColumns + `, coalesce(edits::STRING, '')
		FROM ` + solutionTable + `
		WHERE candidate_id = $1`

	rows, err := s.Pool.Query(ctx, q, candidateID)
	if err != nil {
		return Candidate{}, fmt.Errorf("remediation: load candidate: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Candidate{}, fmt.Errorf("remediation: load candidate: %w", err)
		}
		return Candidate{}, fmt.Errorf("%w: %s", ErrNoCandidate, candidateID)
	}
	return scanCandidate(rows, true)
}

// MarkOpened records that an engineer picked this candidate and a draft opened.
func (s *Solutions) MarkOpened(ctx context.Context, candidateID, prURL string) error {
	const q = `
		UPDATE ` + solutionTable + `
		SET status = $2, pr_url = $3, updated_at = now()
		WHERE candidate_id = $1`

	tag, err := s.Pool.Exec(ctx, q, candidateID, string(CandidatePROpen), prURL)
	if err != nil {
		return fmt.Errorf("remediation: mark candidate opened: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrNoCandidate, candidateID)
	}
	return nil
}

// Recent lists remediations newest first, for the frontend's landing page.
func (s *Solutions) Recent(ctx context.Context, limit int) ([]Outcome, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	const q = `
		SELECT investigation_id, coalesce(incident_id, ''), coalesce(service_id, ''),
		       status, coalesce(triage::STRING, ''), coalesce(error, ''),
		       started_at, finished_at
		FROM ` + remediationTable + `
		ORDER BY started_at DESC
		LIMIT $1`

	rows, err := s.Pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("remediation: list remediations: %w", err)
	}
	defer rows.Close()

	var out []Outcome
	for rows.Next() {
		var (
			o          Outcome
			status     string
			triage     string
			finishedAt *time.Time
		)
		if err := rows.Scan(
			&o.InvestigationID, &o.IncidentID, &o.ServiceID, &status,
			&triage, &o.Error, &o.StartedAt, &finishedAt,
		); err != nil {
			return nil, fmt.Errorf("remediation: scan remediation: %w", err)
		}

		o.Status = RemediationStatus(status)
		o.FinishedAt = finishedAt
		_ = decode(triage, &o.Triage)
		// candidates are deliberately not loaded here: a diff per candidate per
		// row turns a list page into megabytes
		out = append(out, o)
	}
	return out, rows.Err()
}

// shared by every candidate read, so the scan order cannot drift from the
// query. The file contents are appended by the one caller that needs them: a
// list endpoint would otherwise carry every changed file, in full, per row.
const candidateColumns = `
	SELECT candidate_id, investigation_id, coalesce(incident_id, ''),
	       coalesce(service_id, ''), strategy, coalesce(summary, ''),
	       coalesce(rationale, ''), coalesce(files::STRING, ''), coalesce(diff, ''),
	       coalesce(verification::STRING, ''), status, coalesce(pr_url, ''),
	       coalesce(error, ''), repairs, created_at`

func scanCandidate(rows pgx.Rows, withEdits bool) (Candidate, error) {
	var (
		c            Candidate
		files        string
		verification string
		status       string
		edits        string
	)

	targets := []any{
		&c.ID, &c.InvestigationID, &c.IncidentID, &c.ServiceID, &c.Strategy,
		&c.Summary, &c.Rationale, &files, &c.Diff, &verification,
		&status, &c.PRURL, &c.Error, &c.Repairs, &c.CreatedAt,
	}
	if withEdits {
		targets = append(targets, &edits)
	}

	if err := rows.Scan(targets...); err != nil {
		return Candidate{}, fmt.Errorf("remediation: scan candidate: %w", err)
	}

	c.Status = CandidateStatus(status)
	_ = decode(files, &c.Files)
	_ = decode(verification, &c.Verification)
	_ = decode(edits, &c.Edits)
	return c, nil
}

// JSON for a JSONB column. A nil pointer becomes SQL NULL rather than the
// four bytes "null", so a column that was never written reads back as absent.
func encode(v any) (any, error) {
	if v == nil {
		return nil, nil
	}

	payload, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("remediation: encode: %w", err)
	}
	if string(payload) == "null" {
		return nil, nil
	}
	return string(payload), nil
}

func decode(payload string, into any) error {
	if payload == "" || payload == "null" {
		return nil
	}
	return json.Unmarshal([]byte(payload), into)
}
