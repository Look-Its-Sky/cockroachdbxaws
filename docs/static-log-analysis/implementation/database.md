# CockroachDB Schema and Transactions

The following is a normative logical schema. Exact CockroachDB types and the
supported topology guard are defined in reviewed migrations and catalog checks.

An ordinary persistence `Store` is constructed for one immutable `(region,
tenant_id)` scope. Every ordinary read, write, lease, and outbox operation MUST
reject a different scope. Cross-scope administrative access requires a separate,
explicitly privileged path; changing a request parameter is not such a path.

Batch processing decides every reason the Store cannot serve a batch at all —
an unserved scope or a redaction-policy version other than its own — before it
judges any individual record, and reports them as a configuration failure
distinct from record-local invalidity. A caller isolating record-local
invalidity destroys the durable payload it isolates, so it must never mistake a
deployment-wide contradiction for a malformed record. Classification cannot be
separated this way: it reaches records only through the sealed M3 replay digest,
where a misconfigured boundary is indistinguishable from a forged record and
both MUST fail closed. The Store therefore exposes its immutable boundary
(scope, classification, redaction-policy version) so a coordinator proves
agreement once at startup instead.

## Tables

### `records_seen`

Primary key `record_id`; stores identity version, source, the sealed/versioned M3
semantic digest and admission priority, first processing time, incident generation,
and safe processing outcome. This is the global effectively-once gate. Retention
MUST cover the maximum raw-source replay horizon plus lateness and reopen periods.
A duplicate is a no-op only when scope, source, identity version, exact M3 replay
identity, and priority match. Any conflict, including one found in another scope,
MUST fail closed after journal compaction as well as before it.

### `incident_families`

Primary key `incident_id`; unique `(region, tenant_id, source_account, service_id,
environment, fingerprint_version, fingerprint)`. Stores current severity, detection status,
latest generation, latest occurrence, and active investigation reference. The
`incident_id` is the same deterministic, 64-character lowercase hexadecimal M1
incident identity; persistence does not mint a second identity for the family.

### `incident_generations`

Primary key `(incident_id, generation)`; unique generation key including deployment
and episode start. Stores first/latest occurrence, count, severity, quiet/reopen
times, deployment, rule trigger, and context version. `generation` is an opaque,
positive, deterministic key derived from the finalized M1 deployment episode. It
is not a display ordinal. M1 alone mints an opaque, record-bound persistence
decision after that record's full event-time ordering position is strictly behind
the irreversible frontier. The decision carries its version, frontier, deployment,
stable episode start, and key; M4 validates it through the shared M1 contract.
Earlier late arrivals therefore never renumber a generation or rewrite historical
foreign keys. A provisional journal record remains pending and replayable until M1
can issue this decision.

The deterministic generation is a signed 63-bit hash key, so a collision is
possible even though it is extraordinarily unlikely. The write first performs an
`INSERT ... ON CONFLICT DO NOTHING`, then distinguishes an existing identical
deployment/episode key from an occupied generation primary key. The latter is a
terminal identity conflict: the whole record transaction rolls back, the database
retry loop does not retry it, and operators must investigate rather than silently
attaching the record to the wrong episode.

### `occurrences`

Primary key `record_id`; foreign key to family/generation. Stores safe summary,
event/observed times, fingerprint, correlation tokens, late classification, and
regional evidence reference. Large repeated evidence MAY be deterministically
sampled after its unique contribution is stored.

The safe summary is occurrence projection schema `1.0`, encoded as JSON and
limited to 256 KiB. It retains event and observed times, severity, safe body and
event name, log/resource/scope attributes, service and deployment identity,
correlation identity, normalized exception including safe stack frames, and
redaction metadata. It excludes transport-attempt metadata and credentials. The
exact projection MUST pass the final prohibited-content validator immediately
before persistence and again when read; the selected evidence payload uses the
same projection bytes.

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
Any replay at the current or an older version succeeds only when snapshot,
classification, policy version, and creation time exactly match the stored row;
a missing or conflicting immutable version rolls back the complete record
transaction.

### `reports`

Primary key `report_id`; unique investigation reference; stores structured report,
classification, provenance, completion time, expiry, and embedding reference.

### `outbox_messages`

