# Working in this repository

## Layout

```text
docs/        design documents and normative implementation contracts
services/    deployable Go analysis service and Next.js operations dashboard
```

Run Go commands from `services/static-log-analysis/`, or with
`go -C services/static-log-analysis`. Run dashboard npm commands from
`services/dashboard/`.

## Progress

The plan is
[docs/static-log-analysis/implementation/tdd-plan.md](docs/static-log-analysis/implementation/tdd-plan.md).
Update this section when a milestone or a numbered behaviour lands.

| Milestone | State |
|---|---|
| 0 — test harness | Complete |
| 1 — deterministic domain | Complete |
| 2 — security and admission | Complete |
| 3 — journal | Complete |
| 4 — CockroachDB | Complete |
| 5 — vertical slice | Complete |
| 6 — resilience and sources | Substantially complete: see acceptance.md for what is not demonstrated |

What exists in code: the test harness, the domain model, redaction, admission,
the Pebble journal, the deterministic incident engine, the CockroachDB schema
and transactions, `internal/pipeline`, which coordinates the whole
OTLP-to-investigation slice, and the Milestone 6 work so far —
`cmd/log-analysis`, `internal/runtime` (roles, configuration, worker,
shutdown), `internal/otlpreceiver` including its listener bounds and panic
recovery, `internal/outbox` (the publisher and its failure semantics), and
`internal/cloudwatch` (the pull-based source adapter, verified against
LocalStack).

Also landed: `internal/outbox/sqsaws`, the Amazon SQS Standard transport, so the
`outbox` role now runs end to end against a real queue; `pipeline.IngestRecords`
and `internal/cloudwatch/cwsink`, the second ingestion entry point that keeps
the native CloudWatch event ID `cw:v1` is defined over; fuzz targets for
redaction and OTLP decoding with a nightly gate; and the normative
`cloudwatch-source.md`.

The source-only role drives the CloudWatch adapter into a journal for bounded
diagnostics and adapter testing. Production uses `log-analysis cloudwatch`,
which binds no listener and runs both the poll and process workers against its
one journal.

The service now has a non-root production image, a reviewed one-shot migration
utility, and a self-contained local Compose deployment for CockroachDB,
LocalStack SQS, the safe combined `all` topology, the separate `outbox` role,
and the Collector's persistent pre-redaction queue. The deployment entry point
is `services/static-log-analysis/compose.yaml`.

Cross-repository local integration is now a stable contract rather than a
container-name convention: the Collector owns the named
`static-log-analysis-ingress` network and alias, the Compose overlay renderer
attaches an arbitrary instrumented service, and the smoke script proves the
OTLP-to-SQS path. OTLP producers without a native UUIDv7 use the implemented,
measured `derived:v1` fallback; malformed native IDs are still rejected.

Envoy-style OTLP records that omit observed time but supply event time use that
event time as a deterministic, explicitly versioned fallback. Permanent
record-local rejections are counted through a closed unlabelled metric set, so
discarded payloads do not erase their safe failure category.

Also landed: `internal/enrich` (non-blocking deployment enrichment, wired
through normalization and the runtime), capacity shedding wired to a live
journal, `internal/outage` (the CockroachDB outage scenario, network severed
rather than container stopped), replica drain, the admin health/readiness/
metrics listener, and `deploy/collector/` with the first-pass redaction
security.md requires before the Collector's persistent queue.

The service-local hackathon deployment is intentionally small:
`deploy/aws/compose.yaml` runs migration, combined CloudWatch processing, and
outbox containers on one EC2 host; `infra/aws` creates that host, its encrypted
disk, regional SQS queues, stable outbound address, and least-privilege instance
role. The fast CI gate renders Compose and validates Terraform. Managed
CockroachDB remains external.

The optional public operations dashboard is a Better Auth-protected Next.js
container behind Caddy on the same analysis host. Its local SQLite database is
only for dashboard users, sessions, and roles. It reads a bounded safe
projection, unlabelled metrics, readiness, and SQS queue attributes, has no
CockroachDB credential, and stays in an optional Compose profile so dashboard
configuration cannot block analysis startup.

The dashboard's local agent slice now distinguishes recommendations from
executed remediation, includes bounded repository/remediation list and detail
views, and renders a durable maximum-50-event categorical progress timeline.
The agent keeps read-only remediation history available while execution is
forced off; mutation routes remain unavailable without an initialized sandbox.

The dashboard now also has the first Phase 5 agent read model: its server calls
authenticated `/ping` and `/agent/:id`, correlates at most ten recent detector
investigations with agent state, and renders a detail page containing only safe
signal metadata, verdict/timing counts, and a stripped tool-name trace. Model
prose, tool inputs/output, context, provider errors, and the agent token never
reach the browser. Repository/remediation visibility and all mutations remain
unfinished.

The approved investigation-only agent integration is implemented but not yet
deployed to AWS. `agent_space` now strictly admits the closed assignment envelope and
exact SQS attributes against a trusted region/tenant/classification boundary,
then reads the immutable `SafeValue` snapshot through a distinct read-only
analysis database connection. Terraform provides an exact-queue App Runner
consumer role, and the agent deployer pins one instance and forces remediation
off. Database grants, Terraform apply, App Runner deployment, and end-to-end
release evidence remain operator work; see `docs/agent-sqs-deployment.md`.

