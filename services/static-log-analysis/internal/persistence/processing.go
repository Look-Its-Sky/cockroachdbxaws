package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/agent"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/fingerprint"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/incident"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/journal"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/queue"
	crdbpgxv5 "github.com/cockroachdb/cockroach-go/v2/crdb/crdbpgxv5"
	"github.com/jackc/pgx/v5"
)

func (s *Store) Process(ctx context.Context, input ProcessInput) (ProcessResult, error) {
	if s == nil {
		return ProcessResult{}, ErrInvalidInput
	}
	results, err := s.ProcessBatch(ctx, []ProcessInput{input})
	if err != nil {
		return ProcessResult{}, err
	}
	return results[0], nil
}

func (s *Store) ProcessBatch(ctx context.Context, inputs []ProcessInput) ([]ProcessResult, error) {
	if s == nil || s.pool == nil || len(inputs) == 0 || len(inputs) > MaxProcessBatch {
		return nil, ErrInvalidInput
	}
	// Every reason this Store cannot serve the batch at all is decided first and
	// reported as a distinct sentinel. A caller isolating record-local poison
	// destroys durable payloads, so it must never see a deployment-wide scope,
	// policy, or classification disagreement as a malformed record.
	replays := make([]journal.ReplayIdentity, len(inputs))
	for i := range inputs {
		if !validScope(inputs[i].Scope) || !s.permits(inputs[i].Scope) ||
			inputs[i].Record.Redaction.PolicyVersion != s.validator.Version() {
			return nil, ErrConfiguration
		}
		// The sealed replay digest also binds classification, and journal.md
		// requires a classification or priority conflict to remain detectable
		// here after compaction. It is therefore kept record-local: this
		// comparison cannot tell a forged record from a misconfigured boundary,
		// and a forged record must still fail closed. Boundary agreement is
		// instead proven once, at coordinator startup, against Boundary().
		replay, err := journal.NewReplayIdentity(inputs[i].Record, inputs[i].Scope.TenantID,
			s.classification, inputs[i].Replay.Priority())
		if err != nil || replay.Version() != inputs[i].Replay.Version() ||
			replay.Digest() != inputs[i].Replay.Digest() || replay.Priority() != inputs[i].Replay.Priority() {
			return nil, ErrInvalidInput
		}
		replays[i] = replay
	}
	for i := range inputs {
		if !s.validProcessInput(inputs[i]) {
			return nil, ErrInvalidInput
		}
	}
	results := make([]ProcessResult, len(inputs))
	attempt := 0
	err := crdbpgxv5.ExecuteTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		attempt++
		for i := range results {
			results[i] = ProcessResult{}
		}
		if s.hooks.onAttempt != nil {
			s.hooks.onAttempt(attempt, inputs)
		}
		for i := range inputs {
			result, err := s.processOne(ctx, tx, inputs[i], replays[i])
			if err != nil {
				return err
			}
			results[i] = result
		}
		return nil
	})
	if err != nil {
		return nil, databaseError(ctx, err)
	}
	return results, nil
}

