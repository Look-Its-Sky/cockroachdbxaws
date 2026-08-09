# Static Log Analysis Service

Status: design baseline, approved for implementation planning  
Last updated: 2026-08-06

The Static Log Analysis Service is a deterministic log-processing component. It
normalizes deployed-service logs, evaluates versioned static rules, groups and
deduplicates failures, enriches incidents, and publishes investigation requests.
No LLM or agent participates in log detection.

Agents begin only after the service has created a structured incident. They use
approved read-only tools to investigate source, telemetry, deployments, changes,
and similar historical incidents.

## Goals

1. Receive logs from multiple services and sources.
2. Normalize them into a versioned canonical representation.
3. Redact sensitive information before persistent storage.
4. Evaluate deterministic rules, thresholds, counts, rates, and time windows.
5. Group related records and avoid duplicate incidents and agents.
6. Enrich incidents without making enrichment an ingestion dependency.
7. Preserve accepted records through short downstream outages.
8. Store incident state and publish investigation requests reliably.
9. Give agents concise initial context plus scoped access to more evidence.
10. Keep all log-derived data and processing within its source region.

## Non-goals

- LLM-based log classification or detection.
- Permanent storage of all raw logs in CockroachDB.
- Automated remediation or production mutation by investigating agents.
- Perfect exactly-once transport.
- Cross-region log replication.
- A general-purpose streaming platform in the first release.

## Documentation map

- [Architecture and data flow](architecture.md)
- [Source adapters](source-adapters.md)
- [Implementation stack](implementation-stack.md)
- [Implementation contracts](implementation/README.md)
- [Canonical data model](data-model.md)
- [Rules, grouping, and incident lifecycle](rules-and-incidents.md)
- [Agent assignment and context contract](agent-contract.md)
- [Security, redaction, and retention](security.md)
- [Failure handling and operations](operations.md)
- [Operational runbooks](runbooks.md)
- [Test strategy and acceptance criteria](testing.md)
- [Requirement acceptance evidence](acceptance.md)
- [Architecture decisions](decisions/README.md)
- [Terminology](glossary.md)

## Agreed baseline

- Go 1.26 and OTLP/Protobuf.
- One modular binary with independently runnable ingestion, processing, and
  outbox roles.
- At-least-once delivery with effectively-once record contribution.
- A short OpenTelemetry Collector persistent transport queue.
- An authoritative, redacted, disk-backed journal in the analysis service.
- The service acknowledges OTLP only after a synchronized journal commit.
- CockroachDB stores incidents, investigations, reports, audit records, and the
  transactional outbox.
- Amazon SQS Standard carries small investigation-assignment messages.
- One active investigation per incident family, enforced using CockroachDB
  uniqueness and renewable leases.
- Versioned YAML rules with typed rule families; CEL may later support safe,
  non-Turing-complete custom predicates.

## Implementation readiness

Normative implementation contracts are under [implementation/](implementation/README.md).
Where those contracts are more specific than an architectural overview, the
implementation contract governs. Changes to a normative contract require tests,
documentation updates, and an ADR when they alter an accepted architecture choice.

## Known measurement gaps

The local OpenTelemetry demo produced approximately four log records per second
during a short sample. This is not a production sizing input. Journal and queue
capacity must be recalculated from peak serialized OTLP and journal byte rates in
each target region. Production service count, peak rate, record-size distribution,
and source retention are still measurement-dependent.
