package persistence

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

func (s *Store) Incident(ctx context.Context, scope Scope, incidentID string) (IncidentSnapshot, error) {
	if s == nil || s.pool == nil || !validScope(scope) || !s.permits(scope) || !validFingerprint(incidentID) {
		return IncidentSnapshot{}, ErrInvalidInput
	}
	result := IncidentSnapshot{Scope: scope, IncidentID: incidentID}
	var latest *int64
	var active *string
	err := s.pool.QueryRow(ctx, `SELECT latest_generation,active_investigation_id,context_version,
		COALESCE((SELECT sum(occurrence_count) FROM incident_generations g
		 WHERE g.region=f.region AND g.tenant_id=f.tenant_id AND g.incident_id=f.incident_id),0)
		FROM incident_families f WHERE region=$1 AND tenant_id=$2 AND incident_id=$3`,
		scope.Region, scope.TenantID, incidentID).Scan(&latest, &active, &result.ContextVersion, &result.OccurrenceCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return IncidentSnapshot{}, ErrUnavailable
	}
	if err != nil {
		return IncidentSnapshot{}, databaseError(ctx, err)
	}
	if latest != nil {
		result.LatestGeneration = *latest
	}
	if active != nil {
		result.ActiveInvestigationID = *active
	}
	return result, nil
}

func (s *Store) Occurrence(ctx context.Context, scope Scope, recordID string) (OccurrenceSnapshot, error) {
	if s == nil || s.pool == nil || !validScope(scope) || !s.permits(scope) || !validFingerprint(recordID) {
		return OccurrenceSnapshot{}, ErrInvalidInput
	}
	result := OccurrenceSnapshot{Scope: scope, RecordID: recordID}
	var encoded []byte
	err := s.pool.QueryRow(ctx, `SELECT incident_id,generation,late,evidence_only,safe_summary FROM occurrences
		WHERE region=$1 AND tenant_id=$2 AND record_id=$3 AND octet_length(safe_summary::STRING) <= $4`,
		scope.Region, scope.TenantID, recordID, MaxProjectionBytes).Scan(
		&result.IncidentID, &result.Generation, &result.Late, &result.EvidenceOnly, &encoded)
	if errors.Is(err, pgx.ErrNoRows) {
		return OccurrenceSnapshot{}, ErrUnavailable
	}
	if err != nil || len(encoded) > MaxProjectionBytes || json.Unmarshal(encoded, &result.Projection) != nil ||
		!s.validOccurrenceProjection(result.Projection) {
		return OccurrenceSnapshot{}, ErrUnavailable
	}
	result.Projection.EventTime = result.Projection.EventTime.UTC()
	result.Projection.ObservedTime = result.Projection.ObservedTime.UTC()
	return result, nil
}

func (s *Store) OccurrenceExists(ctx context.Context, scope Scope, recordID string) (bool, error) {
	if s == nil || s.pool == nil || !validScope(scope) || !s.permits(scope) || !validFingerprint(recordID) {
		return false, ErrInvalidInput
	}
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM occurrences
		WHERE region=$1 AND tenant_id=$2 AND record_id=$3)`, scope.Region, scope.TenantID, recordID).Scan(&exists)
	if err != nil {
		return false, databaseError(ctx, err)
	}
	return exists, nil
}