func (s *Store) validProcessInput(input ProcessInput) bool {
	if !validScope(input.Scope) || !s.permits(input.Scope) || input.Record.Validate() != nil ||
		incident.ValidatePersistenceDecision(input.Record, input.Decision) != nil ||
		input.IncidentID != input.Decision.IncidentID() || input.Generation != input.Decision.GenerationKey() ||
		input.DeploymentID != input.Decision.DeploymentID() || !input.EpisodeStart.Equal(input.Decision.EpisodeStart()) ||
		input.EvidenceOnly != input.Decision.EvidenceOnly() ||
		input.Record.Region != input.Scope.Region || input.Record.Redaction.PolicyVersion != s.validator.Version() ||
		s.validator.ValidateRecord(input.Record) != nil || !validUTC(input.ProcessedAt) ||
		!validText(input.FingerprintVersion, MaxScopeBytes) || !validFingerprint(input.Fingerprint) ||
		!validFingerprint(input.IncidentID) || !validText(input.DeploymentID, MaxScopeBytes) ||
		!validUTC(input.EpisodeStart) || !validDetectionStatus(input.DetectionStatus) ||
		!validText(input.Severity, MaxScopeBytes) || !validUTC(input.QuietAt) || !validUTC(input.ReopenUntil) ||
		!input.ReopenUntil.After(input.QuietAt) || input.Late != input.Decision.Late() ||
		!validRuleTrigger(input.RuleTrigger) || s.validator.ValidateText(input.RuleTrigger) != nil || input.ContextVersion < 0 {
		return false
	}
	derivedFingerprint, err := fingerprint.Error(input.Record)
	if err != nil || input.FingerprintVersion != derivedFingerprint.Version || input.Fingerprint != derivedFingerprint.Digest {
		return false
	}
	derivedIncidentID, err := incident.DeterministicID(input.Record)
	if err != nil || input.IncidentID != derivedIncidentID {
		return false
	}
	generation, err := CanonicalGeneration(input.DeploymentID, input.EpisodeStart)
	if err != nil || input.Generation != generation {
		return false
	}
	if input.Evidence != nil {
		evidence := input.Evidence
		if !validUUIDv7(evidence.EvidenceID) || evidence.Version < 1 ||
			!validClassification(evidence.Classification) || evidence.Provenance != "normalized_log" ||
			s.validator.ValidateText(evidence.Provenance) != nil ||
			!validUTC(evidence.CreatedAt) || !validUTC(evidence.ExpiresAt) || !evidence.ExpiresAt.After(evidence.CreatedAt) {
			return false
		}
	}
	// Finalized late evidence is attached to immutable existing topology. Any
	// stale investigation or context proposal carried by the caller is ignored;
	// evidence-only data can never drive agent execution or lifecycle changes.
	if input.EvidenceOnly {
		return true
	}
	// A contributing record writes its generation's lifecycle columns, and that
	// lifecycle belongs to M1. Re-deriving detection status or quiet bounds from
	// a single record cannot see the generation's other contributions.
	if input.DetectionStatus != string(input.Decision.DetectionStatus()) ||
		!input.QuietAt.Equal(input.Decision.QuietAt()) || !input.ReopenUntil.Equal(input.Decision.ReopenUntil()) {
		return false
	}
	if input.Investigation != nil && input.InvestigationCandidate != nil {
		return false
	}
	investigation := input.Investigation
	if investigation == nil {
		investigation = input.InvestigationCandidate
	}
	if investigation != nil {
		contextJSON, contextErr := encodeSafeValue(investigation.Context)
		if !validUUIDv7(investigation.InvestigationID) || !validTriggerReason(investigation.TriggerReason) ||
			investigation.ContextVersion < 1 || (input.Investigation != nil && investigation.ContextVersion != input.ContextVersion) ||
			!validClassification(investigation.Classification) || investigation.Context.Validate() != nil ||
			contextErr != nil || len(contextJSON) > MaxContextBytes ||
			!s.validOutboxMessage(investigation.Outbox) || investigation.Outbox.MessageID == investigation.InvestigationID ||
			!s.validAssignmentForProcess(input, *investigation) {
			return false
		}
		probe := input.Record
		probe.Body = investigation.Context
		probe.EventName = ""
		if s.validator.ValidateRecord(probe) != nil {
			return false
		}
		if input.InvestigationCandidate != nil && (input.ContextVersion != 0 || investigation.ContextVersion != 1 ||
			investigation.TriggerReason != "five_in_five" || input.Record.SeverityClass != model.SeverityClassError ||
			strings.TrimSpace(input.Record.Service.Name) == "" || strings.TrimSpace(input.Record.Service.Environment) == "") {
			return false
		}
	}
	if input.ContextUpdate != nil {
		update := input.ContextUpdate
		contextJSON, contextErr := encodeSafeValue(update.Snapshot)
		if input.Investigation != nil || input.InvestigationCandidate != nil || update.Version < 1 || update.Version != input.ContextVersion ||
			update.Snapshot.Validate() != nil || !validClassification(update.Classification) ||
			!validUTC(update.CreatedAt) || !s.validSafeValue(update.Snapshot) ||
			contextErr != nil || len(contextJSON) > MaxContextBytes {
			return false
		}
	}
	if input.ContextVersion > 0 && input.Investigation == nil && input.InvestigationCandidate == nil && input.ContextUpdate == nil {
		return false
	}
	return true
}

