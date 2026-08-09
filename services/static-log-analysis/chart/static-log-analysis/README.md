# Static Log Analysis Helm chart

This chart installs the safe production topology:

- one or more OpenTelemetry Collector gateway replicas, each with a persistent
  pre-redaction sending queue;
- the combined `all` role as a StatefulSet, with one non-shared Pebble journal
  PVC per replica;
- the `outbox` role as a stateless Deployment; and
- an optional combined CloudWatch polling-and-processing StatefulSet with
  separate journal and checkpoint PVCs; and
- an idempotent migration Job run as a Helm pre-install/pre-upgrade hook.

It deliberately does not install CockroachDB, certificates, an agent
orchestrator, or the unsafe split `ingest`, source-only, and `process` topology.

## Prerequisites

- An EKS cluster with EC2 worker nodes and the EBS CSI driver.
- A `ReadWriteOnce` storage class such as `gp3`.
- A managed, region-compatible CockroachDB cluster.
- The resources from `infra/aws`, or equivalent regional SQS queues and IAM.
- EKS Pod Identity configured for the outbox service account.
- Helm 3.17 or newer.

Create the namespace before creating secrets:

```bash
kubectl create namespace static-log-analysis
```

Create or synchronize these secrets in that namespace:

| Secret | Required keys | Purpose |
|---|---|---|
| `static-log-analysis-database` | `dsn` | Provider-issued CockroachDB TLS DSN |
| `static-log-analysis-server-tls` | `tls.crt`, `tls.key`, `ca.crt` | Analysis server identity and trusted Collector-client CA |
| `static-log-analysis-collector-server-tls` | `tls.crt`, `tls.key`, `ca.crt` | Collector server identity and trusted project-client CA |
| `static-log-analysis-collector-client-tls` | `tls.crt`, `tls.key`, `ca.crt` | Collector client identity and trusted analysis-server CA |

The analysis certificate needs a DNS SAN matching the generated analysis
Service name. With the documented release name that is `static-log-analysis`.
Set `tls.analysisServerName` when using a different certificate name. The
Collector server certificate must match the DNS name project collectors use.

Do not commit any of these secret values. In production, synchronize them from
your approved secrets manager rather than creating long-lived plaintext files.

## Install

Copy and edit the example values. In particular, replace the image, services,
environments, region, tenant, queue URLs, storage class, and measured storage
sizes:

```bash
cp chart/static-log-analysis/values-production.example.yaml production-values.yaml
helm lint chart/static-log-analysis -f production-values.yaml
helm upgrade --install static-log-analysis chart/static-log-analysis \
  --namespace static-log-analysis \
  --values production-values.yaml \
  --atomic --wait --timeout 10m
```

The values schema intentionally rejects the empty defaults. An operator must
choose the regional identity boundary and exact admission allowsets.

## Pull CloudWatch logs

Enable the combined role only when the matching `infra/aws.cloudwatch_sources`
map has been applied:

```yaml
cloudwatch:
  enabled: true
  accountID: "111122223333"
  credentialIdentity: arn:aws:iam::111122223333:role/static-log-analysis-cloudwatch
  sources:
    - logGroup: /aws/ecs/application/api
      service: api
      environment: production
```

The chart creates one StatefulSet replica with one journal PVC and one
checkpoint PVC. The replica pulls every configured log group and processes the
records against that same journal; it binds only the admin listener. The chart
intentionally rejects more than one CloudWatch replica because its journal and
checkpoints are local. Sharding source sets across independent replicas is not
yet a supported chart topology; it needs component-level release controls and
disjoint checkpoint ownership before it can be enabled safely.

## Connect a project in the same cluster

Keep each application pointed at its existing Collector or Grafana Alloy. Add a
logs-only OTLP exporter to the gateway Service:

```yaml
exporters:
  otlp/static_log_analysis:
    endpoint: static-log-analysis-collector.static-log-analysis.svc.cluster.local:4317
    tls:
      ca_file: /tls/static-log-analysis/ca.crt
      cert_file: /tls/static-log-analysis/tls.crt
      key_file: /tls/static-log-analysis/tls.key
      server_name_override: static-log-analysis-collector.static-log-analysis.svc.cluster.local

service:
  pipelines:
    logs/static_log_analysis:
      receivers: [otlp]
      processors: [resource/project_identity, batch]
      exporters: [otlp/static_log_analysis]
```

The branch must set `service.name` and `deployment.environment.name`. The exact
values must appear in `allowedServices` and `allowedEnvironments`. Preserve the
project's existing pipelines and do not feed Collector self-telemetry back into
this branch.

For a project outside the cluster, set `collector.service.type` and the
platform-specific annotations for a private internal load balancer. Never
expose the gateway publicly; mutual TLS remains required either way.

## Verify

```bash
kubectl -n static-log-analysis get jobs,pods,pvc,services
kubectl -n static-log-analysis rollout status statefulset/static-log-analysis
kubectl -n static-log-analysis rollout status deployment/static-log-analysis-outbox
kubectl -n static-log-analysis port-forward service/static-log-analysis 9464:9464
```

In another shell:

```bash
curl -fsS http://127.0.0.1:9464/readyz
curl -fsS http://127.0.0.1:9464/metrics
```

Send five matching synthetic errors through an admitted project service within
five minutes, then check queue depth without receiving a message:

```bash
aws sqs get-queue-attributes \
  --queue-url "$ASSIGNMENT_QUEUE_URL" \
  --attribute-names ApproximateNumberOfMessages ApproximateNumberOfMessagesNotVisible
```

The agent consumer must use the assignment's stable message/investigation ID as
an idempotency key. SQS Standard delivery may repeat a message.
