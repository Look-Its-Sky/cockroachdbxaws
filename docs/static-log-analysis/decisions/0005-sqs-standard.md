# ADR 0005: Amazon SQS Standard for Agent Assignments

Status: accepted

## Decision

Use one regional SQS Standard queue plus a dead-letter queue. Messages contain a
small immutable investigation pointer. CockroachDB leases and unique investigation
keys enforce one active agent.

## Rationale

Ordering is not required, and lifecycle deduplication lasts longer than broker
deduplication windows. SQS Standard is managed, regional, scalable, and naturally
matches the at-least-once architecture.

## Consequences

Duplicates and reordering are normal. Visibility timeout, redrive policy, regional
IAM, and lease behavior require explicit configuration and tests.
