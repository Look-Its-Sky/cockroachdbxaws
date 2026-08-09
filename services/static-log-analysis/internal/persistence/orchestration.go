package persistence

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/agent"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/queue"
	crdbpgxv5 "github.com/cockroachdb/cockroach-go/v2/crdb/crdbpgxv5"
	"github.com/jackc/pgx/v5"
)

type outboxCandidate struct {
	message        queue.Message
	aggregateType  string
	aggregateID    string
	payloadVersion string
	attributes     []byte
	createdAt      time.Time
	incidentID     *string
	serviceID      *string
	environment    *string
	contentDigest  []byte
}

func (s *Store) AcquireInvestigation(ctx context.Context, scope Scope, investigationID, owner, token string, now time.Time, ttl time.Duration) (Lease, error) {
	expiresAt, err := safeDeadline(now, ttl)
	if s == nil || s.pool == nil || !validScope(scope) || !s.permits(scope) || !validUUIDv7(investigationID) ||
		!validText(owner, MaxOwnerBytes) || !validUUIDv7(token) || err != nil {
		return Lease{}, ErrInvalidInput
	}
	lease := Lease{Scope: scope, InvestigationID: investigationID, Owner: owner, Token: token, ExpiresAt: expiresAt}
	err = crdbpgxv5.ExecuteTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		lease.IncidentID = ""
		lease.RenewalSequence = 0
		err := tx.QueryRow(ctx, `UPDATE investigation_claims
			SET owner=$4, lease_token=$5, lease_expires_at=$6, renewal_sequence=renewal_sequence+1
			WHERE region=$1 AND tenant_id=$2 AND investigation_id=$3
			  AND (owner IS NULL OR lease_expires_at <= $7)
			RETURNING incident_id, renewal_sequence`, scope.Region, scope.TenantID, investigationID,
			owner, token, expiresAt, now).Scan(&lease.IncidentID, &lease.RenewalSequence)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStaleClaim
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE investigations SET state='claimed', started_at=COALESCE(started_at,$4)
			WHERE region=$1 AND tenant_id=$2 AND investigation_id=$3`, scope.Region, scope.TenantID,
			investigationID, now)
		return err
	})
	if err != nil {
		return Lease{}, databaseError(ctx, err)
	}
	return lease, nil
}

func (s *Store) RenewInvestigation(ctx context.Context, lease Lease, now time.Time, ttl time.Duration) (Lease, error) {
	expiresAt, err := safeDeadline(now, ttl)
	if s == nil || s.pool == nil || !validScope(lease.Scope) || !s.permits(lease.Scope) || !validUUIDv7(lease.InvestigationID) ||
		!validText(lease.Owner, MaxOwnerBytes) || !validUUIDv7(lease.Token) || err != nil {
		return Lease{}, ErrInvalidInput
	}
	updated := lease
	updated.ExpiresAt = expiresAt
	err = s.pool.QueryRow(ctx, `UPDATE investigation_claims
		SET lease_expires_at=$6, renewal_sequence=renewal_sequence+1
		WHERE region=$1 AND tenant_id=$2 AND investigation_id=$3 AND owner=$4 AND lease_token=$5
		  AND lease_expires_at > $7
		RETURNING incident_id, renewal_sequence`, lease.Scope.Region, lease.Scope.TenantID,
		lease.InvestigationID, lease.Owner, lease.Token, expiresAt, now).Scan(&updated.IncidentID, &updated.RenewalSequence)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrStaleClaim
	}
	if err != nil {
		return Lease{}, databaseError(ctx, err)
	}
	return updated, nil
}

func (s *Store) CompleteInvestigation(ctx context.Context, lease Lease, completedAt time.Time) error {
	if s == nil || s.pool == nil || !validScope(lease.Scope) || !s.permits(lease.Scope) || !validUUIDv7(lease.InvestigationID) ||
		!validUUIDv7(lease.Token) || !validUTC(completedAt) {
		return ErrInvalidInput
	}
	err := crdbpgxv5.ExecuteTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var incidentID string
		err := tx.QueryRow(ctx, `DELETE FROM investigation_claims
			WHERE region=$1 AND tenant_id=$2 AND investigation_id=$3 AND lease_token=$4
			  AND lease_expires_at > $5
			RETURNING incident_id`, lease.Scope.Region, lease.Scope.TenantID, lease.InvestigationID,
			lease.Token, completedAt).Scan(&incidentID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStaleClaim
		}
		if err != nil {
			return err
		}
		command, err := tx.Exec(ctx, `UPDATE investigations SET state='completed', completed_at=$4
			WHERE region=$1 AND tenant_id=$2 AND investigation_id=$3 AND state <> 'completed'`,
			lease.Scope.Region, lease.Scope.TenantID, lease.InvestigationID, completedAt)
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return ErrStaleClaim
		}
		_, err = tx.Exec(ctx, `UPDATE incident_families SET active_investigation_id=NULL
			WHERE region=$1 AND tenant_id=$2 AND incident_id=$3 AND active_investigation_id=$4`,
			lease.Scope.Region, lease.Scope.TenantID, incidentID, lease.InvestigationID)
		return err
	})
	return databaseError(ctx, err)
}

func (s *Store) AppendInvestigationContext(ctx context.Context, value InvestigationContext) error {
	if s == nil || s.pool == nil || !validScope(value.Scope) || !s.permits(value.Scope) || !validUUIDv7(value.InvestigationID) ||
		value.Version < 1 || value.Snapshot.Validate() != nil || !validClassification(value.Classification) ||
		value.PolicyVersion != s.validator.Version() || !validUTC(value.CreatedAt) || !s.validSafeValue(value.Snapshot) {
		return ErrInvalidInput
	}
	encoded, err := encodeSafeValue(value.Snapshot)
	if err != nil {
		return err
	}
	if len(encoded) > MaxContextBytes {
		return ErrInvalidInput
	}
	err = crdbpgxv5.ExecuteTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := s.appendInvestigationContextTx(ctx, tx, value, encoded); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE incident_families SET context_version=greatest(context_version,$4)
			WHERE region=$1 AND tenant_id=$2 AND active_investigation_id=$3`, value.Scope.Region,
			value.Scope.TenantID, value.InvestigationID, value.Version)
		return err
	})
	return databaseError(ctx, err)
}

