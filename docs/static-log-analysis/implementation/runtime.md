# Runtime, Roles, and OTLP Ingress

## Artifact and roles

There is one long-running executable, `log-analysis`, and the role is its first
positional argument:

```text
log-analysis all
log-analysis ingest
log-analysis process
log-analysis outbox
log-analysis source
```

| Role | Opens | Listens | Holds a database credential |
|---|---|---|---|
| `all` | journal, database, receiver, worker | yes | yes |
| `ingest` | journal, receiver | yes | no |
| `process` | journal, database, worker | no | yes |
| `outbox` | database, queue client, worker | no | yes |
| `source` | journal, checkpoints, source client, worker | no | no |

`source` is the pull-based ingestion path. It writes to a journal exactly as
`ingest` does but binds no listener, because nothing pushes to it. It is
deliberately **not** part of `all`: a source replica needs log groups
configured, and folding it in would make every replica require CloudWatch
configuration it does not use. Its contract is cloudwatch-source.md.

A `source` replica holds no database credential, for the same reason `ingest`
does not.

The container image also carries the one-shot `log-analysis-migrate` deployment
utility. It is not a service role: an init job runs it before application
replicas. It accepts no positional arguments, reads
`STATIC_LOG_ANALYSIS_DATABASE_DSN`, and calls the reviewed embedded migration
engine, including migration checksums and live-catalog verification. Its output
is categorical and never includes the DSN.

The `ingest` role holds no database credential. security.md requires
least-privilege identities per role, and operations.md requires ingestion to
survive a 30-minute CockroachDB outage, which a replica that had to reach
CockroachDB in order to start could not do. Because the coordinator requires a
`Store`, an ingest replica is given one that refuses every call rather than one
that pretends to succeed. A consequence is that on an ingest replica the
store-boundary half of the startup cross-check is degenerate: it echoes the
configuration. The journal manifest check is the real one there.

Each replica owns exactly one journal directory. Two processes MUST NOT open one
journal; the journal's own directory lock enforces this, and the `process` role
therefore also owns a volume even though it never listens.

## Configuration

Configuration is command-line flags plus one environment variable. There is no
configuration file and no configuration library: the surface is small, and a
flag that must be typed is a flag an operator has seen.

