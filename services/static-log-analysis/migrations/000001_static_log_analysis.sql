-- Static log analysis schema, version 1.
--
-- This is the reviewed single-region schema. Production deploys one analysis
-- service and one CockroachDB database per region; ApplyMigrations refuses a
-- CockroachDB multi-region database. Region/tenant key prefixes remain as
-- defense in depth. REGIONAL BY ROW is intentionally unsupported by this schema.

CREATE TABLE records_seen (
    record_id STRING PRIMARY KEY,
    region STRING NOT NULL,
    tenant_id STRING NOT NULL,
    identity_version STRING NOT NULL,
    source_type STRING NOT NULL,
    semantic_digest BYTES NOT NULL,
    semantic_priority INT2 NOT NULL,
    replay_identity_version STRING NOT NULL,
    first_processed_at TIMESTAMPTZ NOT NULL,
    incident_id STRING NULL,
    generation INT8 NULL,
    safe_outcome STRING NOT NULL,
    CONSTRAINT records_seen_scope_nonempty CHECK (length(region) BETWEEN 1 AND 128 AND length(tenant_id) BETWEEN 1 AND 128),
    CONSTRAINT records_seen_record_id_shape CHECK (record_id ~ '^[0-9a-f]{64}$'),
    CONSTRAINT records_seen_incident_id_shape CHECK (incident_id IS NULL OR incident_id ~ '^[0-9a-f]{64}$'),
    CONSTRAINT records_seen_semantic_digest_shape CHECK (length(semantic_digest) = 32),
    CONSTRAINT records_seen_semantic_priority CHECK (semantic_priority BETWEEN 0 AND 3),
    CONSTRAINT records_seen_generation_positive CHECK (generation IS NULL OR generation > 0),
    INDEX records_seen_by_scope (region, tenant_id, record_id)
);

-- migrate:split
CREATE TABLE incident_families (
    region STRING NOT NULL,
    tenant_id STRING NOT NULL,
    incident_id STRING NOT NULL,
    source_account STRING NOT NULL,
    service_id STRING NOT NULL,
    environment STRING NOT NULL,
    fingerprint_version STRING NOT NULL,
    fingerprint STRING NOT NULL,
    severity STRING NOT NULL,
    detection_status STRING NOT NULL,
    latest_generation INT8 NULL,
    latest_occurrence TIMESTAMPTZ NULL,
    active_investigation_id STRING NULL,
    context_version INT8 NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (region, tenant_id, incident_id),
    CONSTRAINT incident_families_incident_id_shape CHECK (incident_id ~ '^[0-9a-f]{64}$'),
    CONSTRAINT incident_families_source_account_nonempty CHECK (length(source_account) BETWEEN 1 AND 128),
    CONSTRAINT incident_families_natural_key UNIQUE (region, tenant_id, source_account, service_id, environment, fingerprint_version, fingerprint),
    CONSTRAINT incident_families_status CHECK (detection_status IN ('active', 'quiet', 'ended')),
    CONSTRAINT incident_families_context_nonnegative CHECK (context_version >= 0),
    CONSTRAINT incident_families_latest_generation_positive CHECK (latest_generation IS NULL OR latest_generation > 0)
);

-- migrate:split
CREATE TABLE incident_generations (
    region STRING NOT NULL,
    tenant_id STRING NOT NULL,
    incident_id STRING NOT NULL,
    generation INT8 NOT NULL,
    deployment_id STRING NOT NULL,
    episode_start TIMESTAMPTZ NOT NULL,
    first_occurrence TIMESTAMPTZ NOT NULL,
    latest_occurrence TIMESTAMPTZ NOT NULL,
    occurrence_count INT8 NOT NULL,
    severity STRING NOT NULL,
    detection_status STRING NOT NULL,
    quiet_at TIMESTAMPTZ NOT NULL,
    reopen_until TIMESTAMPTZ NOT NULL,
    rule_trigger STRING NOT NULL,
    context_version INT8 NOT NULL DEFAULT 0,
    PRIMARY KEY (region, tenant_id, incident_id, generation),
    CONSTRAINT incident_generations_episode_key UNIQUE (region, tenant_id, incident_id, deployment_id, episode_start),
    CONSTRAINT incident_generations_family_fk FOREIGN KEY (region, tenant_id, incident_id) REFERENCES incident_families (region, tenant_id, incident_id),
    CONSTRAINT incident_generations_positive CHECK (generation > 0 AND occurrence_count >= 0 AND context_version >= 0),
    CONSTRAINT incident_generations_status CHECK (detection_status IN ('active', 'quiet', 'ended')),
    CONSTRAINT incident_generations_rule_trigger CHECK (rule_trigger IN ('ordinary_error_v1')),
    CONSTRAINT incident_generations_times CHECK (latest_occurrence >= first_occurrence AND quiet_at >= latest_occurrence AND reopen_until >= quiet_at)
);