Local integration is available through `compose.local-integration.yaml`. It
joins the static-analysis Compose network, creates a separate agent database
and a context-reader user limited to the three safe projection tables, binds
the API to loopback port 18081, and forces remediation off without mounting the
Docker socket. The detector-to-SQS-to-live-context path is demonstrated. A
GPU-backed local run using `qwen2.5-coder:14b-instruct` and
`nomic-embed-text` also completed and persisted a one-iteration `ROLLBACK`
verdict. `compose.local-models.yaml` is the loopback-only Linux overlay for
hosts whose firewall drops Docker bridge traffic. Automatic remediation is
still unproven and remains disabled. See
`docs/deployment/local-agent-integration-readiness.md` and
`docs/deployment/local-model-hosting.md`.

The AWS CloudWatch pull path is the combined `cloudwatch` role. Its poll and
process workers share one journal, checkpoints live on a second Docker volume,
and the EC2 instance profile grants exact-log-group `logs:FilterLogEvents` plus
SQS publishing. The source-only role remains diagnostic and is not deployed.

Missing: a real enrichment provider and late-answer context versioning, the
audit-event subsystem, rule reload, an operator-facing suppress/reopen API, and
the OpenTelemetry demo Flagd scenarios (generic Demo log forwarding is wired).
There is also an unresolved architectural
question — recorded in acceptance.md — about how a split `ingest` replica's
journal is ever processed, given that journals are per-replica and non-shared.

`docs/static-log-analysis/acceptance.md` is the running answer to "is it done":
it links each of testing.md's ten acceptance items to named tests and says
plainly which are not demonstrated. Keep it current — every test name and gate
in it is verified against the tree.

## How the work is done

Production behaviour begins with a failing test at the narrowest credible
boundary. Implement only enough to pass, then add boundary, failure, and
concurrency coverage in proportion to risk. Tests assert observable state and
invariants, not private call order. Real Pebble and real CockroachDB are used
for storage contracts; mocks do not substitute for durability or concurrency.

Never weaken, skip, or delete an existing test to make something pass. If a test
encodes wrong behaviour, say so and change it deliberately, in its own step.

A test double must not fabricate the behaviour under test. A review found two
tests named for the five-in-five rule that passed regardless of the real window
logic, because the fake store elected an investigation whenever it saw five
inputs. Name a test for what it actually pins.

Documentation is part of a milestone's definition of done, not a follow-up.

Precedence when documents disagree, highest first:

1. Security and regional-boundary requirements
2. The normative implementation contracts under `docs/.../implementation/`
3. Architecture Decision Records
4. Architectural overview documents

A conflict is resolved in documentation and tests before implementation merges.

## Gates

```text
gofmt -l . && go vet ./... && go vet -tags=integration ./...
./scripts/check-generated-proto.sh
go test ./...                                                 # fast: unit, fixture, property
REQUIRE_DOCKER=1 go test -p 1 -race -tags=integration ./...    # storage and concurrency
```

Tests needing Docker sit behind the `integration` build tag. `REQUIRE_DOCKER=1`
turns an unavailable container into a failure rather than a skip, because a
skipped storage test in CI is the same as no storage test.

`-p 1` on the integration run is a resource bound, not a correctness workaround.
Each test binary starts its own CockroachDB container for its whole run, so the
default per-CPU package parallelism puts several on one machine at once under
the race detector. That has already killed a container mid-run and failed every
remaining test with `connection refused`, which reads like a storage bug and is
not one. Serialized, the job is about five minutes.

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
adding a version, never by editing one. This extends to a generation's episode
identity at runtime. `PersistenceDecision` is the immutable bridge to storage
and `incident_generations` is keyed on `(deployment_id, episode_start)`, so an
episode start is assigned once and adopted thereafter. Retention compaction may
release payloads, but re-deriving an identity from the survivors gives one live
episode a second durable generation and splits its occurrences, evidence, and
election index across both.

The trusted envelope comes from authenticated transport identity and
configuration. Application-supplied attributes are untrusted claims; a service
or region claim that conflicts with the envelope is rejected and counted, never
silently accepted or rewritten.

## Failure semantics that are easy to get backwards

These are settled by review and by the documents; changing one needs a reason.

Quarantine deletes a record's durable payload, so only a **record-local** error
may ever reach it. A configuration mismatch and a generation identity conflict
are not record-local: the first would destroy every acknowledged record for a
deployment typo, and the second is the exact evidence an operator must
investigate. Both keep their claims and surface to the caller. In
`persistence`, `ErrConfiguration` names the batch-wide case so a caller can tell
it apart from record-local `ErrInvalidInput`; bisecting a batch-wide error down
to single records and quarantining each one destroys every acknowledged record.

A permanent failure reported as retryable makes a transport replay a poisoned
batch forever and starves its valid siblings. A retryable failure reported as
permanent destroys work a retry would have saved. Ingestion distinguishes the
two explicitly, and so must every transport built on it.

Startup is where a misdeployed replica is caught. `pipeline.New` cross-validates
the journal manifest and the store boundary against the configured scope,
classification, and redaction policy version. Downstream, the same mismatch
makes every individual record look invalid instead.

Startup must never be fatal because of one durable record. A record admitted by
design but not observable by the rule engine — one with no service identity, for
instance — is skipped during recovery and terminated categorically later. The
alternative is a single record permanently bricking a replica.
