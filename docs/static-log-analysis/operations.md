# Failure Handling and Operations

## Capacity and shedding

Journal capacity is calculated per region:

```text
measured peak serialized bytes/second * 3600 seconds * safety factor
```

Use at least a 2x safety factor until production distributions are known.

Default utilization behavior:

| Utilization | Behavior |
|---|---|
| Below 70% | Normal |
| 70-85% | Warn and accelerate processing |
| 85-95% | Sample eligible repeated DEBUG/INFO records |
| Above 95% | Reject optional records and backpressure mandatory traffic |

ERROR, FATAL, qualifying security, and records conservatively classified as inputs
to an active rule are protected. The protection classifier is compiled with the
rules and runs before shedding without mutating rule state. Uncertain means
protected. Initially, only explicitly configured, unprotected DEBUG/INFO sources
may shed. Dropped records are counted by service, severity, rule dependency, and
reason. Per-service quotas prevent one noisy service consuming the entire regional
journal.

If mandatory data cannot be persisted, ingestion returns a retryable failure and
alerts. It never acknowledges a silently dropped mandatory record.

## Failure matrix

| Failure | Required behavior |
|---|---|
| Collector restart | Persistent sending queue replays |
| Analysis endpoint unavailable | Collector retries with backoff and jitter |
| Analysis restart | Journal reopens and unfinished records replay |
| CockroachDB unavailable | Ingestion continues; journal retains work for at least 30 minutes |
| Serializable transaction conflict | Retry whole idempotent transaction |
| SQS unavailable | Committed outbox messages remain pending |
| Publish acknowledged but DB update fails | Outbox republishes; orchestrator deduplicates |
| Metadata provider unavailable | Process with available fields and enrich later |
| Rule reload invalid | Previous immutable ruleset remains active |
| Journal near full | Priority shedding and backpressure policy applies (see runtime.md) |
| Journal corruption | Stop unsafe partition, alert, preserve files, follow recovery runbook |
| Clock skew | Prefer source event time within validation bounds; expose skew metrics |
| Agent dies | Lease expires and same investigation is reassigned |
| Entire region unavailable | No cross-region log movement; recover region-local services |

## Shutdown

On graceful shutdown, stop accepting new requests, drain admitted handlers through
journal commit, stop claiming new work, finish or checkpoint active transactions,
and close the journal cleanly. A forced stop remains safe because acknowledged
records are already synchronized.

## Required telemetry

Metrics include ingestion records and bytes, rejection and partial-success counts,
redaction matches and failures, journal records/bytes/oldest age, append latency,
replay and corruption counts, rule evaluations and matches, created/deduplicated
incidents, outbox depth and failures, agent assignments and suppressions, and
enrichment availability. `static_log_analysis_derived_identity_total` counts
OTLP normalization attempts that lacked a native record UID; sustained growth is
a producer-migration signal, not a reason to drop otherwise usable logs.

Permanent record-local drops are exported as separate unlabelled counters under
`static_log_analysis_rejected_*_total`. Invalid records use the closed safe
subreasons `missing_timestamps`, `missing_content`, `prohibited_content`,
`structural_validation`, and `other`; record content never appears in a metric
name or label.

Do not use trace IDs, request IDs, incident IDs, messages, or fingerprints as
metric labels.

The counters that exist today are served as Prometheus text on the admin
listener's `/metrics`, alongside `/healthz` and `/readyz`. See runtime.md. They
are unlabelled by construction. Ingestion byte counts, append latency, rule
evaluation counts, and outbox depth are not yet exported.

## Initial SLO targets

These are design targets and require production validation:

- 99.9% successful ingestion for valid records when regional infrastructure is
  available.
- 99% of ordinary qualifying incidents detected within 60 seconds after the end
  of their rule window.
- Critical exact-match incidents persisted within 10 seconds at normal load.
- Zero acknowledged mandatory-record loss during a 30-minute CockroachDB outage
  when the persistent volume remains healthy and sized capacity is not exceeded.
- Fewer than one duplicate active investigation per 10,000 assignments; every
  duplicate attempt is suppressed and observable.
- 100% of persisted records pass the configured prohibited-content validation
  stage, and the maintained regression corpus has zero known secret escapes.

## Operational runbooks required before production

Written in [runbooks.md](runbooks.md). Three of the twelve document a lever
that does not exist yet and say so explicitly rather than describing a procedure
an operator cannot follow.

- Journal capacity exhaustion and recovery.
- Journal corruption and read-only preservation.
- Collector backlog and failed export.
- CockroachDB outage and replay verification.
- Outbox/SQS backlog and dead-letter replay.
- Disable, shadow, and roll back a noisy rule.
- Suppress or reopen an incident with audit history.
- Reassign a stuck investigation.
- Rotate redaction HMAC keys.
- Diagnose missing service/deployment metadata.
- Upgrade and roll back journal format.
- Verify regional data boundaries.
