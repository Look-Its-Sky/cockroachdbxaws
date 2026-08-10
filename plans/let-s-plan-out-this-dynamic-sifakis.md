# SQS-driven investigations: a background consumer beside the HTTP API

## Context

Today the only way to start an investigation is a synchronous `POST /agent`
(`agent_space/routes/private.go:116`), which blocks the caller for the length of the
run — measured at ~66s, with observed variance up to 135s. That is fragile in front of
any proxy, and it means an upstream detector has no way to hand work over.

The `static-log-analysis` producer publishes `agent.assignment.v1` messages to SQS. We
want the same agent loop driven off that queue, on its own goroutine, so investigations
start without an HTTP caller and survive the caller going away.

The important detail in the sample message: **the body is a pointer, not a payload.** It
carries `incident_id`, `investigation_id`, `context_version`, `service_id`, `environment`,
`severity` — but no prose. `agent.Runner.Run(ctx, question, limit)` needs prose. So the
consumer's real job is *resolve → run → record*, not just *receive → run*.

Decisions taken (confirmed with the user):

- Incident prose is **fetched from CockroachDB**, keyed by `incident_id` + `context_version`.
- Verdicts are **held in-process** and read back via `GET /agent/:id`.
- Dev runs against **LocalStack**; the same code path points at real SQS later via config only.
- The loop is **serial**, with a visibility heartbeat and dedup on `deduplication_key`.

## What exists and gets reused

