# CockroachDB × AWS Incident Investigation Platform

The platform's first component is the deterministic Static Log Analysis Service.

## Layout

```text
docs/        design documents and normative implementation contracts
services/    deployable components, one Go module each
```

- [Static Log Analysis Service documentation](docs/static-log-analysis/README.md)
- [Static Log Analysis Service code](services/static-log-analysis/)

## Status

Milestone 0 of the [TDD implementation
plan](docs/static-log-analysis/implementation/tdd-plan.md) is complete: the
[test harness](docs/static-log-analysis/implementation/test-harness.md) and the
domain types it needs exist and are tested, including against real Pebble and
real CockroachDB.

No ingestion, normalization, rule evaluation, incident, or agent behaviour is
implemented yet. The remaining documentation describes the intended system and
its contracts rather than what has been built.

## Running the tests

```text
cd services/static-log-analysis
go test ./...                                          # fast gate
REQUIRE_DOCKER=1 go test -race -tags=integration ./...  # storage and concurrency gate
```
