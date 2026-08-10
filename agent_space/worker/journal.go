package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"agent_space/agent"
)

// where verdicts outlive the process. Not named in scripts/seed-cluster.sql on
// purpose: that script DROPs what it recreates, and resetting the demo data
// must not throw away investigation history.
const journalTable = "investigations"

// somewhere to persist verdicts, so a restart does not lose them and a
// redelivered message is not investigated a second time.
//
// Every method may fail without consequence to a run: the Store treats this as
// a best-effort mirror of what it already holds in memory. A database that is
// down slows nothing down and loses only durability.
type Journal interface {
	Save(ctx context.Context, rec Record) error
	Load(ctx context.Context, investigationID string) (Record, bool, error)
}

// PGJournal stores records in CockroachDB.
type PGJournal struct {
	Pool *pgxpool.Pool
}

var _ Journal = (*PGJournal)(nil)

// create the table if it is not there yet, the way crdbvector does for its own.
// Returns an error rather than logging: the caller decides whether to run
// without durability, and silently degrading is how you find out months later.
func NewPGJournal(ctx context.Context, pool *pgxpool.Pool) (*PGJournal, error) {
	if pool == nil {
		return nil, errors.New("worker: journal needs a database pool")
	}

	const ddl = `
		CREATE TABLE IF NOT EXISTS ` + journalTable + ` (
			investigation_id STRING PRIMARY KEY,
			incident_id      STRING NOT NULL,
			correlation_id   STRING,
			service_id       STRING,
			status           STRING NOT NULL,
			result           JSONB,
			error            STRING,
			started_at       TIMESTAMPTZ NOT NULL,
			finished_at      TIMESTAMPTZ,
			updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
			INDEX (incident_id),
			INDEX (started_at DESC)
		)`

	if _, err := pool.Exec(ctx, ddl); err != nil {
		return nil, fmt.Errorf("worker: create %s: %w", journalTable, err)
	}
	return &PGJournal{Pool: pool}, nil
}

// upsert by investigation id, so the running record is replaced by the verdict
// rather than accumulating a row per state change
func (j *PGJournal) Save(ctx context.Context, rec Record) error {
	payload, err := json.Marshal(rec.Result)
	if err != nil {
		return fmt.Errorf("worker: encode result: %w", err)
	}

	const q = `
		UPSERT INTO ` + journalTable + ` (
			investigation_id, incident_id, correlation_id, service_id,
			status, result, error, started_at, finished_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6::JSONB, $7, $8, $9, now())`

	_, err = j.Pool.Exec(ctx, q,
		rec.InvestigationID, rec.IncidentID, rec.CorrelationID, rec.ServiceID,
		string(rec.Status), string(payload), rec.Error, rec.StartedAt, rec.FinishedAt,
	)
	if err != nil {
		return fmt.Errorf("worker: save investigation: %w", err)
	}
	return nil
}

func (j *PGJournal) Load(ctx context.Context, investigationID string) (Record, bool, error) {
	const q = `
		SELECT investigation_id, incident_id, coalesce(correlation_id, ''),
		       coalesce(service_id, ''), status, coalesce(result::STRING, ''),
		       coalesce(error, ''), started_at, finished_at
		FROM ` + journalTable + `
		WHERE investigation_id = $1`

	var (
		rec        Record
		status     string
		payload    string
		finishedAt *time.Time
	)

	err := j.Pool.QueryRow(ctx, q, investigationID).Scan(
		&rec.InvestigationID, &rec.IncidentID, &rec.CorrelationID,
		&rec.ServiceID, &status, &payload, &rec.Error,
		&rec.StartedAt, &finishedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("worker: load investigation: %w", err)
	}

	rec.Status = Status(status)
	rec.FinishedAt = finishedAt
	if payload != "" {
		// a trace that will not decode is not worth losing the verdict over
		if err := json.Unmarshal([]byte(payload), &rec.Result); err != nil {
			rec.Result = agent.Result{}
		}
	}
	return rec, true, nil
}