| Flag | Default | Meaning |
|---|---|---|
| `-region`, `-tenant-id` | none | the regional boundary this replica serves |
| `-classification` | `SENSITIVE` | highest classification permitted here |
| `-worker-owner` | `static-log-analysis` | journal claim owner |
| `-journal-dir` | none | this replica's non-shared volume |
| `-journal-max-bytes` | none | journal capacity |
| `-journal-min-free-bytes` | 1 GiB | filesystem headroom |
| `-redaction-policy` | `baseline` | named policy |
| `-redaction-forbidden-values` | empty | service-specific forbidden values |
| `-max-*` | admission defaults | admission limits |
| `-otlp-grpc-listen` | `:4317` | OTLP/gRPC |
| `-otlp-http-listen` | `:4318` | OTLP/HTTP |
| `-otlp-trust-source` | none | `mutual_tls` or `static_local` |
| `-otlp-source-account` | none | authenticated source account |
| `-otlp-allowed-environments`, `-otlp-allowed-services` | none | claim allow-sets |
| `-otlp-source-instance`, `-otlp-credential-identity` | none | caller identity, `static_local` only |
| `-otlp-tls-cert`, `-otlp-tls-key`, `-otlp-client-ca` | none | `mutual_tls` only |
| `-otlp-read-header-timeout` | 10s | bound on receiving request headers |
| `-otlp-read-timeout` | 30s | bound on receiving a whole request, body included |
| `-otlp-write-timeout` | 30s | bound on writing one response |
| `-otlp-idle-timeout` | 60s | bound on an idle keep-alive connection |
| `-otlp-max-header-bytes` | 64 KiB | bound on the request header block |
| `-otlp-max-in-flight` | 64 | exports admitted at once, both transports together |
| `-otlp-max-concurrent-streams` | 256 | concurrent gRPC streams per connection |
| `-process-cohort` | 100 | records claimed per cycle |
| `-process-interval` | 250ms | pause after a productive cycle |
| `-process-idle-interval` | 1s | pause when nothing was claimed |
| `-process-backoff-min`, `-process-backoff-max` | 1s, 30s | failure backoff |
| `-outbox-queue-url`, `-outbox-dead-letter-queue-url` | none | the two regional queues |
| `-outbox-endpoint-url` | none | SQS-compatible endpoint instead of AWS |
| `-cloudwatch-account` | none | authenticated account, part of `cw:v1` identity |
| `-cloudwatch-log-groups` | none | comma-separated `group=service=environment` |
| `-cloudwatch-source-instance`, `-cloudwatch-credential-identity` | none | retained for audit |
| `-cloudwatch-checkpoint-dir` | none | this replica's non-shared checkpoint volume |
| `-cloudwatch-endpoint-url` | none | CloudWatch-compatible endpoint instead of AWS |
| `-cloudwatch-lookback`, `-cloudwatch-max-lookback`, `-cloudwatch-max-window` | adapter defaults | window sizing |
| `-cloudwatch-page-limit`, `-cloudwatch-max-pages` | adapter defaults | per-cycle read bounds |
| `-cloudwatch-interval`, `-cloudwatch-idle-interval` | 250ms, 5s | poll cadence |
| `-cloudwatch-backoff-min`, `-cloudwatch-backoff-max` | 1s, 30s | failure backoff |
| `-enrichment-deployments` | empty | `service=environment=deployment_id=version` declarations |
| `-enrichment-budget`, `-enrichment-ttl`, `-enrichment-retry-after` | 10s, 5m, 30s | metadata lookup bounds |
| `-enrichment-max-entries` | 8192 | bound on cached deployments |
| `-shed-near-capacity-percent` | 0 (off) | journal utilization at which eligible records may be shed |
| `-shed-eligible-sources` | empty | sources whose unprotected DEBUG and INFO may be shed |
| `-admin-listen` | `:9464` | health, readiness, and metrics (empty disables) |
| `-shutdown-timeout` | 30s | drain budget |

`-journal-max-bytes` deliberately has no default. operations.md derives journal
capacity from measured peak serialized bytes per second, and a default would
substitute an unmeasured number for the one thing standing between a database
outage and acknowledged loss.

Admission limits are configurable downward only, matching
identity-and-admission.md. An explicit `0` is refused rather than read as "use
the default", because an operator who writes `0` means zero.

### Listener bounds

The `-otlp-*` bounds above read `0` the opposite way: `0` means the documented
default, and a negative value is refused. The difference is deliberate. An
admission limit of zero is a coherent instruction, so it is obeyed. A listener
bound of zero is not a smaller bound — `net/http` reads it as no deadline and
grpc-go reads an unset stream count as `math.MaxUint32` — so honouring it
literally would silently remove the bound an operator was trying to set.

These bounds matter more than the flag table suggests. The `static_local` trust
mode has no client certificate by design, so on that socket they are the only
thing between a caller and this replica:

- Without a read bound, a caller that declares a body and then dribbles it holds
  a handler, its goroutine, and its accumulated buffer for as long as it likes,
  at a cost to the caller of one socket.
- Without an in-flight bound, peak memory is whatever callers choose. Each
  admitted export holds its compressed body, its decoded form, and its
  materialized form at the same time, so `-otlp-max-in-flight` is the multiplier
  on admission's byte limits that decides what this replica can be sized
  against. At the defaults that is roughly 64 × 36 MiB.

`-otlp-max-in-flight` is one pool shared by both transports. Two independent
bounds would let a caller take twice the configured memory just by splitting its
traffic across gRPC and HTTP.

An export refused because the pool is full is shed as **retryable**
(`UNAVAILABLE` / `503`), never as a verdict about the batch. The Collector's
persistent queue holds it, which is the whole reason shedding is safe here.
Acquisition never waits: a caller made to queue would hold the socket and the
memory the bound exists to cap.

`-otlp-read-header-timeout` may not exceed `-otlp-read-timeout`; that
combination would end every request before its headers could arrive.

### Handler panics

