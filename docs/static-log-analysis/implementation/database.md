# CockroachDB Schema and Transactions

The following is a normative logical schema. Exact CockroachDB types and locality
clauses are defined in reviewed migrations.

## Tables

### `records_seen`

Primary key `record_id`; stores identity version, source, first processing time,
incident generation, and safe processing outcome. This is the global effectively-
once gate. Retention MUST cover the maximum raw-source replay horizon plus lateness
and reopen periods.

### `incident_families`

Primary key `incident_id`; unique `(region, tenant_id, service_id, environment,
fingerprint_version, fingerprint)`. Stores current severity, detection status,
latest generation, latest occurrence, and active investigation reference.

### `incident_generations`

Primary key `(incident_id, generation)`; unique generation key including deployment
and episode start. Stores first/latest occurrence, count, severity, quiet/reopen
times, deployment, rule trigger, and context version.

### `occurrences`

Primary key `record_id`; foreign key to family/generation. Stores safe summary,
event/observed times, fingerprint, correlation tokens, late classification, and
regional evidence reference. Large repeated evidence MAY be deterministically
sampled after its unique contribution is stored.

### `evidence`

Immutable primary key `evidence_id`; includes incident/generation, version,
classification, safe payload or regional reference, provenance, created time, and
expiry. Corrections append rather than overwrite.

### `investigations`

Primary key `investigation_id`; unique active investigation enforced per incident
family using an active-claim row described below. Stores state, trigger reason,
context version, report reference, and lifecycle times.

### `investigation_claims`

Primary key `incident_id`; unique `investigation_id`; stores owner, lease token,
lease expiry, and renewal sequence. A compare-and-set lease token is required for
renewal and completion so an expired worker cannot overwrite a successor.

### `investigation_progress`

Primary key `(investigation_id, sequence)`; append-only structured progress.

### `investigation_contexts`

Primary key `(investigation_id, version)`; immutable context snapshots or manifests.
Agents compare their current version at investigation checkpoints. The initial
implementation uses version polling; no separate context-update queue is required.

### `reports`

Primary key `report_id`; unique investigation reference; stores structured report,
classification, provenance, completion time, expiry, and embedding reference.

### `outbox_messages`

Primary key `message_id`; unique `deduplication_key`; stores type, aggregate,
versioned payload, state, attempts, next attempt, created/published times, and last
safe error. Claims use expiry and fencing tokens.

### `audit_events`

Append-only primary key `audit_id`; actor, action, target, region, safe details,
source address/workload identity, and timestamp.

## Processing transaction

For each bounded worker batch, the retryable transaction:

1. Inserts `records_seen`; an existing key makes that record a no-op.
2. Upserts the incident family and appropriate generation.
3. Inserts occurrence and selected evidence.
4. Updates aggregate counts only for newly inserted records.
5. Applies investigation policy.
6. If required, inserts investigation plus outbox row and establishes the family
   active-investigation reference atomically.

The transaction callback MUST be safe to execute multiple times. External calls,
random ID generation, and wall-clock reads occur before entering the retryable
callback and their values are reused on every retry.

## Outbox transaction behavior

Publishing never occurs inside a database transaction. Workers claim with a
fencing token, publish to SQS, then conditionally mark published. A lost database
update may republish; orchestrator idempotency handles it.

## Contention guidance

No global counters are updated per record. Work batches are bounded. Family rows
may be hot during an incident; counters SHOULD use single-statement conditional
updates and be verified under concurrent load. Exact occurrence truth remains in
`records_seen`/`occurrences`, permitting aggregate repair.

## Regional locality

Every log-derived table includes region in its key or locality design. Migrations
MUST declare CockroachDB locality appropriate to deployment topology and prove
through tests that application queries cannot read another region without an
explicit privileged path.
