# ADR 0004: CockroachDB Transactional Outbox

Status: accepted

## Decision

Commit incident changes, investigation creation, and an outbox row in one
CockroachDB transaction. A separate role publishes pending rows and records
successful delivery. It may publish duplicates.

## Rationale

Direct database-then-queue or queue-then-database publication has an atomicity gap.
The outbox ensures a committed investigation remains publishable through queue
outages and process crashes.

## Consequences

The publisher requires retry, backoff, metrics, retention, and failure handling.
Consumers remain idempotent because successful SQS publication followed by a
failed status update causes republishing.
