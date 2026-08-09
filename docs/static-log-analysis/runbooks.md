# Operational Runbooks

operations.md requires twelve runbooks before production. These are those twelve
plus a drain runbook, which scale-down needs and which the list did not name. Each one names
the signal an operator sees first, what the system is doing while they read, and
the action.

Two rules run through all of them, and both are properties of the system rather
than advice:

- **An acknowledged record is already durable.** Every OTLP success was returned
  only after the journal committed, so no recovery step below risks losing
  acknowledged work by being slow.
- **Quarantine deletes a record's durable payload.** Nothing here tells you to
  quarantine a batch. Only record-local faults may ever reach it, and the code
  decides that; an operator reaching for it manually is destroying evidence.

Where a runbook says a state is "not implemented yet", that is deliberate: see
acceptance.md for what is not demonstrated.

---

## 1. Journal capacity exhaustion and recovery

**Signal.** Ingestion returns `503`/`UNAVAILABLE`; the Collector's queue grows.

**What the system is doing.** The journal refuses to grow past
`-journal-max-bytes`, or past the point where free space would drop below
`-journal-min-free-bytes`. Mandatory records are refused with a *retryable*
failure and never falsely acknowledged. The Collector's persistent queue holds
them.

**Action.**

1. Confirm it is capacity and not a disk fault: a capacity refusal is
   `ErrCapacity`; a disk fault is not.
2. Drain the backlog by letting the `process` role catch up. Capacity is
   released as records are committed to CockroachDB and compacted out of the
   journal, so the usual cause is that processing is behind, not that ingestion
   is ahead.
3. If CockroachDB is the reason processing is behind, follow runbook 4 first.
   Capacity cannot be recovered while the database is unavailable.
4. If the volume is genuinely undersized, grow it and raise
   `-journal-max-bytes`. operations.md sizes it as
   `measured peak serialized bytes/second × 3600 × safety factor`, at least 2×.

**Do not** delete journal files to make space. They are the only copy of
acknowledged, unprocessed work.

**Shedding.** `-shed-near-capacity-percent` and `-shed-eligible-sources` wire
operations.md's utilization table to the live journal. Both must be set or
neither, and shedding is off by default. Only explicitly unprotected DEBUG and
INFO from a declared source is ever eligible; everything else backpressures.
Check `static_log_analysis_shed_total` and
`static_log_analysis_backpressured_total` on `/metrics` to see which is
happening.

## 2. Journal corruption and read-only preservation

**Signal.** A replica fails to start, or a read fails with a corruption error.

**What the system is doing.** The journal refuses to reopen rather than serving
partial state. It does not repair itself and does not skip the bad record,
because either would silently change what was acknowledged.

**Action.**

1. Stop the replica. Do not restart it in a loop; restarting cannot repair a
   Pebble corruption and each attempt writes more.
2. **Preserve the volume before anything else.** Snapshot it. This is the
   evidence.
3. Route traffic to another replica. Journals are per-replica and not shared, so
   the others are unaffected.
4. Recover the acknowledged-but-unprocessed records from the snapshot offline.
   Records are keyed by `record_id`; identity is stable, so re-ingesting them
   through a healthy replica contributes each exactly once.
5. Rebuild the failed replica on a fresh volume.

## 3. Collector backlog and failed export

**Signal.** Collector queue depth grows; exporter reports failures.

**What the system is doing.** Depends on the answer it got, and the distinction
matters:

| Answer | Meaning | Collector behaviour |
|---|---|---|
| `UNAVAILABLE` / `503` | retryable | holds and retries — correct |
| `INVALID_ARGUMENT` / `400` | permanent | **drops the batch** |
| `UNAUTHENTICATED` / `401` | permanent | drops the batch |
| `RESOURCE_EXHAUSTED` (gRPC) | body over the compressed limit | drops the batch |
| `UNIMPLEMENTED` | permanent | drops every batch |

**Action.**

1. Read the *code*, not the queue depth. A growing queue with `503` is the
   system working as designed during a downstream problem.
2. `UNIMPLEMENTED` means a compression or protocol mismatch. The service links
   gzip explicitly for this reason; if this appears, the Collector is using an
   encoding this build does not register.
3. `400` on every batch usually means the envelope's region does not match the
   replica's, or the payload is not OTLP protobuf. `415` specifically means JSON
   OTLP, which this service does not implement.
4. `503` with "receiver is at its in-flight export limit" is shedding, not
   failure. Raise `-otlp-max-in-flight` only after checking the replica has the
   memory for it: worst case is roughly that number × 36 MiB at default
   admission limits.

