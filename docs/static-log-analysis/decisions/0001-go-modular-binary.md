# ADR 0001: Go and a Modular Single Binary

Status: accepted

## Decision

Implement the first release in Go 1.26 as one binary supporting `all`, `ingest`,
`process`, `outbox`, and combined `cloudwatch` roles. The source-only role is a
diagnostic surface, not a production topology. Use OTLP Protobuf, explicit
internal domain types, `pgx/v5`, versioned SQL migrations, and standard-library
HTTP where practical.

## Rationale

Go provides predictable resource use, strong concurrency support, mature OTLP and
AWS libraries, small artifacts, and native fuzzing. One binary minimizes early
operational boundaries while runnable roles preserve future independent scaling.

## Consequences

Package boundaries must be enforced. Do not add an ORM, scripting runtime, Redis,
or separate microservice without a new decision record.