A panic under an export handler is answered as retryable on both transports and
the replica keeps serving. grpc-go does not recover a handler panic on its own:
without an interceptor it unwinds through the serving goroutine and takes the
whole replica with it, including every export in flight and the journal drain
behind them. `net/http` does recover, but it answers nothing, so a Collector
would see a dropped connection instead of a categorical status.

The answer is retryable because a panic is a defect in this service, not a
verdict about the caller's batch — the same reasoning that makes an unrecognized
failure retryable in the table above.

### Source configuration

`-cloudwatch-log-groups` carries all three components in one token because a
CloudWatch event has no authenticated service identity: service and environment
are operator declarations, and requiring them together makes it impossible to
configure a group whose identity nobody chose. Naming one group twice is
refused; it would give that group two identities and let map order decide which
a record got.

The envelope's allowed service and environment sets are **derived** from the
configured groups rather than configured beside them, so an allow-set can never
be narrower than the groups it has to admit.

`-cloudwatch-checkpoint-dir` MUST NOT be `-journal-dir`. The two have different
lifetimes: a journal is drained and may be rebuilt, and a checkpoint rebuilt
alongside it would reread the whole lookback window.

A role that does not poll MUST NOT carry source configuration, for the same
reason it must not carry a database credential it cannot use: it reads as though
that replica were retrieving logs when nothing in it ever will.

**Credentials against a non-AWS endpoint.** `-cloudwatch-endpoint-url` and
`-outbox-endpoint-url` change where the client connects, not how it
authenticates. A local endpoint has no instance role, so the AWS default
credential chain finds nothing and every call fails on identity. Supply
`AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` in the environment for such a
deployment.

### Enrichment

Deployment identity is part of grouping, not decoration: a deployment change
creates a linked incident generation. But resolving it MUST NOT put an external
dependency inside the ingestion path. architecture.md: "Detection and
persistence never wait for external metadata. Cached enrichment is used
immediately."

`internal/enrich` therefore answers from cache and schedules everything else.
A lookup never blocks. The consequence is explicit and accepted: records that
arrive before the first answer group under `unknown-deployment` permanently,
because an identity is assigned once and adopted thereafter. That is the correct
trade — the alternative is an ingestion path that stops when a metadata service
does.

A provider distinguishes two negatives. `ErrNoDeployment` is definite and is
cached as `not_available`; anything else is retryable, stays `pending`, and is
tried again after `-enrichment-retry-after`. Conflating them would either retry
forever or give up permanently.

Enrichment is optional. Without declarations every record groups under
`unknown-deployment` with status `pending`, which is what data-model.md
specifies for a deployment that is not yet known.

### Capacity shedding

`-shed-near-capacity-percent` and `-shed-eligible-sources` wire operations.md's
utilization table to a live journal. Both must be set or neither: a threshold
with nothing eligible, or eligible sources with no threshold, sheds nothing and
is refused so an operator is not left believing otherwise.

Shedding is **off by default**. It drops data deliberately, and an operator who
declared no eligible source declared that nothing may be dropped.

Only explicitly unprotected DEBUG and INFO from a declared source is ever
eligible. Uncertain is protected. A record that cannot be shed and cannot be
persisted produces retryable backpressure, never an acknowledgement: "It never
acknowledges a silently dropped mandatory record."

Capacity is observed once per batch rather than per record, because the
observation is a point in time either way and a batch straddling the threshold
would otherwise shed some of its records and not others for no reason an
operator could explain.

### The admin surface

`-admin-listen` serves three paths on a listener separate from ingress, because
whoever may export logs must not thereby be able to read this replica's state:

| Path | Meaning |
|---|---|
| `/healthz` | liveness — the process is running |
| `/readyz` | readiness — this replica can do its role's work |
| `/metrics` | Prometheus text exposition of the categorical counters |

Liveness is unconditional on purpose. Conflating it with readiness makes a
supervisor kill a replica that is merely waiting for a dependency, which is
exactly when its journal is holding acknowledged work.

Every metric is an unlabelled counter. operations.md forbids unbounded label
cardinality, and every interesting label here — service, stream, fingerprint —
is unbounded. The exposition format is written by hand rather than through a
client library, because a library that can create time series can create
unbounded ones.

