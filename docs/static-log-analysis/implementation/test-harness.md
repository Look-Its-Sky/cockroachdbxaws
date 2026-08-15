# Test Harness

The harness is the set of components every later test is written against. It
exists so that a test states a scenario rather than assembling one, and so that
the properties the service depends on are observable rather than assumed.

Harness code lives in `services/static-log-analysis/internal/testsupport/` and
is itself tested. A helper whose job is to fail on a missing fixture, an invalid
record, or an unavailable database is proven to fail; otherwise it could mask
every failure downstream of it.

## Components

| Component | Package | Purpose |
|---|---|---|
| Fake clock | `testsupport/fakeclock` | Time moves only when a test advances it |
| Deterministic IDs | `testsupport/testids` | Reproducible, sortable UUIDv7 identifiers |
| Record builders | `testsupport/builders` | Normalized records that are valid by default |
| Golden fixtures | `testsupport/golden` | Committed expectations with reviewable diffs |
| Capturing publisher | `testsupport/capture` | Records published messages and injects failures |
| Pebble factory | `testsupport/pebbletest` | Real Pebble, on disk or crash-simulating |
| CockroachDB container | `testsupport/crdbtest` | Real CockroachDB, isolated database per test |
| OTLP producer | `testsupport/otlpgen` | Deterministic OTLP payloads including hostile ones |
| Test boundary | `testsupport/tb` | The `testing.TB` subset harness code fails through |

Supporting production packages introduced alongside them: `internal/model`
(domain types and structural validation), `internal/clock`, `internal/ids`, and
`internal/queue`.

## Properties the harness guarantees

**Determinism.** Two factories, producers, or identifier sources configured the
same way produce identical output. Nothing reads wall-clock time or entropy
unless a test asks it to. Without this, a duplicate-delivery test cannot tell a
genuine difference from generator noise.

**Exact instants.** A fake clock fires a timer at its deadline and not a
nanosecond earlier, and the OTLP producer can place a record exactly on a
boundary or either side of it. Window intervals are half-open, so approximate
timing would let an off-by-one boundary error pass.

**Validity by construction.** Every record a builder returns is validated before
it is returned. A malformed record can be built deliberately with
`AllowInvalid()`, and a scenario that asks for one and receives a valid record
fails, so a stale escape hatch cannot quietly stop testing anything.

**Real storage.** Durability and concurrency are properties of Pebble and
CockroachDB, not of the code around them. `pebbletest.NewCrashable` discards
exactly the writes a power loss would discard, so a synced commit can be told
apart from an unsynced one. `crdbtest` produces genuine serialization conflicts
and unique constraint violations.

**Observable failure.** The capturing publisher distinguishes a publish that
never reached the queue from one that delivered and then lost its
acknowledgement, because the second is why a consumer sees a message twice. A
test that injects a failure with a bad count or an unknown mode panics rather
than silently injecting nothing.

**Specific failures, not any failure.** The CockroachDB conflict test asserts
SQLSTATE `40001` rather than a non-nil error, so a closed connection or a
mistyped statement cannot stand in for a serialization conflict. The container
image is pinned by digest, so the database version changes only in a reviewed
commit.

## Running tests

```text
go test ./...                                        # fast gate
REQUIRE_DOCKER=1 go test -race -tags=integration ./... # storage and concurrency gate
UPDATE_GOLDEN=1 go test ./...                        # rewrite fixtures, then review the diff
```

Tests requiring Docker are behind the `integration` build tag. `REQUIRE_DOCKER=1`
turns an unavailable container into a failure rather than a skip, because a
skipped storage test in continuous integration is the same as no storage test.
`CRDB_TEST=off` skips them without attempting to start anything, and
`CRDB_TEST_IMAGE` overrides the pinned CockroachDB image.

Continuous integration never sets `UPDATE_GOLDEN` and checks that the tree is
clean after the test run, so a rewritten fixture cannot reach a merge
unreviewed.

## Deliberate limits

The builders produce records that are already normalized, so they are the input
to rule, incident, journal, and persistence tests. Admission, normalization, and
identity derivation start from OTLP instead.

A builder's `record_id` is an opaque synthetic value, not the real SHA-256
derivation. Identity derivation is a behaviour with its own tests, and a builder
that reimplemented it would make those tests agree with a copy of themselves.

The JSON encoding of domain types is a readable representation for fixtures,
diagnostics, and agent-facing payloads. The durable internal format is Protobuf
and is defined with the schema artifacts. Because it is agent-facing, its
decoder is strict: a value carrying a kind with no payload is refused rather
than decoded to a zero, so an intentionally encoded zero is never confused with
a truncated one.

`SafeValue` names where in the pipeline a value belongs; it is not a property
the type enforces. Its fields are exported and its constructors accept any
string. Redaction is enforced by the redaction stage, and persistence must still
require redaction policy metadata and run final prohibited-content validation.

`Message.Validate` checks structure, the identifier shape, and the queue size
limit. It does not yet check that a message type names a known payload schema;
that arrives with the agent contract schemas and their message catalogue.