// appendInvestigationContextTx is the single immutable-context write path.
// Replays at any already-observed version are accepted only when every sealed
// field is byte-for-byte/SQL-value equivalent to the existing row.
func (s *Store) appendInvestigationContextTx(ctx context.Context, tx pgx.Tx, value InvestigationContext, encoded []byte) (bool, error) {
	var current int64
	if err := tx.QueryRow(ctx, `SELECT context_version FROM investigations
		WHERE region=$1 AND tenant_id=$2 AND investigation_id=$3 FOR UPDATE`, value.Scope.Region,
		value.Scope.TenantID, value.InvestigationID).Scan(&current); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrInvalidInput
		}
		return false, err
	}
	if value.Version <= current {
		var equivalent bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM investigation_contexts
			WHERE region=$1 AND tenant_id=$2 AND investigation_id=$3 AND version=$4
			  AND snapshot=$5::JSONB AND classification=$6 AND policy_version=$7 AND created_at=$8)`, value.Scope.Region,
			value.Scope.TenantID, value.InvestigationID, value.Version, encoded,
			value.Classification, value.PolicyVersion, value.CreatedAt).Scan(&equivalent); err != nil {
			return false, err
		}
		if equivalent {
			return false, nil
		}
		return false, ErrImmutable
	}
	if _, err := tx.Exec(ctx, `INSERT INTO investigation_contexts
		(region,tenant_id,investigation_id,version,snapshot,classification,policy_version,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, value.Scope.Region, value.Scope.TenantID,
		value.InvestigationID, value.Version, encoded, value.Classification, value.PolicyVersion, value.CreatedAt); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE investigations SET context_version=$4
		WHERE region=$1 AND tenant_id=$2 AND investigation_id=$3`, value.Scope.Region,
		value.Scope.TenantID, value.InvestigationID, value.Version); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) validSafeValue(value model.SafeValue) bool {
	return s.validator.ValidateValue(value) == nil
}

func (s *Store) InvestigationContext(ctx context.Context, scope Scope, investigationID string, version int64) (InvestigationContext, error) {
	if s == nil || s.pool == nil || !validScope(scope) || !s.permits(scope) || !validUUIDv7(investigationID) || version < 1 {
		return InvestigationContext{}, ErrInvalidInput
	}
	result := InvestigationContext{Scope: scope, InvestigationID: investigationID, Version: version}
	var snapshot []byte
	err := s.pool.QueryRow(ctx, `SELECT snapshot,classification,policy_version,created_at FROM investigation_contexts
		WHERE region=$1 AND tenant_id=$2 AND investigation_id=$3 AND version=$4
		  AND octet_length(snapshot::STRING) <= $5`, scope.Region, scope.TenantID, investigationID,
		version, MaxContextBytes).Scan(&snapshot, &result.Classification, &result.PolicyVersion, &result.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return InvestigationContext{}, ErrUnavailable
	}
	if err != nil || json.Unmarshal(snapshot, &result.Snapshot) != nil || result.Snapshot.Validate() != nil ||
		!validClassification(result.Classification) || result.PolicyVersion != s.validator.Version() ||
		!validUTC(result.CreatedAt.UTC()) || !s.validSafeValue(result.Snapshot) {
		return InvestigationContext{}, ErrUnavailable
	}
	result.CreatedAt = result.CreatedAt.UTC()
	return result, nil
}

func (s *Store) ClaimOutbox(ctx context.Context, scope Scope, owner string, tokens []string, now time.Time, ttl time.Duration) ([]OutboxClaim, error) {
	expiresAt, deadlineErr := safeDeadline(now, ttl)
	if s == nil || s.pool == nil || !validScope(scope) || !s.permits(scope) || !validText(owner, MaxOwnerBytes) ||
		len(tokens) == 0 || len(tokens) > MaxClaimBatch || deadlineErr != nil {
		return nil, ErrInvalidInput
	}
	seen := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		if !validUUIDv7(token) || seen[token] {
			return nil, ErrInvalidInput
		}
		seen[token] = true
	}
	var claims []OutboxClaim
	err := crdbpgxv5.ExecuteTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		claims = claims[:0]
		// SKIP LOCKED treats an unresolved write intent as a lock, so a message
		// whose creating transaction has already committed would be skipped
		// until some later reader resolved it. This bounded non-locking read
		// resolves those intents first, leaving SKIP LOCKED to skip only rows a
		// live competitor actually holds.
		if _, err := tx.Exec(ctx, `SELECT message_id FROM outbox_messages
			WHERE region=$1 AND tenant_id=$2 AND
			 ((state='pending' AND next_attempt_at <= $3) OR (state='claimed' AND claim_expires_at <= $3))
			ORDER BY created_at,message_id LIMIT $4`, scope.Region, scope.TenantID, now, len(tokens)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT o.message_id,o.deduplication_key,o.message_type,o.aggregate_type,o.aggregate_id,
			o.payload_version,o.payload,o.attributes,o.content_digest,o.created_at,i.incident_id,f.service_id,f.environment
			FROM outbox_messages o
			LEFT JOIN investigations i ON (i.region,i.tenant_id,i.investigation_id)=(o.region,o.tenant_id,o.aggregate_id)
			LEFT JOIN incident_families f ON (f.region,f.tenant_id,f.incident_id)=(i.region,i.tenant_id,i.incident_id)
			WHERE o.region=$1 AND o.tenant_id=$2 AND
			 ((o.state='pending' AND o.next_attempt_at <= $3) OR (o.state='claimed' AND o.claim_expires_at <= $3))
			ORDER BY o.created_at,o.message_id LIMIT $4 FOR UPDATE OF o SKIP LOCKED`, scope.Region, scope.TenantID,
			now, len(tokens))
		if err != nil {
			return err
		}
		var candidates []outboxCandidate
		for rows.Next() {
			var item outboxCandidate
			if err := rows.Scan(&item.message.MessageID, &item.message.DeduplicationKey, &item.message.Type,
				&item.aggregateType, &item.aggregateID, &item.payloadVersion, &item.message.Body, &item.attributes,
				&item.contentDigest, &item.createdAt, &item.incidentID, &item.serviceID, &item.environment); err != nil {
				rows.Close()
				return err
			}
			if json.Unmarshal(item.attributes, &item.message.Attributes) != nil {
				rows.Close()
				return ErrUnavailable
			}
			recomputed := outboxContentDigest(item.message, item.aggregateType, item.aggregateID, item.payloadVersion)
			if len(item.contentDigest) != len(recomputed) || subtle.ConstantTimeCompare(item.contentDigest, recomputed[:]) != 1 {
				rows.Close()
				return ErrUnavailable
			}
			candidates = append(candidates, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, candidate := range candidates {
			valid, validationErr := s.validAssignmentOutbox(ctx, tx, scope, candidate)
			if validationErr != nil {
				return validationErr
			}
			if !valid {
				return ErrUnavailable
			}
		}
		for i, candidate := range candidates {
			claim := OutboxClaim{Scope: scope, Message: candidate.message, Owner: owner, Token: tokens[i], ExpiresAt: expiresAt}
			if err := tx.QueryRow(ctx, `UPDATE outbox_messages SET state='claimed', attempts=attempts+1,
				claim_owner=$4,claim_token=$5,claim_expires_at=$6
				WHERE region=$1 AND tenant_id=$2 AND message_id=$3 RETURNING attempts`, scope.Region,
				scope.TenantID, claim.Message.MessageID, owner, claim.Token, expiresAt).Scan(&claim.Attempt); err != nil {
				return err
			}
			claims = append(claims, claim)
		}
		return nil
	})
	if err != nil {
		return nil, databaseError(ctx, err)
	}
	return claims, nil
}

