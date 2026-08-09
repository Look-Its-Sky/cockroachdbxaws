# Versioned API and Data Schemas

The implementation MUST define Protobuf schemas for OTLP-facing/internal durable
messages and JSON Schema for operator-authored YAML and agent-facing JSON. Generated
Go types are used at boundaries; untyped maps do not cross into domain logic.

## Required schema artifacts

Before the corresponding feature is implemented, the repository will contain:

```text
api/proto/internal/v1/normalized_log.proto
api/proto/internal/v1/incident.proto
api/schema/rules/v1/rule.schema.json
api/schema/redaction/v1/policy.schema.json
api/schema/agent/v1/assignment.schema.json
api/schema/agent/v1/context.schema.json
api/schema/agent/v1/progress.schema.json
api/schema/agent/v1/report.schema.json
```

## Common envelope

All durable or external messages contain:

```text
schema_version
message_id
created_at
region
tenant_id
classification
producer
correlation_id
```

Required strings reject empty or whitespace-only values. Timestamps use RFC 3339
UTC in JSON and Protobuf timestamps internally. IDs use canonical lowercase UUID
text at JSON boundaries.

The journal persists `DurableNormalizedLog`, whose explicit common envelope wraps
the nested `NormalizedLog` payload. `message_id` is a deterministic canonical UUIDv5
derived from `record_id` (unlike producer/queue UUIDv7 IDs, it names immutable
durable record content rather than creation order); `created_at` is the canonical
first-attempt trusted
`source.received_at`; `tenant_id` and maximum `classification` come from immutable
single-tenant journal configuration; `producer` is the trusted source instance;
and `correlation_id` is the normalized trace ID when present. Decode verifies these
fields against the nested payload and current journal boundary. The checked-in Go
binding is regenerated from the schema with:

```text
protoc -I . -I /usr/include --go_out=. \
  --go_opt=module=github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis \
  api/proto/internal/v1/normalized_log.proto
```

## Normalized log required fields

`schema_version`, `record_id`, `record_id_version`, `identity_quality`, `batch_id`,
trusted source envelope, event and observed time, timestamp inference status,
severity number/class, safe body or event name, service/environment status,
redaction policy version, and region are required.

Optional structures include deployment, exception, trace/span correlation,
attributes, scope, and regional raw reference. Every optional enrichment field
distinguishes `not_available`, `not_applicable`, `pending`, and `available` where
absence would otherwise be ambiguous.
A present regional raw reference uses a known `otlp` or `cloudwatch` source type,
one of the closed classification values `PUBLIC`, `INTERNAL`, `SENSITIVE`, or
`RESTRICTED`, and a complete ordered `from <= to < expires_at` UTC range.

## Rule schema invariants

A rule requires ID, version, enabled state, scope, typed match/condition, grouping,
window semantics when stateful, protected-input declaration, event severity,
quiet/reopen policy, and fixtures. Rate rules require denominator and minimum
volume. Regex sizes and input fields are bounded. Unknown fields are errors.

## Agent schemas

Assignment messages contain pointers and scheduling metadata only. Context is an
immutable versioned manifest containing trigger, service/deployment, occurrence
summary, selected evidence, relationships, enrichment status, allowed tools, and
limits. Progress is append-only by sequence. Reports require supporting and
contradicting evidence for any probable or confirmed root cause.

Log-derived strings in agent schemas are typed as untrusted evidence and rendered
separately from orchestrator instructions.

## Compatibility tests

Every schema has valid/invalid golden examples, unknown-field behavior tests,
round-trip tests, previous-minor-version reader tests, deterministic serialization
where identity depends on bytes, and maximum-size tests. Generated artifacts are
checked for clean regeneration in CI.

For durable Protobuf events, readers accept unknown fields when the declared major
version matches. The journal retains the original durable envelope bytes rather
than decoding and rewriting them, so an older reader does not silently discard
unknown identity-relevant bytes. Operator-authored configuration remains strict and
rejects unknown fields. Unknown majors are always refused.