### Draining a replica

architecture.md: "Scale-down drains a replica before its volume is detached or
deleted." That is a different operation from shutdown. Shutdown drains the
handlers in flight and closes; **drain drives the journal to empty**, because a
journal with pending records is acknowledged work that only this replica's
volume holds.

`Drain` stops accepting new work first and processes afterwards. The other order
is a race the replica cannot win, because ingestion would keep adding to what
draining is trying to empty. While draining, `/readyz` reports 503 so a balancer
stops routing to it.

A drain cannot finish faster than the records in the journal can finalize: a
record inside its allowed-lateness window is held, not stuck. Budget accordingly.

A role that runs no process worker **refuses to drain** rather than looping until
its deadline. Nothing in an `ingest` or `source` replica can turn its journal
into committed rows, and reporting a timeout would suggest slowness rather than
impossibility. See the open question in acceptance.md about how such a replica's
journal is drained at all.

### The database DSN

The CockroachDB DSN comes from `STATIC_LOG_ANALYSIS_DATABASE_DSN` and MUST NOT
be a flag. A flag default appears in `--help`, in the process table, and in
shell history, and a rotated credential would leave copies in all three.

The DSN MUST NOT reach any log line, error, or startup description. The driver's
own parse error echoes the connection string, so that error is discarded rather
than wrapped, and the startup description reports only whether a DSN was
configured. Masking inside a dependency is not a property this service relies on.

A role that opens no database refuses to start if the DSN is set at all. The
`ingest` role is the case that matters: it opens no store, because security.md
requires least privilege per role and operations.md requires ingestion to keep
running through a database outage, which a replica that had to reach CockroachDB
to start could not do. Accepting the credential anyway would leave a replica
holding a secret nothing in it can use, and would hide from the operator that
this replica never talks to CockroachDB. Shipping one environment to every role
is therefore not a supported deployment; each role gets the credentials it owns.

### Startup validation

Every value is validated before anything is opened, and an invalid value refuses
to run. Beyond flag-local checks, `pipeline.New` cross-validates the configured
scope, classification, and redaction-policy version against two immutable
boundaries: the journal directory's manifest and the store's boundary. A
contradiction is reported as an operator-facing startup error naming the region,
tenant, classification, policy version, and journal directory involved. This is
the only place such a contradiction can be caught: downstream every record would
instead look individually invalid, and isolating an individually invalid record
destroys its durable payload.

Opening the journal also probes the filesystem's free space once. The capacity
policy is otherwise consulted only on a write, where a filesystem whose free
space cannot be measured is indistinguishable from a full one and every record
is refused. Left to that, a replica on a platform with no free-space
implementation would bind its listeners, log that it started, and then refuse
every export for the rest of its life. Startup is where a misdeployed replica is
caught, so an unmeasurable volume is a configuration fault instead.

Configuration failures exit with status 2 and runtime failures with status 1, so
a supervisor does not restart-loop a process whose flags can never work.

## OTLP ingress

Both transports are served: OTLP/gRPC on 4317 and OTLP/HTTP with binary
Protobuf on 4318 at `/v1/logs`. source-adapters.md makes gRPC primary with HTTP
a supported fallback.

The HTTP surface accepts `POST` with `application/x-protobuf` and an optional
`gzip` content encoding. JSON OTLP is a valid protocol variant this service does
not implement; it is answered `415` rather than being allowed to fail later as
if the batch were malformed. Any other path is `404`, because a traces or
metrics export delivered here is a misconfigured Collector and answering it
would hide that.

Raw OTLP bytes pass through the receiver opaquely. The request is handed to
`pipeline.Service.Ingest` as bytes, including over gRPC where the decoded
message is re-marshaled, because admission owns bounded decoding and a generated
decoder has already spent the nesting and structural-node budget before it could
enforce it.

### Acknowledgement boundary

The receiver returns OTLP success ONLY after `Ingest` reports that the accepted
records are committed to the journal. A result carrying no acknowledgement is
answered as retryable, never as success, even though the coordinator does not
produce one today: a record held only in memory must never be acknowledged, and
the transport is the last place that invariant can be enforced.

