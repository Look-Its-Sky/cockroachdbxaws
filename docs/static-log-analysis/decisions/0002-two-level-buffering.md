# ADR 0002: Two-Level Durable Buffering

Status: accepted

## Decision

Use a short OpenTelemetry Collector persistent sending queue and an authoritative
disk-backed journal inside the analysis service. Redact before either persistent
write. Acknowledge OTLP only after a synchronized analysis-journal commit.

Use Pebble behind a narrow internal journal interface for the initial Go
implementation. Pin and upgrade-test its version; no domain package may import it
directly.

Each ingestion replica owns a distinct journal and persistent volume. Journals are
never shared between processes. Global deduplication remains in CockroachDB.

## Rationale

The Collector queue covers endpoint and network failure. The analysis journal
covers worker and downstream failure after acceptance. Either queue alone leaves
an avoidable durability gap.

## Consequences

Operators must monitor and capacity-plan both layers. A local persistent volume
survives process/container failure but does not guarantee survival of volume or
region loss. Replicated streaming is a later decision if that guarantee changes.
