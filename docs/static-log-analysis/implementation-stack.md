# Implementation Stack

## Selected technology

| Concern | Selection |
|---|---|
| Language | Go 1.26 |
| Primary protocol | OTLP/gRPC |
| Secondary protocol | OTLP/HTTP with Protobuf |
| Transport types | Official OTLP Protobuf Go packages |
| Administrative HTTP | Go `net/http` |
| Local journal | Pebble behind an internal interface |
| Database | CockroachDB |
| SQL driver | `pgx/v5` |
| Transaction retry | `cockroach-go` `crdbpgxv5` helper |
| Rule files | Strict, versioned YAML |
| Custom predicates | CEL after typed rules are established |
| Agent queue | Amazon SQS Standard with DLQ |
| AWS client | AWS SDK for Go v2 |
| Service telemetry | OpenTelemetry Go SDK and Prometheus metrics endpoint |
| IDs | UUIDv7-compatible sortable identifiers |
| Packaging | Multi-stage Docker image, non-root runtime |

Dependencies are pinned through `go.mod`, reviewed for license and maintenance,
and updated through tested pull requests. Persistent-format and protocol changes
require fixture-based compatibility tests.

## Proposed package boundaries

```text
cmd/log-analysis/
internal/
  ingest/          OTLP handlers and admission control
  redact/          universal validation and service policy
  journal/         durability interface and Pebble implementation
  normalize/       OTLP/source conversion
  identity/        record and fingerprint versions
  rules/           compiled rules and bounded window state
  incidents/       grouping and lifecycle
  enrich/          cached and asynchronous metadata
  persistence/     explicit CockroachDB SQL
  outbox/          claim, publish, and retry
  queue/           SQS interface and implementation
  orchestration/   investigation leases and context versions
  telemetry/       metrics, traces, and safe internal logging
  clock/           injectable event and wall clocks
  model/           domain types without storage dependencies
migrations/
configs/rules/
testdata/
```

Domain packages do not import Pebble, SQS, or concrete database clients. Adapters
implement narrow interfaces owned by the consuming package.

Direct OTLP instrumentation creates `log.record.uid` before its first export
attempt. Collection receivers for files or cloud APIs may derive a UID from native
event identity or source position. A custom Collector processor may normalize or
validate these IDs, but MUST NOT generate a fresh random ID after an upstream retry
boundary. Unsupported producers use the documented derived-identity fallback.

## Persistence guidance

Use explicit SQL and versioned migrations rather than an ORM. CockroachDB
transactions that modify incident state use client-side retry handling and remain
idempotent. Keep transactions small, avoid hot global counters, and use unique
constraints for record, generation, outbox, and investigation correctness.

The journal uses synchronized atomic batches for accepted records. Its on-disk
schema has an independent version and migration/rollback test suite. The process
must refuse unsafe downgrade or unknown future formats.

## Rule implementation

Typed Go rule evaluators are implemented first. Regular expressions use Go's
RE2-based engine and enforce pattern and input-size limits. CEL, when added, is
compiled and type-checked during rule loading; programs may only evaluate a
stateless, explicitly exposed safe view of a normalized record.

## Configuration

Environment variables configure endpoints, credentials by reference, role,
region, capacity, and deployment settings. Strict YAML configures rules and
redaction policies. Unknown fields, duplicate IDs, unsupported versions, invalid
durations, unbounded regexes, and missing protected-input declarations fail
validation before activation.

Secrets enter through the regional platform secret facility and are never placed
in rule files, images, command-line arguments, logs, or agent packages.

## Deliberately excluded initially

- Kafka or Kinesis as the core log stream.
- Redis for rule state.
- A full ORM.
- JavaScript, Lua, or arbitrary rule scripts.
- OpenSearch as a correctness dependency.
- Separate deployable service for each pipeline stage.
- Custom JSON as the primary ingestion protocol.