func (s *Store) validAssignmentOutbox(ctx context.Context, tx pgx.Tx, scope Scope, item outboxCandidate) (bool, error) {
	message := item.message
	payload, err := agent.DecodeAssignment(message.Body)
	if err != nil || !s.validOutboxMessage(message) || message.Type != agent.AssignmentMessageType ||
		item.aggregateType != "investigation" || !validUUIDv7(item.aggregateID) || item.payloadVersion != agent.AssignmentSchemaVersion ||
		message.DeduplicationKey != "assignment:"+item.aggregateID || item.incidentID == nil || item.serviceID == nil || item.environment == nil ||
		payload.SchemaVersion != item.payloadVersion || payload.MessageID != message.MessageID || payload.MessageType != message.Type || payload.InvestigationID != item.aggregateID ||
		payload.CorrelationID != item.aggregateID ||
		payload.Region != scope.Region || payload.TenantID != scope.TenantID || payload.Classification != s.classification ||
		!payload.CreatedAt.Equal(item.createdAt) || payload.IncidentID != *item.incidentID || payload.ServiceID != *item.serviceID ||
		payload.Environment != *item.environment || !validAssignmentAttributes(message.Attributes, scope.Region) {
		return false, nil
	}
	var routesExist bool
	if err := tx.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM incident_generations WHERE region=$1 AND tenant_id=$2 AND incident_id=$3 AND generation=$4) AND
		EXISTS(SELECT 1 FROM investigation_contexts WHERE region=$1 AND tenant_id=$2 AND investigation_id=$5 AND version=$6)`,
		scope.Region, scope.TenantID, payload.IncidentID, payload.IncidentGeneration,
		payload.InvestigationID, payload.ContextVersion).Scan(&routesExist); err != nil {
		return false, err
	}
	return routesExist, nil
}

func (s *Store) MarkOutboxPublished(ctx context.Context, claim OutboxClaim, publishedAt time.Time) error {
	if s == nil || s.pool == nil || !validScope(claim.Scope) || !s.permits(claim.Scope) || !validUUIDv7(claim.Message.MessageID) ||
		!validUUIDv7(claim.Token) || !validUTC(publishedAt) {
		return ErrInvalidInput
	}
	command, err := s.pool.Exec(ctx, `UPDATE outbox_messages SET state='published',published_at=$5,
		claim_owner=NULL,claim_token=NULL,claim_expires_at=NULL,last_safe_error=NULL
		WHERE region=$1 AND tenant_id=$2 AND message_id=$3 AND claim_token=$4
		  AND state='claimed' AND claim_expires_at > $5`, claim.Scope.Region, claim.Scope.TenantID,
		claim.Message.MessageID, claim.Token, publishedAt)
	if err != nil {
		return databaseError(ctx, err)
	}
	if command.RowsAffected() != 1 {
		return ErrStaleClaim
	}
	return nil
}

func (s *Store) RetryOutbox(ctx context.Context, claim OutboxClaim, failure OutboxFailure, now, nextAttemptAt time.Time) error {
	if s == nil || s.pool == nil || !validScope(claim.Scope) || !s.permits(claim.Scope) || !validUUIDv7(claim.Message.MessageID) ||
		!validUUIDv7(claim.Token) || !validOutboxFailure(failure) || !validUTC(now) ||
		!validUTC(nextAttemptAt) || nextAttemptAt.Before(now) {
		return ErrInvalidInput
	}
	command, err := s.pool.Exec(ctx, `UPDATE outbox_messages SET state='pending',next_attempt_at=$5,
		claim_owner=NULL,claim_token=NULL,claim_expires_at=NULL,last_safe_error=$6
		WHERE region=$1 AND tenant_id=$2 AND message_id=$3 AND claim_token=$4
		  AND state='claimed' AND claim_expires_at > $7`, claim.Scope.Region, claim.Scope.TenantID,
		claim.Message.MessageID, claim.Token, nextAttemptAt, string(failure), now)
	if err != nil {
		return databaseError(ctx, err)
	}
	if command.RowsAffected() != 1 {
		return ErrStaleClaim
	}
	return nil
}

func validOutboxFailure(failure OutboxFailure) bool {
	switch failure {
	case OutboxFailureQueueUnavailable, OutboxFailureTimeout, OutboxFailureLostAck, OutboxFailureRejected:
		return true
	default:
		return false
	}
}