func (s *Store) validAssignmentForProcess(input ProcessInput, investigation InvestigationInput) bool {
	payload, err := agent.DecodeAssignment(investigation.Outbox.Body)
	if err != nil {
		return false
	}
	return investigation.Outbox.Type == agent.AssignmentMessageType &&
		investigation.Outbox.DeduplicationKey == "assignment:"+investigation.InvestigationID &&
		validAssignmentAttributes(investigation.Outbox.Attributes, input.Scope.Region) &&
		payload.SchemaVersion == agent.AssignmentSchemaVersion && payload.MessageID == investigation.Outbox.MessageID && payload.MessageType == investigation.Outbox.Type &&
		payload.InvestigationID == investigation.InvestigationID && payload.CorrelationID == investigation.InvestigationID && payload.Region == input.Scope.Region &&
		payload.TenantID == input.Scope.TenantID && payload.Classification == s.classification &&
		payload.IncidentID == input.IncidentID && payload.IncidentGeneration == input.Generation &&
		payload.ServiceID == input.Record.Service.Name && payload.Environment == input.Record.Service.Environment &&
		payload.Severity == input.Severity && payload.ContextVersion == investigation.ContextVersion &&
		payload.CreatedAt.Equal(input.ProcessedAt)
}

func validRuleTrigger(value string) bool {
	switch value {
	case "ordinary_error_v1":
		return true
	default:
		return false
	}
}

func (s *Store) validOutboxMessage(message queue.Message) bool {
	if message.Validate() != nil || s.validator.ValidateText(message.DeduplicationKey) != nil ||
		s.validator.ValidateText(message.Type) != nil || s.validator.ValidateText(string(message.Body)) != nil {
		return false
	}
	for name, value := range message.Attributes {
		if s.validator.ValidateText(name) != nil || s.validator.ValidateText(value) != nil {
			return false
		}
	}
	return true
}