-- migrate:split
CREATE TABLE occurrences (
    region STRING NOT NULL,
    tenant_id STRING NOT NULL,
    record_id STRING NOT NULL,
    incident_id STRING NOT NULL,
    generation INT8 NOT NULL,
    event_time TIMESTAMPTZ NOT NULL,
    observed_time TIMESTAMPTZ NOT NULL,
    fingerprint_version STRING NOT NULL,
    fingerprint STRING NOT NULL,
    safe_summary JSONB NOT NULL,
    trace_token STRING NULL,
    span_token STRING NULL,
    late BOOL NOT NULL,
    evidence_only BOOL NOT NULL,
    regional_evidence_reference JSONB NULL,
    PRIMARY KEY (region, tenant_id, record_id),
    CONSTRAINT occurrences_record_id_unique UNIQUE (record_id),
    CONSTRAINT occurrences_generation_fk FOREIGN KEY (region, tenant_id, incident_id, generation) REFERENCES incident_generations (region, tenant_id, incident_id, generation)
);

-- migrate:split
CREATE TABLE evidence (
    region STRING NOT NULL,
    tenant_id STRING NOT NULL,
    evidence_id STRING NOT NULL,
    incident_id STRING NOT NULL,
    generation INT8 NOT NULL,
    version INT8 NOT NULL,
    classification STRING NOT NULL,
    safe_payload JSONB NOT NULL,
    regional_reference JSONB NULL,
    provenance STRING NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (region, tenant_id, evidence_id),
    CONSTRAINT evidence_id_uuidv7 CHECK (evidence_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    CONSTRAINT evidence_generation_fk FOREIGN KEY (region, tenant_id, incident_id, generation) REFERENCES incident_generations (region, tenant_id, incident_id, generation),
    CONSTRAINT evidence_version_positive CHECK (version > 0),
    CONSTRAINT evidence_classification CHECK (classification IN ('PUBLIC', 'INTERNAL', 'SENSITIVE', 'RESTRICTED')),
    CONSTRAINT evidence_provenance CHECK (provenance IN ('normalized_log')),
    CONSTRAINT evidence_expiry CHECK (expires_at > created_at)
);

-- migrate:split
CREATE TABLE investigations (
    region STRING NOT NULL,
    tenant_id STRING NOT NULL,
    investigation_id STRING NOT NULL,
    incident_id STRING NOT NULL,
    state STRING NOT NULL,
    trigger_reason STRING NOT NULL,
    context_version INT8 NOT NULL,
    report_reference STRING NULL,
    queued_at TIMESTAMPTZ NOT NULL,
    started_at TIMESTAMPTZ NULL,
    completed_at TIMESTAMPTZ NULL,
    PRIMARY KEY (region, tenant_id, investigation_id),
    CONSTRAINT investigations_id_uuidv7 CHECK (investigation_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    CONSTRAINT investigations_family_pair_unique UNIQUE (region, tenant_id, incident_id, investigation_id),
    CONSTRAINT investigations_family_fk FOREIGN KEY (region, tenant_id, incident_id) REFERENCES incident_families (region, tenant_id, incident_id),
    CONSTRAINT investigations_context_positive CHECK (context_version > 0),
    CONSTRAINT investigations_state CHECK (state IN ('queued', 'claimed', 'preparing_workspace', 'investigating', 'forming_hypothesis', 'writing_report', 'completed', 'failed', 'timed_out', 'cancelled', 'superseded'))
);

-- migrate:split
ALTER TABLE incident_families ADD CONSTRAINT incident_families_active_investigation_fk
    FOREIGN KEY (region, tenant_id, incident_id, active_investigation_id)
    REFERENCES investigations (region, tenant_id, incident_id, investigation_id);

-- migrate:split
CREATE TABLE investigation_claims (
    region STRING NOT NULL,
    tenant_id STRING NOT NULL,
    incident_id STRING NOT NULL,
    investigation_id STRING NOT NULL,
    owner STRING NULL,
    lease_token STRING NULL,
    lease_expires_at TIMESTAMPTZ NULL,
    renewal_sequence INT8 NOT NULL DEFAULT 0,
    PRIMARY KEY (region, tenant_id, incident_id),
    CONSTRAINT investigation_claims_investigation_unique UNIQUE (region, tenant_id, investigation_id),
    CONSTRAINT investigation_claims_family_fk FOREIGN KEY (region, tenant_id, incident_id) REFERENCES incident_families (region, tenant_id, incident_id),
    CONSTRAINT investigation_claims_investigation_fk FOREIGN KEY (region, tenant_id, incident_id, investigation_id) REFERENCES investigations (region, tenant_id, incident_id, investigation_id),
    CONSTRAINT investigation_claims_lease_shape CHECK ((owner IS NULL AND lease_token IS NULL AND lease_expires_at IS NULL) OR (owner IS NOT NULL AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)),
    CONSTRAINT investigation_claims_token_uuidv7 CHECK (lease_token IS NULL OR lease_token ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    CONSTRAINT investigation_claims_renewal_nonnegative CHECK (renewal_sequence >= 0)
);

-- migrate:split
CREATE TABLE investigation_progress (
    region STRING NOT NULL,
    tenant_id STRING NOT NULL,
    investigation_id STRING NOT NULL,
    sequence INT8 NOT NULL,
    state STRING NOT NULL,
    safe_payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (region, tenant_id, investigation_id, sequence),
    CONSTRAINT investigation_progress_investigation_fk FOREIGN KEY (region, tenant_id, investigation_id) REFERENCES investigations (region, tenant_id, investigation_id),
    CONSTRAINT investigation_progress_sequence_positive CHECK (sequence > 0)
);

-- migrate:split
CREATE TABLE investigation_contexts (
    region STRING NOT NULL,
    tenant_id STRING NOT NULL,
    investigation_id STRING NOT NULL,
    version INT8 NOT NULL,
    snapshot JSONB NOT NULL,
    classification STRING NOT NULL,
    policy_version STRING NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (region, tenant_id, investigation_id, version),
    CONSTRAINT investigation_contexts_investigation_fk FOREIGN KEY (region, tenant_id, investigation_id) REFERENCES investigations (region, tenant_id, investigation_id),
    CONSTRAINT investigation_contexts_version_positive CHECK (version > 0),
    CONSTRAINT investigation_contexts_classification CHECK (classification IN ('PUBLIC', 'INTERNAL', 'SENSITIVE', 'RESTRICTED'))
);

-- migrate:split
CREATE TABLE reports (
    region STRING NOT NULL,
    tenant_id STRING NOT NULL,
    report_id STRING NOT NULL,
    investigation_id STRING NOT NULL,
    structured_report JSONB NOT NULL,
    classification STRING NOT NULL,
    provenance STRING NOT NULL,
    completed_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    embedding_reference STRING NULL,
    PRIMARY KEY (region, tenant_id, report_id),
    CONSTRAINT reports_id_uuidv7 CHECK (report_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    CONSTRAINT reports_investigation_unique UNIQUE (region, tenant_id, investigation_id),
    CONSTRAINT reports_investigation_fk FOREIGN KEY (region, tenant_id, investigation_id) REFERENCES investigations (region, tenant_id, investigation_id),
    CONSTRAINT reports_classification CHECK (classification IN ('PUBLIC', 'INTERNAL', 'SENSITIVE', 'RESTRICTED')),
    CONSTRAINT reports_expiry CHECK (expires_at > completed_at)
);

-- migrate:split
CREATE TABLE outbox_messages (
    region STRING NOT NULL,
    tenant_id STRING NOT NULL,
    message_id STRING NOT NULL,
    deduplication_key STRING NOT NULL,
    message_type STRING NOT NULL,
    aggregate_type STRING NOT NULL,
    aggregate_id STRING NOT NULL,
    payload_version STRING NOT NULL,
    payload BYTES NOT NULL,
    attributes JSONB NOT NULL,
    content_digest BYTES NOT NULL,
    state STRING NOT NULL,
    attempts INT8 NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    claim_owner STRING NULL,
    claim_token STRING NULL,
    claim_expires_at TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL,
    published_at TIMESTAMPTZ NULL,
    last_safe_error STRING NULL,
    PRIMARY KEY (region, tenant_id, message_id),
    CONSTRAINT outbox_messages_id_uuidv7 CHECK (message_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    CONSTRAINT outbox_messages_claim_token_uuidv7 CHECK (claim_token IS NULL OR claim_token ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    CONSTRAINT outbox_messages_deduplication_unique UNIQUE (region, tenant_id, deduplication_key),
    CONSTRAINT outbox_messages_content_digest_length CHECK (length(content_digest) = 32),
    CONSTRAINT outbox_messages_state CHECK (state IN ('pending', 'claimed', 'published')),
    CONSTRAINT outbox_messages_attempts_nonnegative CHECK (attempts >= 0),
    CONSTRAINT outbox_messages_claim_shape CHECK ((state = 'claimed' AND claim_owner IS NOT NULL AND claim_token IS NOT NULL AND claim_expires_at IS NOT NULL) OR (state <> 'claimed' AND claim_owner IS NULL AND claim_token IS NULL AND claim_expires_at IS NULL)),
    CONSTRAINT outbox_messages_published_shape CHECK ((state = 'published' AND published_at IS NOT NULL) OR state <> 'published'),
    INDEX outbox_messages_claimable (region, tenant_id, state, next_attempt_at, claim_expires_at, created_at)
);

-- migrate:split
CREATE TABLE audit_events (
    region STRING NOT NULL,
    tenant_id STRING NOT NULL,
    audit_id STRING NOT NULL,
    actor STRING NOT NULL,
    action STRING NOT NULL,
    target_type STRING NOT NULL,
    target_id STRING NOT NULL,
    safe_details JSONB NOT NULL,
    source_identity STRING NOT NULL,
    source_address STRING NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (region, tenant_id, audit_id),
    CONSTRAINT audit_events_id_uuidv7 CHECK (audit_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$')
);
