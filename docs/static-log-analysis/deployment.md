# Deployment

## Local development stack

The service directory contains a self-contained Docker Compose deployment. It
uses the safe current topology: the combined `all` role owns ingestion and its
non-shared journal, while a separate `outbox` role publishes elected
investigations to SQS.

From the repository root:

```bash
cd services/static-log-analysis
docker compose up --build -d --wait
```

No environment file is required for the example service. For a real repository,
copy the example and declare its exact service and environment admission sets:

```bash
cp .env.example .env
docker compose up --build -d --wait
```

`SLA_ALLOWED_SERVICES` and `SLA_ALLOWED_ENVIRONMENTS` are security boundaries,
not discovery filters. There is deliberately no wildcard. A record claiming an
undeclared identity is rejected rather than silently attributed to the wrong
service or environment.

The local stack contains:

| Service | Purpose | Host endpoint |
|---|---|---|
| OpenTelemetry Collector | application and Alloy log entry point, persistent transport queue, first-pass redaction | gRPC `localhost:4317`, HTTP `localhost:4318` |
| Static Log Analysis `all` | durable journal, normalization, rules, grouping, CockroachDB persistence | admin `http://localhost:9464` |
| Static Log Analysis `outbox` | transactional-outbox delivery to SQS | admin `http://localhost:9465` |
| CockroachDB | incidents, investigations, contexts, and outbox | SQL `localhost:26257`, console `http://localhost:18080` |
| LocalStack SQS | local assignment and dead-letter queues | `http://localhost:4566` |

The one-shot `cockroach-init` and `migrate` containers finish before either
application process starts. `log-analysis-migrate` uses the embedded reviewed
migrations, verifies their checksums and the live schema, and is idempotent.

Health and logs:

```bash
curl -fsS http://localhost:9464/healthz
curl -fsS http://localhost:9464/readyz
curl -fsS http://localhost:9465/readyz
docker compose ps
docker compose logs -f static-log-analysis outbox otel-collector
```

`docker compose down` stops the stack but preserves the CockroachDB, analysis
journal, and Collector queue volumes. To deliberately erase this local data:

```bash
docker compose down --volumes
```

That reset is destructive and cannot recover queued logs or local incidents.

## Plug a Docker Compose repository into the service

The deployment creates a stable Docker network named
`static-log-analysis-ingress`. The Collector has the stable alias
`static-log-analysis-collector` on that network. Those two names are the public
cross-repository contract; generated Compose project names and container names
are not.

1. Set the monitored identities in this service's `.env`:

   ```dotenv
   SLA_ALLOWED_ENVIRONMENTS=development,staging,production
   SLA_ALLOWED_SERVICES=api,worker,checkout
   ```

2. Start Static Log Analysis first so the shared network exists:

   ```bash
   cd services/static-log-analysis
   docker compose up --build -d --wait
   ```

3. Generate an overlay for an instrumented service in the application
   repository. The arguments are its Compose service key, OpenTelemetry logical
   service name, and deployment environment:

   ```bash
   ./scripts/render-compose-integration.sh api checkout development \
     > /path/to/application/compose.static-log-analysis.yaml
   ```

4. Add that overlay last when starting the application:

   ```bash
   cd /path/to/application
   docker compose -f compose.yaml -f compose.static-log-analysis.yaml up -d
   ```

The generated file sets the standard OTLP/HTTP environment variables and joins
only the named application service to the shared network. The application still
needs an OpenTelemetry logging SDK or auto-instrumentation capable of exporting
logs; environment variables cannot add instrumentation to an application that
has none.

Generate one overlay service entry per application container that exports logs.
All logical `OTEL_SERVICE_NAME` values must be in `SLA_ALLOWED_SERVICES`, and the
environment must be in `SLA_ALLOWED_ENVIRONMENTS`.

### Repository already has a Collector or Grafana Alloy

Keep applications pointed at their existing collector. Join that collector to
the external network and add a logs-only OTLP exporter whose endpoint is:

```text
static-log-analysis-collector:4317
```

Preserve every existing receiver, processor, and exporter; prefer a separate
named logs pipeline for the new exporter so configuration merging cannot replace
the application pipeline's exporter array. Filter
`service.name=otelcol-contrib` from the Static Log Analysis branch, and never
point the Collector's internal telemetry exporter at the OTLP receiver feeding
that branch: doing so creates a feedback loop. Do not give the upstream
collector a persistent queue containing raw logs unless it runs an equivalent
universal redaction pass before that queue. The Static Log Analysis Collector
performs mandatory first-pass redaction before its own persistent sending queue.

For Grafana Alloy, the equivalent exporter endpoint is
`static-log-analysis-collector:4317` using OTLP gRPC. Alloy must also join the
same external Docker network.

### Host processes and non-Compose environments

Applications running on the host send OTLP logs to either:

```text
gRPC: localhost:4317
HTTP: http://localhost:4318/v1/logs
```