func (s *Store) processOne(ctx context.Context, tx pgx.Tx, input ProcessInput, replay journal.ReplayIdentity) (ProcessResult, error) {
	result := ProcessResult{RecordID: input.Record.RecordID}
	semanticDigest := replay.Digest()
	var inserted string
	err := tx.QueryRow(ctx, `
		INSERT INTO records_seen (record_id, region, tenant_id, identity_version, source_type, semantic_digest, semantic_priority, replay_identity_version, first_processed_at, safe_outcome)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'processed')
		ON CONFLICT (record_id) DO NOTHING
		RETURNING record_id`,
		input.Record.RecordID, input.Scope.Region, input.Scope.TenantID, input.Record.RecordIDVersion,
		string(input.Record.Source.SourceType), semanticDigest[:], int16(replay.Priority()), replay.Version(), input.ProcessedAt).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		result.Duplicate = true
		var storedRegion, storedTenant, identityVersion, sourceType, replayVersion string
		var storedPriority int16
		var storedIncident *string
		var storedGeneration *int64
		var storedDigest []byte
		err = tx.QueryRow(ctx, `SELECT region,tenant_id,identity_version,source_type,semantic_digest,semantic_priority,replay_identity_version,incident_id,generation
			FROM records_seen WHERE record_id=$1`, input.Record.RecordID).Scan(&storedRegion, &storedTenant,
			&identityVersion, &sourceType, &storedDigest, &storedPriority, &replayVersion, &storedIncident, &storedGeneration)
		if errors.Is(err, pgx.ErrNoRows) {
			return ProcessResult{}, ErrUnavailable
		}
		if err != nil {
			return ProcessResult{}, err
		}
		if storedRegion != input.Scope.Region || storedTenant != input.Scope.TenantID ||
			identityVersion != input.Record.RecordIDVersion || sourceType != string(input.Record.Source.SourceType) ||
			!equalBytes(storedDigest, semanticDigest[:]) || storedPriority != int16(replay.Priority()) ||
			replayVersion != replay.Version() {
			return ProcessResult{}, ErrInvalidInput
		}
		if storedIncident == nil || storedGeneration == nil {
			return ProcessResult{}, ErrUnavailable
		}
		result.IncidentID = *storedIncident
		result.Generation = *storedGeneration
		return result, nil
	}
	if err != nil {
		return ProcessResult{}, err
	}
	if input.EvidenceOnly {
		return s.attachEvidenceOnly(ctx, tx, input)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO incident_families
			(region, tenant_id, incident_id, source_account, service_id, environment, fingerprint_version, fingerprint,
			 severity, detection_status, context_version, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,0,$11)
		ON CONFLICT (region, tenant_id, source_account, service_id, environment, fingerprint_version, fingerprint) DO NOTHING`,
		input.Scope.Region, input.Scope.TenantID, input.IncidentID, input.Record.Source.SourceAccount,
		input.Record.Service.Name, input.Record.Service.Environment, input.FingerprintVersion, input.Fingerprint,
		input.Severity, input.DetectionStatus, input.ProcessedAt)
	if err != nil {
		return ProcessResult{}, err
	}
	if s.hooks.beforeFamilyLock != nil {
		if err := s.hooks.beforeFamilyLock(ctx, tx, input); err != nil {
			return ProcessResult{}, err
		}
	}
	if err := tx.QueryRow(ctx, `
		SELECT incident_id FROM incident_families
		WHERE region=$1 AND tenant_id=$2 AND source_account=$3 AND service_id=$4 AND environment=$5
		  AND fingerprint_version=$6 AND fingerprint=$7
		FOR UPDATE`, input.Scope.Region, input.Scope.TenantID, input.Record.Source.SourceAccount,
		input.Record.Service.Name, input.Record.Service.Environment, input.FingerprintVersion,
		input.Fingerprint).Scan(&result.IncidentID); err != nil {
		return ProcessResult{}, err
	}

	result.Generation, err = s.canonicalGeneration(ctx, tx, input, result.IncidentID)
	if err != nil {
		return ProcessResult{}, err
	}

	if err := s.insertOccurrenceEvidence(ctx, tx, input, result); err != nil {
		return ProcessResult{}, err
	}
	_, err = tx.Exec(ctx, `
		UPDATE incident_generations SET
			first_occurrence=least(first_occurrence,$5), latest_occurrence=greatest(latest_occurrence,$5),
			occurrence_count=occurrence_count+$11, severity=$6, detection_status=$7,
			quiet_at=greatest(quiet_at,$8), reopen_until=greatest(reopen_until,$9),
			context_version=greatest(context_version,$10)
		WHERE region=$1 AND tenant_id=$2 AND incident_id=$3 AND generation=$4`,
		input.Scope.Region, input.Scope.TenantID, result.IncidentID, result.Generation, input.Record.EventTime,
		input.Severity, input.DetectionStatus, input.QuietAt, input.ReopenUntil, input.ContextVersion, int64(1))
	if err != nil {
		return ProcessResult{}, err
	}
	_, err = tx.Exec(ctx, `
		UPDATE incident_families SET severity=$4, detection_status=$5,
			latest_generation=(SELECT generation FROM incident_generations g
			  WHERE g.region=$1 AND g.tenant_id=$2 AND g.incident_id=$3
			  ORDER BY episode_start DESC,deployment_id DESC LIMIT 1),
			latest_occurrence=CASE WHEN latest_occurrence IS NULL THEN $6 ELSE greatest(latest_occurrence,$6) END
		WHERE region=$1 AND tenant_id=$2 AND incident_id=$3`, input.Scope.Region, input.Scope.TenantID,
		result.IncidentID, input.Severity, input.DetectionStatus, input.Record.EventTime)
	if err != nil {
		return ProcessResult{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE records_seen SET incident_id=$2, generation=$3 WHERE record_id=$1`,
		input.Record.RecordID, result.IncidentID, result.Generation)
	if err != nil {
		return ProcessResult{}, err
	}

	if s.hooks.beforeOutbox != nil {
		if err := s.hooks.beforeOutbox(ctx, tx); err != nil {
			return ProcessResult{}, err
		}
	}
	if input.Investigation != nil {
		if err := s.activateInvestigation(ctx, tx, input, &result); err != nil {
			return ProcessResult{}, err
		}
	} else if input.InvestigationCandidate != nil {
		eligible, err := s.fiveInFiveSatisfied(ctx, tx, input, result.IncidentID)
		if err != nil {
			return ProcessResult{}, err
		}
		if eligible {
			if err := s.activateInvestigationCandidate(ctx, tx, input, &result); err != nil {
				return ProcessResult{}, err
			}
		}
	} else if input.ContextUpdate != nil {
		if err := s.updateActiveContext(ctx, tx, input.Scope, result.IncidentID, *input.ContextUpdate, &result); err != nil {
			return ProcessResult{}, err
		}
	}
	if s.hooks.afterOutbox != nil {
		if err := s.hooks.afterOutbox(ctx, tx); err != nil {
			return ProcessResult{}, err
		}
	}
	return result, nil
}

