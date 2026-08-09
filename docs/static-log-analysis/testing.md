# Test Strategy and Acceptance Criteria

## Test layers

### Unit and fixture tests

- Table-driven normalization for every supported source and language format.
- Golden redaction fixtures, including nested structured bodies.
- Rule boundary values, minimum denominators, consecutive counts, and composites.
- Versioned fingerprint fixtures.
- Fake-clock tests for windows, quiet periods, lateness, and reopen behavior.
- Incident family and generation grouping.
- Evidence selection diversity and deterministic ordering.
- Go fuzzing for OTLP parsing, bodies, attributes, regex configuration, and IDs.
- Exact half-open window boundaries and watermark monotonicity.
- Trusted-envelope conflicts and forged service/region claims.

### Component tests

- Real journal in a temporary persistent directory.
- Synchronized append, crash, reopen, replay, checkpoint, and compaction.
- Duplicate batch and record delivery.
- Real CockroachDB transactions and serialization retries.
- Concurrent incident creation and investigation lease races.
- Outbox success, retry, duplicate publish, and permanent failure.
- Enrichment timeout, cached result, late update, and context versioning.

### End-to-end tests

Use the OpenTelemetry demo, Collector persistent queue, analysis service,
CockroachDB, deterministic OTLP generator, and an SQS-compatible test endpoint.

The demo's Flagd scenarios exercise ad failure/high CPU, cart failure percentages,
payment failure/unavailability, product-catalog targeted failure, recommendation
cache failure, email memory leak, readiness failure, and latency scenarios.

The deterministic generator is still required for exact duplicates, late and
out-of-order records, exact threshold boundaries, controlled stack traces,
malformed records, secret corpus, multi-container repetition, shared traces,
oversized payloads, and replay sequences.

## Required resilience scenarios

1. Stop CockroachDB for at least 30 minutes while ingesting protected records.
2. Verify ingestion remains successful until provisioned capacity is reached.
3. Restore CockroachDB and verify every record contributes exactly once.
4. Lose the OTLP acknowledgement and verify Collector retry is harmless.
5. Restart Collector and analysis containers with queues populated.
6. Fail SQS after incident commit and verify outbox recovery.
7. Deliver an SQS assignment repeatedly and out of order.
8. Race multiple orchestrators for one investigation lease.
9. Fill the journal and verify priority shedding, counters, and backpressure.
10. Make enrichment providers fail and recover during an active investigation.
11. Route Collector batches across separate replica-owned journals and drain one
    replica without losing acknowledged work.

## Security scenarios

Fixtures include JWTs, authorization headers, cookies, AWS-style keys, database
URLs, private keys, passwords, emails, payment fields, query strings, multiline
stack traces, Unicode disguises, nested JSON, and prompt-injection text.

Tests assert prohibited originals do not occur in Collector queue inspection,
journal files, CockroachDB, application logs, agent context, or reports.

## Requirement acceptance

The service is complete when automated evidence demonstrates that it can:

1. Receive or retrieve logs from multiple deployed services.
2. Normalize them into the versioned canonical form.
3. Apply deterministic rules and thresholds.
4. Group and deduplicate related records.
5. Enrich incidents without blocking detection.
6. Store incidents and safe evidence references.
7. Reliably publish investigation requests through the outbox.
8. Prevent duplicate or excessive agent execution.
9. Survive the required downstream outage without acknowledged mandatory loss.
10. Enforce redaction, authorization, retention, and regional boundaries.

Every acceptance item must link to named automated tests and retained CI results
before production approval. Those links are maintained in
[acceptance.md](acceptance.md), which records the current status of each item
and names the gate every cited test runs in.