A non-Compose container can use the published host port. Set
`SLA_OTLP_BIND_ADDRESS=0.0.0.0` only when a firewall or deployment network
provides the intended boundary; the safe local default is loopback. Do not send
directly to the analysis container: the Collector performs mandatory first-pass
redaction before its persistent queue.

Every producer must set `service.name`; the generated overlay does this through
`OTEL_SERVICE_NAME`. It should also set `deployment.environment.name`. A missing
environment is accepted only when the configured environment allow-set has
exactly one member.

A producer that can create a canonical UUIDv7 `log.record.uid` before its first
export should do so for the strongest deduplication. Repositories that cannot do
that are still admitted through deterministic `derived:v1` identity computed
from already-redacted stable evidence. This fallback is visible as
`static_log_analysis_derived_identity_total`; alert on sustained use and migrate
high-volume producers to native IDs. A malformed UID is rejected rather than
silently falling back.

### End-to-end smoke test

The local smoke script sends five matching ERROR records without native UIDs,
exercising OTLP, first-pass redaction, the persistent Collector queue, derived
identity, the journal, incident election, the outbox, and SQS:

```bash
./scripts/send-smoke-errors.sh checkout development
```

The service and environment arguments must be in the configured allow sets.
Then inspect metrics and the local assignment queue:

```bash
curl -fsS http://localhost:9464/metrics | grep static_log_analysis

docker compose exec localstack awslocal sqs receive-message \
  --queue-url http://localhost:4566/queue/us-east-1/000000000000/static-log-analysis \
  --message-attribute-names All \
  --max-number-of-messages 10
```

### Inspect local assignments

The deterministic service creates an assignment only after a configured rule
elects an investigation. For the current ordinary-error rule this requires five
matching errors in five minutes.

```bash
docker compose exec localstack awslocal sqs receive-message \
  --queue-url http://localhost:4566/queue/us-east-1/000000000000/static-log-analysis \
  --message-attribute-names All \
  --max-number-of-messages 10
```

The agent orchestrator is intentionally not part of this stack yet. It will be
a separate SQS consumer, so an assignment remains available on the queue until a
consumer claims it.

## Deployment-environment contract

Use one regional Static Log Analysis deployment for an explicit tenant, region,
service allow-set, and environment allow-set. Development, staging, and
production may share an installation only when that grouping is intentional;
environment remains part of incident identity, so their failures do not merge.

| Setting | Meaning |
|---|---|
| `SLA_REGION` | Hard regional data boundary; must match regional storage and SQS |
| `SLA_TENANT_ID` | Single-tenant scope for this deployment |
| `SLA_ALLOWED_ENVIRONMENTS` | Exact environment claims this ingress may accept |
| `SLA_ALLOWED_SERVICES` | Exact service claims this ingress may accept |
| `SLA_INGRESS_NETWORK` | Stable local cross-repository Docker network |
| `SLA_OTLP_BIND_ADDRESS` | Host bind address; loopback by default |
| `SLA_SOURCE_*` | Local trusted-envelope audit identity |

Changing the tenant, region, classification, or redaction policy on an existing
journal is not an environment switch: those are immutable storage boundaries and
startup correctly fails if they disagree. Use a distinct volume/deployment.

## Build the deployable image

The image is independent of the local dependencies:

```bash
docker build -t static-log-analysis:local .
```

It runs as UID/GID `10001:10001`, embeds both the service and reviewed migration
utility, and expects a writable journal volume at
`/var/lib/static-log-analysis/journal` for roles that own a journal.

Run migrations as a deployment init job before rolling out `all` or `outbox`:

```bash
docker run --rm \
  --entrypoint /usr/local/bin/log-analysis-migrate \
  -e STATIC_LOG_ANALYSIS_DATABASE_DSN \
  static-log-analysis:local
```

Then run separate containers from the same image:

```text
/usr/local/bin/log-analysis all ...
/usr/local/bin/log-analysis outbox ...
```

Production provisions CockroachDB through its official provider; it does not run
the `cockroachdb` service from this Compose file. The provider-issued regional
TLS DSN is supplied first to the one-shot migration job and then only to the
`all` and `outbox` workloads. The service image neither creates nor manages a
production CockroachDB cluster. The provider configuration must select the
reviewed compatible CockroachDB version (currently v25.3.7); database upgrades
are tested and reviewed before that version changes.

Production MUST also replace every other local-only setting in `compose.yaml`:
use Amazon SQS with regional workload identity, OTLP mutual TLS, regional
persistent volumes, measured journal sizing, and the production allow sets.
LocalStack, CockroachDB `--insecure`, plaintext OTLP, and `static_local` trust are
development conveniences only.

Do not deploy separate `ingest`, `source`, and `process` replicas until the
non-shared journal handoff described in acceptance.md is resolved. Otherwise an
ingest/source replica can acknowledge work that no process replica owns.