func (s *Store) activateInvestigationCandidate(ctx context.Context, tx pgx.Tx, input ProcessInput, result *ProcessResult) error {
	var active *string
	if err := tx.QueryRow(ctx, `SELECT active_investigation_id FROM incident_families
		WHERE region=$1 AND tenant_id=$2 AND incident_id=$3 FOR UPDATE`, input.Scope.Region,
		input.Scope.TenantID, result.IncidentID).Scan(&active); err != nil {
		return err
	}
	if active != nil {
		result.InvestigationID = *active
		return nil
	}
	input.Investigation = input.InvestigationCandidate
	input.ContextVersion = input.Investigation.ContextVersion
	if err := s.activateInvestigation(ctx, tx, input, result); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE incident_generations SET context_version=greatest(context_version,$5)
		WHERE region=$1 AND tenant_id=$2 AND incident_id=$3 AND generation=$4`, input.Scope.Region,
		input.Scope.TenantID, result.IncidentID, result.Generation, input.ContextVersion)
	return err
}

// fiveInFiveSatisfied evaluates every candidate endpoint whose half-open
// window could have changed when input.Record was inserted. This makes the
// decision invariant to arrival order: a late earlier record can complete the
// window of an already-persisted later endpoint. Each inner count stops at the
// threshold, and the occurrence-time index bounds both scans to ten minutes.
func (s *Store) fiveInFiveSatisfied(ctx context.Context, tx pgx.Tx, input ProcessInput, incidentID string) (bool, error) {
	var satisfied bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM occurrences AS endpoint
		WHERE endpoint.region=$1 AND endpoint.tenant_id=$2 AND endpoint.incident_id=$3
		  AND NOT endpoint.evidence_only
		  AND endpoint.generation=$5
		  AND endpoint.event_time >= $4 AND endpoint.event_time < $4 + INTERVAL '5 minutes'
		  AND endpoint.safe_summary->>'severity_class' = 'error'
		  AND 5 <= (
			SELECT count(*) FROM (
				SELECT 1 FROM occurrences AS member
				WHERE member.region=$1 AND member.tenant_id=$2 AND member.incident_id=$3
				  AND member.generation=$5 AND NOT member.evidence_only
				  AND member.event_time > endpoint.event_time - INTERVAL '5 minutes'
				  AND member.event_time <= endpoint.event_time
				  AND member.safe_summary->>'severity_class' = 'error'
				LIMIT 5
			) AS bounded_members
		  )
		LIMIT 1
	)`, input.Scope.Region, input.Scope.TenantID, incidentID, input.Record.EventTime, input.Generation).Scan(&satisfied)
	return satisfied, err
}