### Retry semantics

A permanent failure returned as retryable makes a Collector replay a poisoned
batch forever and starve the valid batches queued behind it. A retryable failure
returned as permanent discards acknowledged-mandatory data. The mapping is
therefore explicit and identical on both transports:

| Outcome | gRPC | HTTP | Retry |
|---|---|---|---|
| acknowledged | `OK` | `200` | n/a |
| `ErrJournalUnavailable` | `UNAVAILABLE` | `503` | retryable |
| result without acknowledgement | `UNAVAILABLE` | `503` | retryable |
| unrecognized failure | `UNAVAILABLE` | `503` | retryable |
| `ErrRequestRejected` | `INVALID_ARGUMENT` | `400` | permanent |
| `ErrScopeNotPermitted` | `INVALID_ARGUMENT` | `400` | permanent |
| `ErrJournalRejected` | `INVALID_ARGUMENT` | `400` | permanent |
| `journal.ErrDuplicateConflict` | `INVALID_ARGUMENT` | `400` | permanent |
| unauthenticated caller | `UNAUTHENTICATED` | `401` | permanent |
| unsupported media or encoding | n/a | `415` | permanent |
| body over the compressed limit | `RESOURCE_EXHAUSTED` | `400` | permanent |

The oversized-request row is the one code this service does not choose: a frame
over `-max-compressed-bytes` is rejected by the gRPC transport itself before any
handler runs. On HTTP the limit is enforced while reading, so an oversized body
never becomes an oversized allocation, and it is permanent because the same body
is the same size on every retry.

`ErrScopeNotPermitted` cannot be produced by this receiver today: the envelope's
region is the replica's configured region by construction, so the two always
agree. The mapping is defined and tested anyway, because the coordinator's
contract includes it and a second source adapter or a per-principal envelope
table would make it reachable.

An unrecognized failure class is a defect, not a verdict about the batch.
Calling it permanent would discard data on the strength of a bug, so it is
retryable and logged at error level.

HTTP failures carry a `google.rpc.Status` message with `application/x-protobuf`,
so a Collector reads the same categorical failure it would have received over
gRPC.

### Partial success

Record-local rejections become `ExportLogsPartialSuccess` with
`rejected_log_records` set to the number of rejected records and an
`error_message` composed only from this service's own categorical rejection
reasons, for example `3 log records rejected: claim_not_permitted=2
nesting_too_deep=1`. The message never reads record content. It is put through
the configured redaction policy's text validation before it leaves the process,
and replaced with a fixed safe string if that validation ever fails.

This is how architecture.md's requirement is met that permanently malformed
records must not cause an otherwise valid batch to retry forever: the Collector
drops exactly the rejected records and keeps the rest.

## The trusted envelope

The envelope is derived from the authenticated transport identity and from
configuration. It is NEVER derived from application-supplied attributes.

Two structural properties enforce this rather than a convention:

- The trust source is a closed enumeration with no member meaning "from the
  payload", and an unknown value is refused at startup.
- The authenticator's only input is a `Peer`, which carries the TLS connection
  state and the remote address. No request body is reachable from it, so no
  implementation can consult the payload even by mistake.

The receiver copies the allowed-environment and allowed-service sets into every
envelope. Sharing the backing arrays would let one admitted request mutate the
set every later request is authorized against.

`-otlp-trust-source` has no default. A receiver that defaulted would be an
unauthenticated receiver nobody chose.

### What authenticates a caller today

`mutual_tls` is the only mode that authenticates anything. The listener is built
with `RequireAndVerifyClientCert` against the configured client CA, and the
envelope's caller identity is read from the verified chain: `source_instance`
from the leaf certificate's first DNS SAN and `credential_identity` from its
first URI SAN. `PeerCertificates` is deliberately not consulted; only
`VerifiedChains` distinguishes a certificate a CA vouched for from one the
caller merely presented. A certificate with neither SAN is refused: without a
stable machine identity it cannot be paired with a record UID to make identity
unforgeable across sources. Under `mutual_tls` the operator MUST NOT declare
`-otlp-source-instance` or `-otlp-credential-identity`; two disagreeing sources
of one truth is the ambiguity the envelope exists to remove.