| Thing | Where | How it's used |
| --- | --- | --- |
| `agent.Runner.Run` | `agent_space/agent/runner.go:104` | Unchanged. The worker is just another caller. |
| `agent.Result` / `Step` | `agent_space/agent/trace.go:30` | The JSON the polling route returns. |
| `mcp.ConfigFromEnv` / `Configured()` | `agent_space/utils/mcp/client.go:57` | Config convention to copy exactly: env-driven struct, `Configured()` gate, unconfigured means the feature is off, never fatal. |
| `utils.EnvOr` / `utils.EnvBool` | `agent_space/utils/llm.go:62` | All new env reads. |
| `utils.RequireToken()` | `agent_space/utils/auth.go:17` | Guards the new polling route. |
| `mcptest` in-process server | `agent_space/utils/mcp/mcptest/server.go` | The pattern for the fake SQS used in worker tests. |
| pgx pool built in `initStore` | `agent_space/initialization.go:45` | Hoisted to a package var so the incident resolver shares one pool. |
| `scriptedModel` | `agent_space/agent/runner_test.go:21` | Same shape re-declared in `worker_test.go` (it's unexported in `package agent`). |

## Plan

### 1. Message and queue plumbing — `agent_space/utils/queue/`

`message.go` — the envelope, decoded from the SQS `Body` string:

```go
type Assignment struct {
    SchemaVersion  string    `json:"schema_version"`
    MessageID      string    `json:"message_id"`
    MessageType    string    `json:"message_type"`
    CreatedAt      time.Time `json:"created_at"`
    Region         string    `json:"region"`
    TenantID       string    `json:"tenant_id"`
    Classification string    `json:"classification"`
    Producer       string    `json:"producer"`
    CorrelationID  string    `json:"correlation_id"`
    IncidentID     string    `json:"incident_id"`
    IncidentGen    int64     `json:"incident_generation"`
    InvestigationID string   `json:"investigation_id"`
    ServiceID      string    `json:"service_id"`
    Environment    string    `json:"environment"`
    Severity       string    `json:"severity"`
    ContextVersion int       `json:"context_version"`
}
```

`ParseAssignment(body string) (Assignment, error)` rejects anything whose `message_type`
is not `agent.assignment.v1`, or that is missing `incident_id` / `investigation_id`.
Unknown `message_type` and bad JSON are **permanent** failures — retrying cannot fix them
— and are distinguished from transient ones with a sentinel `ErrPermanent`, following the
`ErrTransport` / `IsSessionFailure` precedent in `agent/trace.go:27`.

`sqs.go` — a narrow interface so tests never touch AWS:

```go
type Receiver interface {
    Receive(ctx context.Context) ([]Message, error)
    Delete(ctx context.Context, receiptHandle string) error
    ExtendVisibility(ctx context.Context, receiptHandle string, seconds int32) error
}
```

with `Message{ MessageID, ReceiptHandle, Body string; Attributes, MessageAttributes map[string]string }`.
The real implementation wraps `aws-sdk-go-v2/service/sqs`, requesting `MessageAttributeNames: ["All"]`
and `MessageSystemAttributeNames: ["ApproximateReceiveCount"]` — the receive count drives
the give-up rule below.

`config.go` — `ConfigFromEnv()` + `Configured()`:

| Var | Default | Meaning |
| --- | --- | --- |
| `SQS_QUEUE_URL` | *(empty)* | Empty ⇒ worker never starts. The whole feature gate. |
| `AWS_ENDPOINT_URL` | *(empty)* | Set ⇒ LocalStack (`BaseEndpoint` override). Unset ⇒ real AWS. |
| `AWS_REGION` | `us-east-1` | |
| `WORKER_VISIBILITY_TIMEOUT` | `120s` | Also the heartbeat extension amount. |
| `WORKER_WAIT_TIME` | `20s` | Long-poll duration. |
| `WORKER_MAX_ATTEMPTS` | `5` | Compared against `ApproximateReceiveCount`. |

Credentials come from the standard chain (`config.LoadDefaultConfig`), so LocalStack just
needs the dummy `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` in `.env`.

New module deps: `github.com/aws/aws-sdk-go-v2/config` and `.../service/sqs`.

### 2. Incident context resolver — `agent_space/incident/store.go`

New table, appended to `agent_space/scripts/seed-cluster.sql`. No FK to `services` — the
producer names services we may not know about:

```sql
CREATE TABLE incident_context (
    incident_id     STRING NOT NULL,
    context_version INT NOT NULL,
    service_id      STRING NOT NULL,
    environment     STRING NOT NULL,
    severity        STRING NOT NULL,
    summary         STRING NOT NULL,
    log_excerpt     STRING NOT NULL,
    detected_at     TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (incident_id, context_version)
);
```

```go
type Resolver struct{ Pool *pgxpool.Pool }
func (r *Resolver) Resolve(ctx context.Context, a queue.Assignment) (Context, error)
```

Selects on `(incident_id, context_version)`; `pgx.ErrNoRows` becomes `ErrNotFound`, which
the worker treats as **transient** (the producer may not have committed the row yet).

`BuildQuestion(a queue.Assignment, c incident.Context) string` renders the prose the
Runner expects — service, environment, severity, detection time, summary, log excerpt,
then the standing ask ("identify the commit, decide ROLLBACK or HOTFIX"). Keep it in
`incident/`, not `agent/`: the Runner's system prompt already owns the decision framing
(`agent/runner.go:30`) and should not be duplicated.

**Seed additions** so the exact sample message resolves end-to-end: a `payment` service
row, two `deploys` rows for it with one obvious culprit, `request_latency` rows straddling
that deploy, and an `incident_context` row for `6424cd11…b5ed` at `context_version = 1`.
The existing `checkout` / `a91f3c2` story is left untouched — `scripts/demo.sh` depends on it.

### 3. The worker — `agent_space/worker/`

`results.go` — in-memory store keyed by `investigation_id`:

```go
type Record struct {
    InvestigationID string       `json:"investigation_id"`
    IncidentID      string       `json:"incident_id"`
    CorrelationID   string       `json:"correlation_id"`
    Status          Status       `json:"status"` // running | done | failed
    Result          agent.Result `json:"result,omitempty"`
    Error           string       `json:"error,omitempty"`
    StartedAt       time.Time    `json:"started_at"`
    FinishedAt      time.Time    `json:"finished_at,omitempty"`
}
```

Mutex-guarded map with a bounded size (drop oldest past ~200 entries). This is deliberately
non-durable: a restart loses verdicts and re-runs redelivered messages. Say so in the doc
comment rather than implying otherwise — promoting it to a CockroachDB table is a later,
independent change.

`worker.go` — the loop:

```
for ctx not cancelled:
  msgs := Receive(max=1, waitTime=20s)          // long poll, so an idle queue costs nothing
  for each msg:
    a, err := ParseAssignment(msg.Body)
    if errors.Is(err, ErrPermanent):             // poison pill
        record failed; Delete; continue
    if results.Seen(a.InvestigationID):          // dedup, incl. deduplication_key attribute
        Delete; continue
    if ApproximateReceiveCount >= MAX_ATTEMPTS:
        record failed("gave up after N deliveries"); Delete; continue

    stop := heartbeat(msg.ReceiptHandle)         // ExtendVisibility every timeout/3
    inc, err := resolver.Resolve(ctx, a)
    if ErrNotFound: stop(); leave undeleted      // producer hasn't committed yet — redelivery retries
    results.Start(a)
    res, err := runner.Run(runCtx, BuildQuestion(a, inc), 0)
    stop()
    results.Finish(a, res, err)
    Delete(msg.ReceiptHandle)                    // delete on completion AND on agent error
```

Points that matter:

- **`runCtx` is derived from the worker's context, never a request context.** The bug in
  NEXT.md §2 — `/agent` cancelling because a client disconnected — must not be reproduced
  here. Give it its own timeout (`WORKER_RUN_TIMEOUT`, default 10m).
- **The heartbeat is what makes a 66s run safe.** Without it, a 30s queue visibility
  timeout redelivers mid-run and the same investigation runs three times on a billable key
  — which is exactly what `ApproximateReceiveCount: "3"` in the sample message shows.
- **Agent errors delete the message.** A failing LLM call re-run five times costs money and
  produces the same failure. The verdict is recorded as `failed` and readable via the route.
  `ErrTransport` (MCP down) is the one exception worth *not* deleting on — that is genuinely
  transient — so branch on `errors.Is(err, agent.ErrTransport)`.
- Serial by construction: one `Receive` with `MaxNumberOfMessages: 1`, no goroutine pool.

### 4. Wiring — `initialization.go`, `main.go`, `routes/`

- `initialization.go`: hoist the pgx pool to a package var (`pool`), add `initWorker()`
  following `initMCP()`'s shape — log and return if `!cfg.Configured()` or if `sreAgent == nil`,
  never `log.Fatal`.
- `main.go`: replace the bare `router.Run` with an `http.Server` plus `signal.NotifyContext`,
  start the worker with `go w.Run(ctx)`, and on shutdown cancel that context and call
  `srv.Shutdown`. This also makes the existing `defer mcpSess.Close()` (`main.go:19`) actually
  run — today `router.Run` blocks until fatal and the defer never fires.
- `routes/public.go`: add `var Results *worker.Store`.
- `routes/private.go`: add `GET /agent/:id` → 503 if `Results == nil`, 404 if unknown, else
  the `Record` as JSON. Gin routes per method, so `POST /agent` and `GET /agent/:id` coexist.

### 5. Local stack — `docker-compose.local.yml`, `.env.example`

```yaml
localstack:
  image: localstack/localstack:4
  environment:
    SERVICES: sqs
  ports: ["4566:4566"]
  volumes:
    - ./docker/localstack-init:/etc/localstack/init/ready.d:ro
```

`docker/localstack-init/01-create-queues.sh` (mirrors the existing `docker/postgres-init`
convention) creates `static-log-analysis` and a `static-log-analysis-dlq`, with a redrive
policy of `maxReceiveCount: 5` and `VisibilityTimeout: 120`. Queue URL then matches the
sample ARN: `http://localhost:4566/000000000000/static-log-analysis`.

`.env.example` gets a `─── SQS / worker ───` block in the file's existing commented style,
stating plainly that an empty `SQS_QUEUE_URL` means the worker never starts and the HTTP
API is unaffected.

`agent_space/scripts/enqueue.sh` — small helper that sends the sample assignment (IDs
overridable by flag) so the demo is one command.

## Testing

New unit tests, no network and no credentials, matching the existing suite's constraints:

- `utils/queue/message_test.go` — parses the **exact** sample body from the request; rejects
  wrong `message_type`, bad JSON, missing IDs, each as `ErrPermanent`.
- `worker/worker_test.go` against a fake in-memory `Receiver` and a scripted `llms.Model`
  (copy `scriptedModel` from `agent/runner_test.go:21`) with `mcptest` tools:
  - happy path → agent ran once, message deleted, `Record.Status == done`
  - missing incident context → **not** deleted, no agent run, nothing recorded
  - malformed body → deleted, no agent run, recorded as failed
  - duplicate delivery of a completed investigation → deleted, agent not re-run
  - slow run → `ExtendVisibility` called at least twice
  - `ApproximateReceiveCount >= max` → deleted, recorded as failed
- `incident/store_test.go` — `BuildQuestion` covers all envelope fields (pure function, no DB).

## Verification

```bash
docker compose -f docker-compose.local.yml up -d          # crdb + mcp + localstack
cd agent_space
go run ./cmd/seed                                          # creates incident_context + payment rows
./scripts/test.sh -r                                       # fmt, build, vet, race

go run .                                                   # look for: "Worker: polling <queue url>"
./scripts/enqueue.sh                                       # sends the sample assignment
curl -s localhost:8080/agent/019fe42e-18e1-7936-8051-ce2536637167 \
     -H "X-Agent-Token: $API_TOKEN" | jq
#   → status "running", then "done" with answer + trace
```

Negative checks worth doing by hand, since they are the ones that bite in production:

1. `aws --endpoint-url=http://localhost:4566 sqs send-message` with an unknown
   `incident_id` → message stays invisible, retries, lands in the DLQ after 5 deliveries;
   no LLM tokens spent.
2. Send a body of `not json` → deleted immediately, one log line, no retry storm.
3. Send the same assignment twice → agent runs once.
4. `aws sqs get-queue-attributes … ApproximateNumberOfMessagesNotVisible` during a run →
   stays 1 for the full ~66s, proving the heartbeat holds the lease.
5. Unset `SQS_QUEUE_URL`, restart → server boots, logs that the worker is disabled, all
   existing routes behave as before.