func (s *Store) attachEvidenceOnly(ctx context.Context, tx pgx.Tx, input ProcessInput) (ProcessResult, error) {
	result := ProcessResult{RecordID: input.Record.RecordID, IncidentID: input.IncidentID, Generation: input.Generation}
	var incidentID string
	if err := tx.QueryRow(ctx, `SELECT incident_id FROM incident_families
		WHERE region=$1 AND tenant_id=$2 AND incident_id=$3 AND source_account=$4
		  AND service_id=$5 AND environment=$6 AND fingerprint_version=$7 AND fingerprint=$8
		FOR UPDATE`, input.Scope.Region, input.Scope.TenantID, input.IncidentID,
		input.Record.Source.SourceAccount, input.Record.Service.Name, input.Record.Service.Environment,
		input.FingerprintVersion, input.Fingerprint).Scan(&incidentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ProcessResult{}, ErrInvalidInput
		}
		return ProcessResult{}, err
	}
	var generation int64
	if err := tx.QueryRow(ctx, `SELECT generation FROM incident_generations
		WHERE region=$1 AND tenant_id=$2 AND incident_id=$3 AND generation=$4
		  AND deployment_id=$5 AND episode_start=$6`, input.Scope.Region, input.Scope.TenantID,
		input.IncidentID, input.Generation, input.DeploymentID, input.EpisodeStart).Scan(&generation); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ProcessResult{}, ErrInvalidInput
		}
		return ProcessResult{}, err
	}
	if err := s.insertOccurrenceEvidence(ctx, tx, input, result); err != nil {
		return ProcessResult{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE records_seen SET incident_id=$2,generation=$3 WHERE record_id=$1`,
		input.Record.RecordID, result.IncidentID, result.Generation); err != nil {
		return ProcessResult{}, err
	}
	return result, nil
}

func (s *Store) insertOccurrenceEvidence(ctx context.Context, tx pgx.Tx, input ProcessInput, result ProcessResult) error {
	summary, err := s.encodeOccurrenceProjection(input.Record)
	if err != nil {
		return err
	}
	var regionalReference []byte
	if input.Record.RawReference != nil {
		regionalReference, err = json.Marshal(input.Record.RawReference)
		if err != nil {
			return ErrInvalidInput
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO occurrences
		(region,tenant_id,record_id,incident_id,generation,event_time,observed_time,
		 fingerprint_version,fingerprint,safe_summary,trace_token,span_token,late,
		 evidence_only,regional_evidence_reference)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		input.Scope.Region, input.Scope.TenantID, input.Record.RecordID, result.IncidentID, result.Generation,
		input.Record.EventTime, input.Record.ObservedTime, input.FingerprintVersion, input.Fingerprint,
		summary, nullString(input.Record.Correlation.TraceID), nullString(input.Record.Correlation.SpanID),
		input.Late, input.EvidenceOnly, nullBytes(regionalReference)); err != nil {
		return err
	}
	if input.Evidence == nil {
		return nil
	}
	evidence := input.Evidence
	_, err = tx.Exec(ctx, `INSERT INTO evidence
		(region,tenant_id,evidence_id,incident_id,generation,version,classification,
		 safe_payload,regional_reference,provenance,created_at,expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		input.Scope.Region, input.Scope.TenantID, evidence.EvidenceID, result.IncidentID, result.Generation,
		evidence.Version, evidence.Classification, summary, nullBytes(regionalReference), evidence.Provenance,
		evidence.CreatedAt, evidence.ExpiresAt)
	return err
}

func (s *Store) encodeOccurrenceProjection(record model.NormalizedLog) ([]byte, error) {
	projection := OccurrenceProjection{
		SchemaVersion:      OccurrenceProjectionSchemaVersion,
		Region:             record.Region,
		EventTime:          record.EventTime,
		ObservedTime:       record.ObservedTime,
		TimestampInferred:  record.TimestampInferred,
		TimestampReason:    record.TimestampInferenceReason,
		SeverityNumber:     record.SeverityNumber,
		SeverityText:       record.SeverityText,
		SeverityClass:      record.SeverityClass,
		Body:               record.Body,
		EventName:          record.EventName,
		Attributes:         record.Attributes,
		ResourceAttributes: record.ResourceAttributes,
		ScopeAttributes:    record.ScopeAttributes,
		Service:            record.Service,
		Deployment:         record.Deployment,
		Correlation:        record.Correlation,
		Exception:          record.Exception,
		Redaction:          record.Redaction,
	}
	if !s.validOccurrenceProjection(projection) {
		return nil, ErrInvalidInput
	}
	encoded, err := json.Marshal(projection)
	if err != nil || len(encoded) > MaxProjectionBytes {
		return nil, ErrInvalidInput
	}
	return encoded, nil
}

func (s *Store) validOccurrenceProjection(projection OccurrenceProjection) bool {
	if projection.SchemaVersion != OccurrenceProjectionSchemaVersion || !validUTC(projection.EventTime) ||
		!validUTC(projection.ObservedTime) || !validText(projection.Region, MaxScopeBytes) ||
		projection.Redaction.PolicyVersion != s.validator.Version() {
		return false
	}
	// The final prohibited-content scan is rerun over the exact fields encoded
	// in the projection, rather than trusting the SafeValue type name.
	probe := model.NormalizedLog{
		SchemaVersion:   projection.SchemaVersion,
		RecordID:        strings.Repeat("0", 64),
		RecordIDVersion: model.RecordIDVersionOTLPV1,
		IdentityQuality: model.IdentityQualityNative,
		BatchID:         "projection-validation",
		Source: model.TrustedEnvelope{
			SourceType:          model.SourceTypeOTLP,
			SourceAccount:       "projection-validation",
			Region:              projection.Region,
			AllowedEnvironments: []string{"projection-validation"},
			AllowedServices:     []string{"projection-validation"},
			SourceInstance:      "projection-validation",
			CredentialIdentity:  "projection-validation",
			ReceivedAt:          projection.ObservedTime,
		},
		Region:                   projection.Region,
		EventTime:                projection.EventTime,
		ObservedTime:             projection.ObservedTime,
		TimestampInferred:        projection.TimestampInferred,
		TimestampInferenceReason: projection.TimestampReason,
		SeverityNumber:           projection.SeverityNumber,
		SeverityText:             projection.SeverityText,
		SeverityClass:            projection.SeverityClass,
		Body:                     projection.Body,
		EventName:                projection.EventName,
		Attributes:               projection.Attributes,
		ResourceAttributes:       projection.ResourceAttributes,
		ScopeAttributes:          projection.ScopeAttributes,
		Service:                  projection.Service,
		Deployment:               projection.Deployment,
		Correlation:              projection.Correlation,
		Exception:                projection.Exception,
		Redaction:                projection.Redaction,
	}
	return probe.Validate() == nil && s.validator.ValidateRecord(probe) == nil
}

// CanonicalGeneration derives the immutable database generation identity from
// the episode key. Callers compute it before entering Process, so the retry
// callback reuses it. It is intentionally not a mutable display ordinal: late
// earlier episodes never rewrite occurrence or evidence history.
func CanonicalGeneration(deploymentID string, episodeStart time.Time) (int64, error) {
	if !validText(deploymentID, MaxScopeBytes) || !validUTC(episodeStart) {
		return 0, ErrInvalidInput
	}
	generation, err := incident.GenerationKey(deploymentID, episodeStart)
	if err != nil {
		return 0, ErrInvalidInput
	}
	return generation, nil
}

func (s *Store) canonicalGeneration(ctx context.Context, tx pgx.Tx, input ProcessInput, incidentID string) (int64, error) {
	_, err := tx.Exec(ctx, `INSERT INTO incident_generations
		(region, tenant_id, incident_id, generation, deployment_id, episode_start, first_occurrence,
		 latest_occurrence, occurrence_count, severity, detection_status, quiet_at, reopen_until,
		 rule_trigger, context_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7,0,$8,$9,$10,$11,$12,$13)
		ON CONFLICT DO NOTHING`, input.Scope.Region,
		input.Scope.TenantID, incidentID, input.Generation, input.DeploymentID, input.EpisodeStart,
		input.Record.EventTime, input.Severity, input.DetectionStatus, input.QuietAt, input.ReopenUntil,
		input.RuleTrigger, input.ContextVersion)
	if err != nil {
		return 0, err
	}
	var canonical int64
	if err := tx.QueryRow(ctx, `SELECT generation FROM incident_generations
		WHERE region=$1 AND tenant_id=$2 AND incident_id=$3 AND deployment_id=$4 AND episode_start=$5`,
		input.Scope.Region, input.Scope.TenantID, incidentID, input.DeploymentID, input.EpisodeStart).Scan(&canonical); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, err
		}
		var occupied bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM incident_generations
			WHERE region=$1 AND tenant_id=$2 AND incident_id=$3 AND generation=$4)`, input.Scope.Region,
			input.Scope.TenantID, incidentID, input.Generation).Scan(&occupied); err != nil {
			return 0, err
		}
		if occupied {
			return 0, ErrIdentityConflict
		}
		return 0, ErrUnavailable
	}
	if canonical != input.Generation {
		return 0, ErrIdentityConflict
	}
	return canonical, nil
}

func (s *Store) activateInvestigation(ctx context.Context, tx pgx.Tx, input ProcessInput, result *ProcessResult) error {
	var active *string
	if err := tx.QueryRow(ctx, `SELECT active_investigation_id FROM incident_families
		WHERE region=$1 AND tenant_id=$2 AND incident_id=$3 FOR UPDATE`,
		input.Scope.Region, input.Scope.TenantID, result.IncidentID).Scan(&active); err != nil {
		return err
	}
	if active != nil {
		result.InvestigationID = *active
		context := ContextInput{Version: input.Investigation.ContextVersion, Snapshot: input.Investigation.Context,
			Classification: input.Investigation.Classification, CreatedAt: input.ProcessedAt}
		return s.updateActiveContext(ctx, tx, input.Scope, result.IncidentID, context, result)
	}
	inv := input.Investigation
	contextJSON, err := encodeSafeValue(inv.Context)
	if err != nil {
		return err
	}
	attributes, err := json.Marshal(inv.Outbox.Attributes)
	if err != nil {
		return ErrInvalidInput
	}
	contentDigest := outboxContentDigest(inv.Outbox, "investigation", inv.InvestigationID, agent.AssignmentSchemaVersion)
	if _, err := tx.Exec(ctx, `INSERT INTO investigations
		(region,tenant_id,investigation_id,incident_id,state,trigger_reason,context_version,queued_at)
		VALUES ($1,$2,$3,$4,'queued',$5,$6,$7)`, input.Scope.Region, input.Scope.TenantID,
		inv.InvestigationID, result.IncidentID, inv.TriggerReason, inv.ContextVersion, input.ProcessedAt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO investigation_claims
		(region,tenant_id,incident_id,investigation_id) VALUES ($1,$2,$3,$4)`, input.Scope.Region,
		input.Scope.TenantID, result.IncidentID, inv.InvestigationID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO investigation_contexts
		(region,tenant_id,investigation_id,version,snapshot,classification,policy_version,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, input.Scope.Region, input.Scope.TenantID,
		inv.InvestigationID, inv.ContextVersion, contextJSON, inv.Classification, s.validator.Version(), input.ProcessedAt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outbox_messages
		(region,tenant_id,message_id,deduplication_key,message_type,aggregate_type,aggregate_id,
		 payload_version,payload,attributes,content_digest,state,next_attempt_at,created_at)
		VALUES ($1,$2,$3,$4,$5,'investigation',$6,'1.0',$7,$8,$9,'pending',$10,$10)`, input.Scope.Region,
		input.Scope.TenantID, inv.Outbox.MessageID, inv.Outbox.DeduplicationKey, inv.Outbox.Type,
		inv.InvestigationID, inv.Outbox.Body, attributes, contentDigest[:], input.ProcessedAt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE incident_families SET active_investigation_id=$4, context_version=$5
		WHERE region=$1 AND tenant_id=$2 AND incident_id=$3`, input.Scope.Region, input.Scope.TenantID,
		result.IncidentID, inv.InvestigationID, inv.ContextVersion); err != nil {
		return err
	}
	result.InvestigationID = inv.InvestigationID
	result.InvestigationCreated = true
	return nil
}