`static_local` authenticates nothing. Every caller that can reach the socket
receives the one configured envelope. It exists for local development, where
there is no certificate authority and the OpenTelemetry demo Collector exports
over plaintext OTLP. It must be selected explicitly, so an unauthenticated
receiver is always something an operator chose. It refuses TLS material, which
it would never verify and which would only look like protection.

### What is deferred

- Workload identity (SPIFFE/SPIRE or a cloud workload identity) as an
  alternative to certificate SANs. identity-and-admission.md permits either;
  only certificates are implemented.
- Per-principal envelopes. Today one receiver serves one source account with one
  allowed-environment and allowed-service set. Several Collectors with different
  allow-sets need a principals table keyed by credential identity.
- Certificate revocation checking and rotation choreography.
- Authorization beyond authentication: every authenticated caller may claim
  anything inside the configured allow-sets.
- Local development runs `static_local`, which means local development has no
  ingress authentication at all. This is stated plainly rather than described as
  a weaker form of authentication, because it is not authentication.

## Process worker

The `process` role runs one loop. It never runs two cycles at once and never
runs two back to back:

```text
Process(cohort)
  -> failed:      pause backoff, doubling from -process-backoff-min to -process-backoff-max
  -> did work:    pause -process-interval
  -> claimed none: pause -process-idle-interval
```

An idle journal is the normal state between bursts, so a worker with nothing to
claim waits rather than spinning a core and hammering Pebble. Backoff is reset
by any cycle that does not fail, so one transient failure does not slow the
worker for the next several cycles. A cycle that only quarantined records
counts as work: it changed durable state, and there may be more like it.

The cycle's context is not derived from the process signal context. A cycle
inside a CockroachDB transaction is cancelled only after the shutdown budget is
spent.

## Shutdown

`SIGINT` or `SIGTERM` begins a graceful shutdown with the `-shutdown-timeout`
budget, in this order:

1. Stop accepting new exports and drain the admitted ones. gRPC uses
   `GracefulStop` and HTTP uses `Shutdown`. Every admitted handler is inside
   `Ingest`, so draining is draining through journal commit exactly as
   operations.md requires.
2. Stop claiming new work and let the cycle in flight finish.
3. Close the database connections that cycle was using.
4. Close the journal.

The journal is last because everything above it can still need it.

If the budget expires, the stop becomes forced: gRPC is stopped outright and the
active process cycle is cancelled. A forced stop remains safe. An acknowledged
record was synchronized before its response was written, and an abandoned
cohort's claims lapse and are claimed again, because nothing is committed in the
journal until after the database transaction returns. A forced stop does not
wait for an unresponsive database, which would let the database hold the process
open past the deadline that exists for exactly that case.

Step 3 is what makes that last sentence true, and it needs its own bound rather
than inheriting one. `pgxpool.Close` waits for every acquired connection to be
returned, so during the outage this service is required to survive it can block
indefinitely. Closing it inline would strand the whole shutdown there — and
because the journal is closed after it, the replica would be `SIGKILL`ed with
Pebble still open. So the pool close is given the remaining budget and abandoned
if it overruns: whatever it still holds is released when the process exits,
which is the next thing to happen, and step 4 always runs.

`Shutdown` is idempotent and reports the same result to every caller.

## Deliberately not in this slice

Since this document was first written the outbox publisher, its Amazon SQS
Standard transport, and the CloudWatch adapter have all been built. The `outbox`
role is now reachable end to end: `openTransport` builds `internal/outbox/sqsaws`,
whose classification of every SQS failure into retryable, message-rejected, and
deployment-fault is verified against a real SQS API.

`internal/cloudwatch` is complete within its own boundary but is still wired to
nothing: no role runs it and its `Sink` has no implementation. See
cloudwatch-source.md, which is cited as normative by that package and has not
been written.

Still genuinely absent: Collector persistent-queue configuration, capacity
shedding wired to live journal utilization, the metrics and audit-event
subsystems operations.md requires, health and readiness endpoints, and replica
drain choreography.