## 4. CockroachDB outage and replay verification

**Signal.** `process` cycles fail; incidents stop being created; ingestion keeps
succeeding.

**What the system is doing.** This is the designed behaviour. Ingestion holds no
database credential and keeps acknowledging into the journal. The `process` role
retries with backoff. Journal claims are renewed rather than lost, so the cohort
in flight is not abandoned.

**Action.**

1. Do not restart ingest replicas. They are working.
2. Watch journal utilization; runbook 1 applies if it approaches capacity. The
   design target is at least 30 minutes of retention at provisioned capacity.
3. When CockroachDB returns, processing resumes on its own. No manual replay.
4. **Verify:** every journaled record should contribute exactly once. Occurrence
   counts are keyed on `record_id`, and the journal commit happens only after
   the CockroachDB transaction returns, so an interrupted cycle re-claims rather
   than double-counting.

**Automated.** `internal/outage` runs this scenario against a real CockroachDB
with the network to it severed: ingestion keeps succeeding, processing fails,
nothing is committed during the outage, and every acknowledged record
contributes exactly once afterwards. See acceptance.md item 9 for what the test
does and does not reproduce.

## 5. Outbox and SQS backlog, dead-letter replay

**Signal.** `outbox_messages` in state `pending` grows, or messages appear on
the dead-letter queue.

**What the system is doing.** Committed assignments stay in the outbox until the
queue accepts them. A message that fails keeps its place and is retried with
exponential backoff and jitter, up to `-outbox-max-attempts`.

**Action for a backlog.**

1. Check the publisher's error class. A **deployment fault** — missing queue,
   revoked credential, wrong region — releases every claim immediately and stops
   the cycle. Nothing drains until it is fixed, by design: bisecting a
   deployment-wide failure into per-message dead letters would move every
   committed assignment out of the way for one configuration mistake.
2. A **retryable** failure needs no action; the backlog drains when the queue
   recovers.

**Action for dead letters.** Each carries a `failure_reason` attribute from a
closed set:

| `failure_reason` | Meaning |
|---|---|
| `rejected` | this message can never be accepted; inspect the body |
| `attempts_exhausted` | the queue kept refusing it through its whole schedule |
| `region_mismatch` | should be impossible; investigate before replaying |

The body is byte-identical to what was committed, so replay is a replay of
exactly that. Republish by sending the body back to the main queue. Consumers
deduplicate on `deduplication_key`, so a redundant replay is harmless.

**A published message is never republished by a cycle** — the durable
`published` state is what stops it, not timing.

## 6. Disable, shadow, or roll back a noisy rule

**Not implemented.** Rules are compiled into the binary; there is no rule
reload, no shadow mode, and no ruleset versioning at runtime. operations.md's
"rule reload invalid → previous immutable ruleset remains active" has nothing
behind it.

**Today's only lever** is to deploy a build with the rule changed, or to stop
the `process` role for the affected deployment, which stops all detection for
it. Neither is a substitute for this runbook.

## 7. Suppress or reopen an incident with audit history

**Partially implemented.** Incident generations, quiet transitions, and
immutable context versions exist and are audited by construction: a generation
becomes quiet at exactly fifteen minutes of silence, and a recurrence past the
two-hour boundary creates a new generation rather than mutating the old one.
`context_version` is written with `greatest(...)` so it cannot regress.

There is **no operator-facing suppress or reopen API**, and no
investigation-completion API. A family stays retained for
`DefaultReinvestigationInterval` (24h) after its last activity.

**Do not** edit `incident_generations` by hand. It is keyed on
`(deployment_id, episode_start)` and an episode start is assigned once and
adopted thereafter; changing one splits a live episode's occurrences, evidence,
and election index across two durable generations.

## 8. Reassign a stuck investigation

**Signal.** An investigation is claimed but not progressing.

**What the system is doing.** Claims carry a lease. When it expires the
investigation becomes claimable again and is reassigned. Exactly one owner wins
at both the unowned and the expiry boundary.

**Action.** Wait for the lease. If an agent died, expiry is the mechanism and it
needs no help. Forcing a reassignment by clearing the claim risks two agents
holding one investigation, which is the thing the lease exists to prevent.

## 9. Rotate redaction HMAC keys

**Not applicable as written.** The baseline policy performs pattern replacement
and forbidden-value substitution; it does not derive keyed pseudonyms, so there
is no HMAC key to rotate.

