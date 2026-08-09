# Static Log Analysis Service

The Static Log Analysis Service receives OTLP logs, redacts sensitive content,
durably journals accepted records, groups related errors into incidents, and
publishes agent-investigation assignments to Amazon SQS. Detection and grouping
are deterministic; no LLM participates in log processing.

```text
Application or Alloy
  -> OpenTelemetry Collector
  -> Static Log Analysis
  -> CockroachDB transactional outbox
  -> Amazon SQS
  -> agent orchestrator
```

## Start the local deployment

From this directory:

```bash
cp .env.example .env
docker compose up --build -d --wait
```

At minimum, configure the exact identities this deployment may accept:

```dotenv
SLA_ALLOWED_ENVIRONMENTS=development,staging,production
SLA_ALLOWED_SERVICES=api,worker,checkout
```

These values are admission boundaries, not discovery filters. There is no
wildcard. A log claiming an undeclared service or environment is rejected
instead of being attributed incorrectly.

The local stack starts:

| Component | Local endpoint |
|---|---|
| OTLP/gRPC | `localhost:4317` |
| OTLP/HTTP | `http://localhost:4318/v1/logs` |
| Analysis health and metrics | `http://localhost:9464` |
| Outbox health | `http://localhost:9465` |
| CockroachDB SQL | `localhost:26257` |
| CockroachDB console | `http://localhost:18080` |
| LocalStack SQS | `http://localhost:4566` |

Check readiness:

```bash
curl -fsS http://localhost:9464/readyz
curl -fsS http://localhost:9465/readyz
docker compose ps
docker compose logs -f static-log-analysis outbox otel-collector
```

## Connect another Docker Compose repository

The deployment creates a stable network named
`static-log-analysis-ingress`. Its Collector is reachable from that network as
`static-log-analysis-collector`; integrations never depend on generated
container names.

Generate an overlay for each instrumented application service:

```bash
./scripts/render-compose-integration.sh \
  COMPOSE_SERVICE \
  OTEL_SERVICE_NAME \
  DEPLOYMENT_ENVIRONMENT \
  > /path/to/application/compose.static-log-analysis.yaml
```

For example:

```bash
./scripts/render-compose-integration.sh api checkout development \
  > /home/ali/Projects/example/compose.static-log-analysis.yaml
```

Start Static Log Analysis first so the shared network exists. Then start the
application with the generated overlay last:

```bash
cd /home/ali/Projects/example

docker compose \
  -f compose.yaml \
  -f compose.static-log-analysis.yaml \
  up -d
```

The generated overlay:

- joins only the named application container to the shared network;
- sets `OTEL_SERVICE_NAME` and `deployment.environment.name`;
- sends logs to `http://static-log-analysis-collector:4318` using
  OTLP/HTTP Protobuf.

The application must already use an OpenTelemetry logging SDK or compatible
auto-instrumentation. Environment variables configure an exporter; they cannot
instrument an application that does not produce OTLP logs.

## Connect an existing Collector or Grafana Alloy

If a repository already has a Collector or Alloy, keep its applications pointed
at that collector. Join the collector to the external ingress network:

```yaml
services:
  otel-collector:
    networks:
      - default
      - static-log-analysis-ingress

networks:
  static-log-analysis-ingress:
    external: true
    name: ${SLA_INGRESS_NETWORK:-static-log-analysis-ingress}
```

Add a logs-only exporter to its telemetry configuration:

```yaml
exporters:
  otlp_grpc/static_log_analysis:
    endpoint: static-log-analysis-collector:4317
    compression: gzip
    tls:
      insecure: true
    retry_on_failure:
      enabled: true
      initial_interval: 1s
      max_interval: 30s
      max_elapsed_time: 0

processors:
  filter/static_log_analysis:
    error_mode: ignore
    logs:
      log_record:
        - 'resource.attributes["service.name"] == "otelcol-contrib"'

service:
  pipelines:
    logs/static_log_analysis:
      receivers: [otlp]
      processors: [filter/static_log_analysis]
      exporters: [otlp_grpc/static_log_analysis]
```

The named pipeline leaves an existing `logs` pipeline intact. Reuse the
repository's actual receiver name and include its existing universal redaction
processors before this exporter when appropriate. Do not export the Collector's
own internal telemetry back into the receiver feeding this pipeline; that forms
a feedback loop. The filter is defense in depth, not a substitute for removing
that self-reference.

