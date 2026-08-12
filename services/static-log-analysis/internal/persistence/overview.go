package persistence

import (
	"context"
	"time"
)

// Overview is the deliberately narrow, content-free projection exposed to the
// operator dashboard. It contains no log body, stack trace, context snapshot,
// report, fingerprint, customer identifier, or queue payload.
type Overview struct {
	RecordsSeen      int64                   `json:"records_seen"`
	IncidentFamilies int64                   `json:"incident_families"`
	Investigations   map[string]int64        `json:"investigations"`
	Outbox           map[string]int64        `json:"outbox"`
	Recent           []OverviewInvestigation `json:"recent_investigations"`
}

type OverviewInvestigation struct {
	InvestigationID string    `json:"investigation_id"`
	Service         string    `json:"service"`
	Environment     string    `json:"environment"`
	Severity        string    `json:"severity"`
	State           string    `json:"state"`
	TriggerReason   string    `json:"trigger_reason"`
	QueuedAt        time.Time `json:"queued_at"`
}

// Overview returns bounded, safe operational metadata for this Store's scope.
func (s *Store) Overview(ctx context.Context, limit int) (Overview, error) {
	if s == nil || s.pool == nil || limit < 1 || limit > 25 {
		return Overview{}, ErrInvalidInput
	}
	result := Overview{Investigations: map[string]int64{}, Outbox: map[string]int64{}}
	if err := s.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM records_seen WHERE region=$1 AND tenant_id=$2),
		(SELECT count(*) FROM incident_families WHERE region=$1 AND tenant_id=$2)`,
		s.scope.Region, s.scope.TenantID).Scan(&result.RecordsSeen, &result.IncidentFamilies); err != nil {
		return Overview{}, databaseError(ctx, err)
	}

	rows, err := s.pool.Query(ctx, `SELECT state,count(*) FROM investigations
		WHERE region=$1 AND tenant_id=$2 GROUP BY state`, s.scope.Region, s.scope.TenantID)
	if err != nil {
		return Overview{}, databaseError(ctx, err)
	}
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			rows.Close()
			return Overview{}, databaseError(ctx, err)
		}
		result.Investigations[state] = count
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Overview{}, databaseError(ctx, err)
	}
	rows.Close()

	rows, err = s.pool.Query(ctx, `SELECT state,count(*) FROM outbox_messages
		WHERE region=$1 AND tenant_id=$2 GROUP BY state`, s.scope.Region, s.scope.TenantID)
	if err != nil {
		return Overview{}, databaseError(ctx, err)
	}
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			rows.Close()
			return Overview{}, databaseError(ctx, err)
		}
		result.Outbox[state] = count
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Overview{}, databaseError(ctx, err)
	}
	rows.Close()

	rows, err = s.pool.Query(ctx, `SELECT i.investigation_id,f.service_id,f.environment,
		f.severity,i.state,i.trigger_reason,i.queued_at
		FROM investigations i JOIN incident_families f
		ON (f.region,f.tenant_id,f.incident_id)=(i.region,i.tenant_id,i.incident_id)
		WHERE i.region=$1 AND i.tenant_id=$2
		ORDER BY i.queued_at DESC,i.investigation_id DESC LIMIT $3`,
		s.scope.Region, s.scope.TenantID, limit)
	if err != nil {
		return Overview{}, databaseError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var item OverviewInvestigation
		if err := rows.Scan(&item.InvestigationID, &item.Service, &item.Environment,
			&item.Severity, &item.State, &item.TriggerReason, &item.QueuedAt); err != nil {
			return Overview{}, databaseError(ctx, err)
		}
		item.QueuedAt = item.QueuedAt.UTC()
		result.Recent = append(result.Recent, item)
	}
	if err := rows.Err(); err != nil {
		return Overview{}, databaseError(ctx, err)
	}
	return result, nil
}
