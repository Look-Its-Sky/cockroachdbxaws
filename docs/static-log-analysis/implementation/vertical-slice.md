# Milestone 5 Vertical Slice

## Production coordinator

`internal/pipeline.Service` owns the first complete runtime sequence:

```text
bounded OTLP decode
-> trusted-envelope reconciliation and redaction
-> record-local normalized-size rejection
-> synchronized Pebble append and acknowledgement
-> bounded journal claim
-> M1 observation and finalized persistence decision
-> fingerprint and deterministic M4 input
-> CockroachDB transaction
-> journal claim commit
```

Raw OTLP values are accepted only by admission and normalization. The journal,
rule engine, and persistence interfaces receive `model.NormalizedLog` values
which have passed the configured redaction policy. Ingestion reports categorical
record-local rejections and acknowledges accepted records only after the journal
append succeeds.

## Durable five-in-five election

Every finalized, grouping-eligible ordinary error carries a fully prepared
`InvestigationCandidate`. Candidate IDs, context, assignment bytes, and routing
attributes are fixed before the CockroachDB retry callback. A candidate does not
advance incident or generation context by itself.

Under the incident-family lock, M4 inserts the unique occurrence and evaluates
the affected candidate endpoints for that generation. The interval is exactly
half-open `(endpoint - 5 minutes, endpoint]`. The indexed query also considers a
late earlier arrival which completes an already-persisted later endpoint. On the
first qualifying five-record window, the same transaction creates the
investigation, v1 context, claim row, outbox assignment, and active-family
reference. Later or concurrent candidates observe the active investigation and
do not append a conflicting v1 context or another outbox row.

The supporting CockroachDB index is
`(region, tenant_id, incident_id, generation, event_time)` and stores the bounded
rule projection fields used by the election.

## Startup configuration agreement

Construction cross-validates the coordinator's scope, classification, redaction
policy version, and admission limits against both immutable boundaries it will
use: the journal directory manifest and the persistence `Store`'s boundary. A
contradiction is a configuration error which refuses startup. This is the only
place such a contradiction can be caught, because downstream every record would
instead look individually invalid, and isolating an individually invalid record
destroys its durable payload.

## Restart and retry behavior

The journal retains committed normalized records for at least the five-minute
window plus two-minute allowed-lateness horizon. On startup, the coordinator
uses bounded pages of `RecoverRetained` records to reconstruct the sealed M1
topology before it claims new work. Pending and claimed records warm topology and
remain eligible for normal journal processing; committed records warm topology
but are never re-persisted. Quarantined records have no retained payload and are
excluded by construction. Journal startup's full scrub still validates their
categorical quarantine metadata.

A retained record the rule engine cannot observe is skipped and counted, never
fatal. Such a record was admitted and durably acknowledged by design, so
refusing to start would make one of them permanently unstartable with no
operator recovery short of deleting the volume. Processing gives it a terminal
categorical outcome instead: it is quarantined as unsupported data, retaining
only safe metadata.

On startup the coordinator also re-adopts the unexpired journal claims it still
owns, so unfinished work resumes on the first cycle instead of waiting out a
claim TTL. While a cohort is being retried its claims are renewed before they
can lapse, so an outage longer than the claim TTL does not churn the same work.
A stale renewal is the journal's answer about one record, so renewal falls back
to one record at a time rather than forfeiting the cohort: a sibling dropped
locally is not returned to pending either, so forfeiting it would stall it for a
full claim TTL, cost it an extra attempt, and unpin its payload for that window.
The coordinator declares its in-flight record identities to the rule engine,
which exempts them from retention compaction; a record whose in-memory payload
were released could never receive a persistence decision and its claim would be
renewed forever.

CockroachDB unavailable errors retain the current journal claims for retry. A
database commit followed by a lost journal-mark response replays the same record
identities through M4, which returns duplicate results before the journal claims
are committed.

Only a record-local invalidity may ever quarantine, because quarantine deletes
the durable payload. Such failures are bisected to individual records, so a
poison record is quarantined without quarantining valid siblings, and one
quarantine is reported as an outcome rather than a batch error whatever cohort
size isolated it. Every other failure returns to the caller with claims intact:
a store configuration mismatch is a deployment fault which would otherwise
destroy every acknowledged record, and a generation identity or immutable-value
conflict is the exact evidence an operator must investigate. Record preparation
distinguishes the same two kinds: only a record which cannot be fingerprinted is
terminal, while a transient identifier or encoding failure keeps its claim.

Ingestion likewise separates permanent from retryable. An envelope addressed to
another region, and any batch the journal permanently refuses, are rejected and
counted rather than reported as journal unavailability, so a transport never
replays a poisoned batch forever and starves its valid siblings.

## Deliberate Milestone 6 exclusions

This milestone does not add Collector persistent queues, CloudWatch adapters,
SQS publishing, DLQ behavior, enrichment providers, replica draining, or the
extended outage/resilience scenarios. It creates and validates the transactional
outbox assignment but leaves publication to the existing outbox boundary.