func (s *Store) updateActiveContext(ctx context.Context, tx pgx.Tx, scope Scope, incidentID string, update ContextInput, result *ProcessResult) error {
	var investigationID *string
	if err := tx.QueryRow(ctx, `SELECT active_investigation_id FROM incident_families
		WHERE region=$1 AND tenant_id=$2 AND incident_id=$3 FOR UPDATE`, scope.Region, scope.TenantID,
		incidentID).Scan(&investigationID); err != nil {
		return err
	}
	if investigationID == nil {
		return ErrInvalidInput
	}
	result.InvestigationID = *investigationID
	encoded, err := encodeSafeValue(update.Snapshot)
	if err != nil {
		return err
	}
	appended, err := s.appendInvestigationContextTx(ctx, tx, InvestigationContext{
		Scope: scope, InvestigationID: *investigationID, Version: update.Version, Snapshot: update.Snapshot,
		Classification: update.Classification, PolicyVersion: s.validator.Version(), CreatedAt: update.CreatedAt,
	}, encoded)
	if err != nil {
		return err
	}
	if !appended {
		return nil
	}
	_, err = tx.Exec(ctx, `UPDATE incident_families SET context_version=$4
		WHERE region=$1 AND tenant_id=$2 AND incident_id=$3`, scope.Region, scope.TenantID,
		incidentID, update.Version)
	return err
}

func nullString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func nullBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}
