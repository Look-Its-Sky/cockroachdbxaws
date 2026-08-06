# Working in this repository

## Layout

```text
docs/        design documents and normative implementation contracts
services/    deployable components, one Go module each
```

The only component so far is `services/static-log-analysis/`. Run Go commands
from inside that directory, or with `go -C services/static-log-analysis`.

## Progress

The plan is
[docs/static-log-analysis/implementation/tdd-plan.md](docs/static-log-analysis/implementation/tdd-plan.md).
Update this section when a milestone or a numbered behaviour lands.

| Milestone | State |
|---|---|
| 0 — test harness | Complete |
| 1 — deterministic domain | In progress: behaviours 1 and 2 of 11 |
| 2 — security and admission | Not started |
| 3 — journal | Not started |
| 4 — CockroachDB | Not started |
| 5 — vertical slice | Not started |
| 6 — resilience and sources | Not started |

Milestone 1 behaviours, in the plan's order:

1. Normalize a representative OTLP payment error — done
2. Reject a service identity conflicting with the trusted envelope — done
3. Preserve one `record_id` across a producer retry with a lost acknowledgement — done
4. Dynamic request/customer values produce the same fingerprint — not started
5. Different exception/application frames produce different fingerprints — not started
6. Count a replayed `record_id` once — not started
7. Four ordinary errors do not request an investigation; the fifth does — not started
8. The sixth through hundredth errors update one incident — not started
9. A deployment change creates a linked generation — not started
10. Exact half-open window boundary behavior — not started
11. Quiet at 15 minutes and same/new generation across two hours — not started

What exists in code: the test harness, the domain model, and the ingestion path
from an OTLP request to a validated `NormalizedLog`. There is no journal, rule
engine, incident store, outbox, HTTP or gRPC server, and no CockroachDB schema.

## How the work is done

Production behaviour begins with a failing test at the narrowest credible
boundary. Implement only enough to pass, then add boundary, failure, and
concurrency coverage in proportion to risk. Tests assert observable state and
invariants, not private call order. Real Pebble and real CockroachDB are used
for storage contracts; mocks do not substitute for durability or concurrency.

Documentation is part of a milestone's definition of done, not a follow-up.

Precedence when documents disagree, highest first:

1. Security and regional-boundary requirements
2. The normative implementation contracts under `docs/.../implementation/`
3. Architecture Decision Records
4. Architectural overview documents

A conflict is resolved in documentation and tests before implementation merges.

## Gates

```text
go test ./...                                           # fast: unit, fixture, property
REQUIRE_DOCKER=1 go test -race -tags=integration ./...   # storage and concurrency
gofmt -l . && go vet ./... && go vet -tags=integration ./...
```

Tests needing Docker sit behind the `integration` build tag. `REQUIRE_DOCKER=1`
turns an unavailable container into a failure rather than a skip, because a
skipped storage test in CI is the same as no storage test.

`UPDATE_GOLDEN=1 go test ./...` rewrites golden fixtures. Review the diff. CI
never sets it and checks the tree is clean afterwards.

## Conventions worth knowing before editing

Domain packages do not import Pebble, SQS, or a database client. Adapters
implement narrow interfaces owned by the consuming package.

Nothing reads wall-clock time or entropy directly: take a `clock.Clock` and an
`ids.Source`. Identifiers are generated before a retryable database transaction
begins and reused on every retry.

`NormalizedLog` is defined as already safe, so redaction runs before
normalization rather than after. Do not build one from raw content and clean it
up afterwards.

`SafeValue` names where a value belongs in the pipeline; it is not a guarantee
the type enforces. Persistence must still require redaction policy metadata and
run prohibited-content validation.

An identity or schema version is immutable. Change how something is derived by
adding a version, never by editing one.
