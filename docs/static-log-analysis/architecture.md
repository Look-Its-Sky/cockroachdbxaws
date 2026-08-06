# Architecture and Data Flow

## Components

```text
Applications
  -> regional OpenTelemetry Collector
       -> universal redaction
       -> short persistent sending queue
       -> OTLP/gRPC or OTLP/HTTP
  -> Static Log Analysis Service
       -> validation and service-specific redaction
       -> authoritative local journal
       -> normalization and deterministic rules
       -> grouping, deduplication, and enrichment
  -> CockroachDB
       -> incident and investigation state
       -> selected redacted evidence and raw-log references
       -> transactional outbox
  -> regional Amazon SQS Standard queue
  -> agent orchestrator
  -> isolated service-specific agent workspace
```

CloudWatch is a later source adapter. It pulls using durable per-log-stream
checkpoints and feeds the same ingestion contract. Source adapters must not embed
rule or incident behavior.

## Delivery contract

Transport is at least once. Processing is effectively once:

> A source record may be received or evaluated more than once, but its stable
> record ID contributes to rule state, occurrence counts, and incident evidence
> at most once.

There are three separate idempotency boundaries:

- `record_id` prevents duplicate record contribution.
- `incident_key + generation` prevents duplicate incident generations.
- `investigation_id` and its lease prevent duplicate active agents.

Literal exactly-once delivery is not required.

## Acknowledgement boundary

The ingestion service returns OTLP success only after it has:

1. Validated the envelope sufficiently to process it.
2. Applied required redaction.
3. Assigned batch and record identities.
4. Committed the safe records to its journal with a synchronized disk write.

Before step 4, a compiled admission classifier determines whether a record is
protected by any active rule. An uncertain classification is protected. This
classifier does not trigger incidents; it exists only to make load shedding safe.

CockroachDB and SQS are not in the ingestion request path. A record held only in
memory must never be acknowledged.

Invalid individual records are represented through OTLP partial success when
possible. Permanently malformed records must not cause an otherwise valid batch
to retry forever.

## Two durability layers

The Collector persistent queue protects the network hop and analysis endpoint.
It is sized for about 5-10 minutes of peak traffic.

The analysis journal protects already accepted work while workers, CockroachDB,
metadata providers, or SQS are unavailable. It is sized for at least 60 minutes
of measured peak input, exceeding the required 30-minute outage tolerance.

The queues have different completion conditions:

- Collector entry completes after the analysis service durably accepts it.
- Journal entry completes after deterministic processing and the corresponding
  CockroachDB transaction commit.

## Runtime shape

The first release is one Go executable with roles:

```text
log-analysis all
log-analysis ingest
log-analysis process
log-analysis outbox
```

Local deployments run `all`. Production may scale roles separately using the
same artifact. Internal packages must preserve boundaries between ingestion,
redaction, journal, normalization, rules, incidents, enrichment, persistence,
outbox, queue, and telemetry.

Each ingestion replica owns one Pebble journal and one non-shared persistent
volume. No two processes open the same journal. The Collector may balance batches
across healthy replicas; global deduplication occurs in CockroachDB. Scale-down
drains a replica before its volume is detached or deleted.

## Event time

Rules use event time where a valid source timestamp exists. The default allowed
lateness is two minutes and may be overridden per rule. Observed time is retained
separately. A missing event timestamp uses observed time and sets
`timestamp_inferred=true`.

Late records may enrich an open or recently closed incident. Very late records
do not retroactively launch another agent for an incident already investigated.

## Enrichment

Detection and persistence never wait for external metadata. Cached enrichment is
used immediately. Normal-priority investigations receive an initial enrichment
budget of at most ten seconds; critical security and FATAL incidents dispatch
immediately.

Enrichment continues asynchronously. Material changes create a new version of
the investigation context and notify the active agent without restarting it.
Unavailable providers are represented explicitly with reason and retryability.

## Regional boundary

Each region has its own Collector queue, analysis journal, service deployment,
SQS queue, secret material, and approved telemetry access. CockroachDB tables
containing log-derived content must use region-appropriate locality. Logs,
evidence, correlatable identifiers, and agent workspaces do not cross regions.
Only explicitly approved non-sensitive aggregate metrics may be global.
