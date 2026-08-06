# TDD Implementation Plan

## Development rule

Production behavior begins with a failing test at the narrowest credible boundary.
Implement only enough behavior to pass, refactor while green, then add boundary,
property, concurrency, and failure coverage proportional to risk.

Tests assert observable state and invariants rather than private call order. Real
Pebble and CockroachDB are used for storage contracts; mocks do not substitute for
durability or concurrency behavior.

## Milestone 0: test harness

Create fake clock, valid record builders, golden fixture loader, deterministic ID
source, capturing publisher, temporary Pebble factory, CockroachDB test container,
and deterministic OTLP producer. Harness code itself receives focused tests where
incorrect behavior could mask production failures.

## Milestone 1: deterministic domain

Write these failing tests in order:

1. Normalize a representative OTLP payment error.
2. Reject a service identity conflicting with the trusted envelope.
3. Preserve one `record_id` across a producer retry with a lost acknowledgement.
4. Make dynamic request/customer values produce the same fingerprint.
5. Make different exception/application frames produce different fingerprints.
6. Count a replayed `record_id` once.
7. Four ordinary errors do not request an investigation; the fifth does.
8. The sixth through hundredth errors update one incident without another agent.
9. A deployment change creates a linked generation and updates context.
10. Exact half-open window boundary behavior.
11. Quiet at 15 minutes and same/new generation across the two-hour boundary.

## Milestone 2: security and admission

1. Redaction is idempotent.
2. Known prohibited values never appear in safe output.
3. Unsafe unparseable content becomes withheld metadata.
4. Uncertain protection classification is protected.
5. Explicitly unprotected DEBUG/INFO may shed at the configured threshold.
6. Protected input receives backpressure rather than false acknowledgement.
7. Compressed, uncompressed, record-count, depth, and record-size limits hold.

## Milestone 3: journal

Run the complete reusable journal contract: synchronized atomic append, crash and
reopen, claim expiry, duplicate identity, commit gap, quarantine, compaction,
capacity, incompatible version, and corruption refusal. Include a process-level
test that terminates immediately after acknowledgement and verifies replay.

## Milestone 4: CockroachDB

Use real CockroachDB for concurrent `record_id`, family generation, investigation,
lease, and outbox races. Inject serialization conflicts and prove retry callbacks
are deterministic. Assert incident plus outbox atomicity in both failure directions.

## Milestone 5: vertical slice

```text
OTLP payment errors
-> trusted-envelope validation
-> redaction
-> durable journal acknowledgement
-> normalization/fingerprint
-> five-in-five rule
-> one CockroachDB incident
-> one outbox assignment
-> one claimable investigation context
```

The vertical slice is complete only when duplicate delivery before and after every
durability boundary produces one logical occurrence contribution and one active
investigation.

## Milestone 6: resilience and sources

Add Collector persistent queue, CloudWatch overlapping checkpoint replay, SQS
Standard/DLQ, enrichment recovery, 30-minute CockroachDB outage, full journal
behavior, replica drain, and selected OpenTelemetry demo Flagd scenarios.

## CI gates

- Fast unit/fixture/property tests on every change.
- Integration tests with race detector for storage and concurrency changes.
- Schema generation and compatibility checks.
- Migration apply and compatibility checks.
- Redaction regression corpus.
- End-to-end vertical slice on pull requests affecting its path.
- Nightly fuzzing, demo faults, restarts, capacity, and 30-minute outage.

Line coverage is diagnostic, not the definition of quality. Every state transition,
retry boundary, unique constraint, rule boundary, classification, source replay,
and original completion requirement MUST have named behavioral coverage.

## Definition of done

A behavior is done when its requirement and schema are current, a test demonstrated
the prior failure, implementation passes at the appropriate real boundary, failure
and concurrency cases pass, security and telemetry effects are verified, and no
skipped or weakened test hides unfinished work.
