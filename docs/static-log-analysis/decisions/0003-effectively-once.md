# ADR 0003: Effectively-Once Processing

Status: accepted

## Decision

Accept at-least-once and out-of-order delivery. Ensure each stable `record_id`
contributes to rule state and incident evidence at most once. Use separate unique
keys for records, incident generations, and investigations.

## Rationale

Acknowledgements can be lost, Collector batches can replay, SQS Standard can
duplicate, and workers can race. Literal exactly-once transport is not a credible
end-to-end guarantee across these boundaries. Idempotent state transitions provide
the required business behavior.

## Consequences

Source adapters require versioned identity rules. CockroachDB uniqueness and
transaction retry behavior are correctness-critical. Duplicate attempts must be
measured even when successfully suppressed.
