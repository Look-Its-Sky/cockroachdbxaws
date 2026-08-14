// Package incident turns an assignment envelope into the prose an agent run
// needs. The queue sends identifiers only, so the description has to be
// fetched from the database the producer wrote it to.
package incident

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"agent_space/utils/queue"
)

// no row for this incident at this context_version. Transient by nature: the
// producer may enqueue the assignment before committing the context, so the
// worker leaves the message for redelivery rather than failing the run.
var ErrNotFound = errors.New("incident: no context row for this incident and version")

// what the producer recorded about an incident, one row of incident_context
type Context struct {
	IncidentID     string
	ContextVersion int
	ServiceID      string
	Environment    string
	Severity       string
	Summary        string
	LogExcerpt     string
	DetectedAt     time.Time
}

// Resolver reads incident context out of CockroachDB.
type Resolver struct {
	Pool *pgxpool.Pool
}

func NewResolver(pool *pgxpool.Pool) *Resolver { return &Resolver{Pool: pool} }

// the row the assignment points at. Keyed by both id and version: the producer
// revises context in place, and an assignment names the version it was raised
// against, so an older message must not pick up newer analysis.
func (r *Resolver) Resolve(ctx context.Context, a queue.Assignment) (Context, error) {
	if r == nil || r.Pool == nil {
		return Context{}, errors.New("incident: no database pool configured")
	}

	const q = `
		SELECT incident_id, context_version, service_id, environment,
		       severity, summary, log_excerpt, detected_at
		FROM incident_context
		WHERE incident_id = $1 AND context_version = $2`

	var c Context
	err := r.Pool.QueryRow(ctx, q, a.IncidentID, a.ContextVersion).Scan(
		&c.IncidentID, &c.ContextVersion, &c.ServiceID, &c.Environment,
		&c.Severity, &c.Summary, &c.LogExcerpt, &c.DetectedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Context{}, fmt.Errorf("%w: incident %s version %d", ErrNotFound, a.IncidentID, a.ContextVersion)
	}
	if err != nil {
		return Context{}, fmt.Errorf("incident: read context: %w", err)
	}

	return c, nil
}

// Latest returns the newest context recorded for an incident, whatever version
// that is.
//
// Resolve is keyed by version because an assignment names the version it was
// raised against. This is for the other direction: a person asking the API to
// remediate an investigation that finished hours ago, where the version the
// message carried is long gone and the current view is the right one.
func (r *Resolver) Latest(ctx context.Context, incidentID string) (Context, error) {
	if r == nil || r.Pool == nil {
		return Context{}, errors.New("incident: no database pool configured")
	}

	const q = `
		SELECT incident_id, context_version, service_id, environment,
		       severity, summary, log_excerpt, detected_at
		FROM incident_context
		WHERE incident_id = $1
		ORDER BY context_version DESC
		LIMIT 1`

	var c Context
	err := r.Pool.QueryRow(ctx, q, incidentID).Scan(
		&c.IncidentID, &c.ContextVersion, &c.ServiceID, &c.Environment,
		&c.Severity, &c.Summary, &c.LogExcerpt, &c.DetectedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Context{}, fmt.Errorf("%w: incident %s", ErrNotFound, incidentID)
	}
	if err != nil {
		return Context{}, fmt.Errorf("incident: read latest context: %w", err)
	}

	return c, nil
}

// render the question for agent.Runner.Run.
//
// This states the facts and stops. What to do with them — find the commit,
// choose ROLLBACK or HOTFIX, cite past incidents — is the Runner's system
// prompt's job, and repeating it here would mean two places to keep in step.
func BuildQuestion(a queue.Assignment, c Context) string {
	var b strings.Builder

	// prefer the stored row: it is the producer's considered view, where the
	// envelope carries whatever was known when the message was raised
	service := first(c.ServiceID, a.ServiceID)
	environment := first(c.Environment, a.Environment)
	severity := first(c.Severity, a.Severity)

	fmt.Fprintf(&b, "Service %s is in incident in %s (severity: %s).\n", service, environment, severity)
	if !c.DetectedAt.IsZero() {
		fmt.Fprintf(&b, "Detected at %s by %s.\n", c.DetectedAt.UTC().Format(time.RFC3339), first(a.Producer, "an upstream detector"))
	}

	if s := strings.TrimSpace(c.Summary); s != "" {
		fmt.Fprintf(&b, "\nSummary:\n%s\n", s)
	}
	if s := strings.TrimSpace(c.LogExcerpt); s != "" {
		fmt.Fprintf(&b, "\nLog excerpt:\n%s\n", s)
	}

	fmt.Fprintf(&b, "\n(incident %s, investigation %s, context version %d)",
		a.IncidentID, a.InvestigationID, c.ContextVersion)

	return b.String()
}

func first(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