Do not add a persistent queue to the upstream collector unless equivalent
universal redaction runs before that queue. The Collector shipped with this
service removes common credentials before its own disk-backed sending queue.

The current OpenTelemetry Demo uses this same network-and-exporter contract; it
is not a special integration path.

## Record identity

The preferred producer identity is a canonical UUIDv7 in `log.record.uid`,
created before the first export attempt. This gives the strongest replay
deduplication.

Repositories that cannot provide one are still accepted using deterministic
`derived:v1` identity over already-redacted stable evidence. Its use is visible
through:

```text
static_log_analysis_derived_identity_total
```

A malformed supplied UID is rejected rather than silently switching identity
versions. High-volume production producers should eventually supply native
UUIDv7 identities.

## Timestamp compatibility

Some OTLP producers, including Envoy access-log exporters, provide event time
but omit observed time. The service deterministically uses event time as the
observed-time fallback and records
`observed_time_inference_reason=observed_time_missing_event_time_used`. A replay
therefore retains the same derived identity. Records missing both timestamps
are permanently rejected and counted by
`static_log_analysis_rejected_invalid_missing_timestamps_total`.

## Validate the complete local flow

The smoke script sends five matching ERROR records. It intentionally omits
native UIDs so it also exercises `derived:v1`:

```bash
./scripts/send-smoke-errors.sh checkout development
```

The service and environment must be present in `.env`. Allow several seconds
for the Collector batch and outbox worker, then inspect metrics:

```bash
curl -fsS http://localhost:9464/metrics |
  grep static_log_analysis
```

Inspect incidents, investigations, and outbox state:

```bash
docker compose exec cockroachdb cockroach sql \
  --insecure \
  --host=127.0.0.1:26257 \
  --database=static_log_analysis \
  --execute="
    SELECT service_id, environment, severity, detection_status,
           latest_occurrence, active_investigation_id
    FROM incident_families
    ORDER BY latest_occurrence DESC;

    SELECT investigation_id, state, trigger_reason, queued_at
    FROM investigations
    ORDER BY queued_at DESC;

    SELECT message_id, state, attempts, published_at, last_safe_error
    FROM outbox_messages
    ORDER BY created_at DESC;
  "
```

View assignments published to local SQS:

```bash
docker compose exec localstack awslocal sqs receive-message \
  --queue-url http://localhost:4566/queue/us-east-1/000000000000/static-log-analysis \
  --message-attribute-names All \
  --max-number-of-messages 10 \
  --visibility-timeout 0
```

Ordinary INFO logs do not create an assignment. The initial ordinary-error rule
requires five matching ERROR records within five minutes, which is why the smoke
script sends five.

## Stop or reset the local deployment

Stop containers while preserving CockroachDB, the journal, and Collector queue:

```bash
docker compose down
```

Deliberately erase all local durable state:

```bash
docker compose down --volumes
```

The volume reset permanently deletes queued logs and local incident history.

## Production deployment boundary

The image is deployable independently of the local dependencies:

```bash
docker build -t static-log-analysis:local .
```

Production uses the official managed CockroachDB provider rather than the local
`cockroachdb` Compose service. Supply the provider-issued regional TLS DSN to a
one-shot migration job and then to the analysis and outbox workloads through
`STATIC_LOG_ANALYSIS_DATABASE_DSN`.

Production must also replace LocalStack with regional Amazon SQS, plaintext
`static_local` OTLP trust with mutual TLS, and local volumes with measured
region-local persistent storage. Do not expose the local Compose deployment as
a production security model.

The production EKS package is in
[`chart/static-log-analysis`](chart/static-log-analysis/README.md). Regional
SQS queues and EKS Pod Identity wiring are in
[`infra/aws`](infra/aws/README.md). The package intentionally consumes an
existing EKS cluster, storage class, provider-managed CockroachDB TLS DSN, and
certificate secrets; it does not take ownership of those platform resources.

## Tests

Run commands from this directory:

```bash
gofmt -l .
go vet ./...
go vet -tags=integration ./...
./scripts/check-generated-proto.sh
./scripts/check-production-deployment.sh
go test ./...
REQUIRE_DOCKER=1 go test -p 1 -race -tags=integration ./...
```

For normative architecture, security, and operational detail, see the
[complete deployment guide](../../docs/static-log-analysis/deployment.md) and
[Static Log Analysis documentation](../../docs/static-log-analysis/README.md).