Primary key `message_id`; unique `deduplication_key`; stores type, aggregate,
versioned payload, state, attempts, next attempt, created/published times, and last
safe error. Claims use expiry and fencing tokens.
Before mutation, a claim also validates the complete assignment catalogue:
message type `agent.assignment.v1`, investigation aggregate type and UUIDv7 ID,
payload version and payload schema `1.0`, and the exact
`assignment:<aggregate-id>` deduplication key. Corrupt rows fail closed without
advancing attempts or claim state.
Each row also stores the required 32-byte `content_digest`. Its versioned,
domain-separated SHA-256 input length-delimits message ID, deduplication key,
message type, aggregate type and ID, payload version, exact payload bytes, and
canonical key-sorted attributes. Claim recomputes and constant-time compares the
digest before typed semantic validation or mutation, so even a different valid
severity, existing generation/context pointer, or safe routing value fails closed.

### `audit_events`

Append-only primary key `audit_id`; actor, action, target, region, safe details,
source address/workload identity, and timestamp.

## Processing transaction

For each bounded worker batch, the retryable transaction:

1. Inserts `records_seen`; an existing key makes that record a no-op.
2. Upserts the incident family and appropriate generation.
3. Inserts occurrence and selected evidence.
4. Updates aggregate counts only for newly inserted contributing records. An
   evidence-only record is retained as an occurrence/evidence row but contributes
   zero and cannot create a family/generation or change severity, state, episode
   bounds, context, or any other aggregate. It must attach to an existing frozen
   M1 family/generation or the transaction fails closed.
5. Applies investigation policy.
6. If required, inserts investigation plus outbox row and establishes the family
   active-investigation reference atomically.

At most one investigation is active for a family. A newer immutable context for
that active investigation appends a context version without creating another
investigation or outbox message. Investigation, initial context, claim row, outbox
message, and active-family reference either commit together or do not commit.

The transaction callback MUST be safe to execute multiple times. External calls,
random ID generation, and wall-clock reads occur before entering the retryable
callback and their values are reused on every retry.

## Outbox transaction behavior

Publishing never occurs inside a database transaction. Workers claim with a
fencing token, publish to SQS, then conditionally mark published. A lost database
update may republish the identical `message_id`, deduplication key, and payload;
orchestrator idempotency handles it. Claims are bounded, increment attempts, and
may be reclaimed when `claim_expires_at <= now`. Publish and retry updates require
the current token and an unexpired claim (`claim_expires_at > now`). Retry clears
the claim and records only a categorical safe failure reason.

Investigation leases use the same exact-expiry convention: an unowned or expired
lease (`lease_expires_at <= now`) can be acquired with a new UUIDv7 fencing token;
renewal and completion require the current token and `lease_expires_at > now`.
Every successful acquisition or renewal advances the renewal sequence, and an
expired predecessor cannot update state after a successor acquires the lease.

## Contention guidance

No global counters are updated per record. Work batches are bounded. Family rows
may be hot during an incident; counters SHOULD use single-statement conditional
updates and be verified under concurrent load. Exact occurrence truth remains in
`records_seen`/`occurrences`, permitting aggregate repair.

## Regional topology

The supported production topology is explicitly `single-region`: each physical
region has its own analysis deployment and its own CockroachDB database. The
database may either have no CockroachDB regional metadata, as in the portable
self-hosted topology, or be assigned exactly one CockroachDB database region, as
CockroachDB Cloud does for a single-region managed cluster. In the managed form,
the one region MUST be the primary region, there MUST be no secondary region,
and the survival goal MUST be `ZONE`. Deployment configuration is responsible
for placing that database in the same physical cloud region as the analysis
service; provider-specific region names are not inferred from log claims.

Startup migration and Store construction require the explicit single-region
mode. Migration reads the live database metadata before making a schema change
and refuses zero/one-region metadata inconsistencies, two or more database
regions, a secondary region, or a region-survival goal. `region` remains in
relational keys and every ordinary Store is bound to one region and tenant as
defense in depth.

Regional-by-row placement and any database shared across physical regions are
unsupported. Supporting either requires another reviewed migration and
threat-model update. Catalog verification keeps separate closed checksum sets
for the portable and one-region-managed table localities and pins the full
reviewed `SHOW CREATE` definitions, including columns, nullability, defaults,
checks, indexes, foreign keys, and locality, in addition to the migration ledger
checksum. Drift, an unknown CockroachDB catalog shape, or an unknown ledger
version refuses startup.

CockroachDB Cloud may protect managed tables with `schema_locked=true`.
Migration metadata names the existing tables each version must change. A pending
migration checks those named tables, temporarily unlocks only locks that were
already present, runs the reviewed migration transaction, and restores every
original lock before it reports success or failure. Failure to restore a lock is
a startup failure. Already-applied migrations do not toggle locks.