**What does need care** is `-redaction-forbidden-values`. It is part of the
policy version, and the policy version is part of the journal manifest and the
store boundary. Changing it changes the version, and a replica whose configured
version disagrees with its journal manifest **refuses to start** — deliberately,
because downstream the same mismatch makes every individual record look invalid.

**To change forbidden values:** drain the replica (let `process` empty the
journal), stop it, change the flag, start it. Do not change the flag under a
journal with pending records.

## 10. Diagnose missing service or deployment metadata

**Signal.** Records arrive but produce no incidents, or deployment context is
`unknown-deployment`.

**What the system is doing.** A record with no service identity is *admitted by
design* — data-model.md says missing service identity only prevents
service-specific agent execution. It is skipped during recovery and terminated
categorically during processing. It does **not** brick the replica.

**Action.**

1. For OTLP: the service identity comes from the payload but must be inside the
   envelope's `-otlp-allowed-services`. A service outside the allow-set is
   rejected as a conflicting claim and counted, never silently accepted.
2. For CloudWatch: service and environment are *operator configuration* per log
   group, because CloudWatch events carry no authenticated service identity.

**Deployment identity** comes from enrichment. Declare it with
`-enrichment-deployments` as `service=environment=deployment_id=version`.

Records that arrive before the first lookup answers group under
`unknown-deployment` **permanently** — an identity is assigned once and adopted
thereafter. That is deliberate: enrichment never blocks ingestion, so the first
records after a start or a new service appearing pay for it. It is not a fault
to chase.

On `/metrics`, `static_log_analysis_enrichment_provider_failures_total` rising
with a flat `..._resolved_total` is a metadata provider to look at. Records keep
being ingested regardless; that is the whole point of the design.

## 11. Drain a replica before detaching its volume

**When.** Scale-down, volume migration, or changing anything that is part of the
journal manifest (see runbook 9).

**What the system does.** `Drain` stops accepting new work first and then
processes the journal to empty. While draining, `/readyz` answers 503, so a
balancer stops routing to the replica before its listeners close. It returns
only when nothing is pending and nothing is claimed — at which point the volume
holds no acknowledged work and is safe to detach.

**Budget it.** A drain cannot finish faster than the records already in the
journal can finalize; a record inside its allowed-lateness window is held, not
stuck.

**An `ingest` or `source` replica refuses to drain.** It runs no process worker,
so nothing in it can turn its journal into committed rows. This is honest rather
than helpful: see the open architectural question in acceptance.md.

## 12. Upgrade and roll back the journal format

**What the system is doing.** The journal manifest records the format version,
scope, classification, and redaction-policy version. `pipeline.New`
cross-validates all of it at startup and refuses to run on a mismatch. An
incompatible version is refused rather than migrated in place.

**Action for an upgrade.**

1. Drain the replica first: let `process` commit everything so the journal holds
   no pending records.
2. Confirm `Stats().Pending == 0`.
3. Stop, deploy, start. A replica that starts is a replica whose manifest agreed.

**Action for a rollback.** The same, in reverse, and only from a drained
journal. A newer journal cannot be read by an older binary; that is the point of
refusing rather than guessing.

**If a replica refuses to start** with a boundary mismatch, it is almost always
pointed at another replica's volume, or a scope flag was typed wrong. Check
`-region`, `-tenant-id`, `-classification`, `-redaction-policy`, and
`-journal-dir` against the manifest named in the error. The process exits **2**
for this class, so a supervisor will not restart-loop it.

## 13. Verify regional data boundaries

**Run these checks.** Each corresponds to an enforced boundary, so a failure is
a real finding rather than a policy reminder.

1. **Envelope vs replica.** An envelope whose region is not the replica's is
   rejected permanently and counted. Confirm the scope-rejection counter is zero
   in steady state.
2. **Payload claims cannot influence the envelope.** The trusted envelope comes
   from authenticated transport identity and configuration only. A service or
   region claim inside a payload that conflicts is rejected and counted, never
   rewritten.
3. **Queue region.** The publisher refuses at startup to publish into a queue
   whose region is not its own, and refuses per message if the scope, the
   message attributes, or the decoded assignment disagree.
4. **Source region.** The CloudWatch adapter refuses to read a group outside its
   configured account and region, and never reads those values back from an API
   answer.
5. **Ingest replicas hold no database credential.** An `ingest` replica refuses
   to start if `STATIC_LOG_ANALYSIS_DATABASE_DSN` is set at all.
6. **Secrets.** The DSN never reaches a log line, an error, or the startup
   description; the driver's own parse error is discarded rather than wrapped,
   because it echoes the connection string.
