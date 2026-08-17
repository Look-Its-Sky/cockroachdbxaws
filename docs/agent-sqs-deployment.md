# Agent assignment integration and deployment plan

**Status:** Integration implemented; infrastructure and App Runner deployment not yet applied

**Last reviewed:** 2026-08-16

**Release scope:** One `agent_space` investigation worker consumes committed
`agent.assignment.v1` messages from the static-analysis SQS queue. Automated
remediation and pull-request creation are out of scope for this release.

This document is safe for the public repository. Substitute placeholders only
in ignored operator files or the deployment shell. Do not commit account IDs,
ARNs, queue URLs, tenant IDs, database hosts, API tokens, or provider keys.

## 1. Deployment outcome

The first integrated release is:

```text
CloudWatch Logs
  -> static-log-analysis (EC2)
  -> CockroachDB static-analysis database
  -> transactional outbox
  -> SQS assignment queue
  -> agent_space (one App Runner instance)
       |-> read-only versioned analysis context
       |-> LLM + read-only CockroachDB MCP investigation
       +-> agent-owned verdict journal
```

App Runner is appropriate for this bounded release because the worker only
investigates and records verdicts. It is not a valid host for the current
remediation implementation, which requires a container runtime and an isolated
repository build boundary. The deployment script therefore forces
`REMEDIATION_ENABLED=false`; changing a local `.env` cannot enable it.

## 2. Contracts that are now implemented

### 2.1 Assignment admission

The consumer accepts no generic SQS JSON. It requires:

- at most 16 KiB and exactly one JSON object;
- no unknown fields or trailing JSON;
- schema `1.0`, type `agent.assignment.v1`, and producer
  `static-log-analysis`;
- valid UUIDv7 message, correlation, and investigation IDs;
- correlation ID equal to investigation ID;
- the exact SQS attributes `message_id`, `deduplication_key`, `message_type`,
  and `region`, with no additional attributes;
- body and transport identifiers that agree;
- region, tenant, and classification equal to the worker's trusted
  configuration; and
- the existing bounded classification, severity, timestamp, incident,
  generation, and context-version constraints.

A rejected envelope is not investigated. Structurally invalid, record-local
payloads are acknowledged because retry cannot repair them. A valid assignment
that conflicts with the worker's trusted region, tenant, or classification is
left for redelivery and eventual DLQ inspection; one bad deployment setting
must not silently delete every valid assignment.

### 2.2 Context access

The SQS payload remains a pointer. The agent resolves the immutable safe
snapshot using all of:

```text
region + tenant_id + investigation_id + incident_id + context_version
```

`ANALYSIS_DATABASE_URL` is a distinct read-only connection. The resolver joins
the analysis-owned `investigations`, `incident_families`, and
`investigation_contexts` tables, caps the encoded snapshot at 256 KiB, and
checks service, environment, severity, and classification against the admitted
assignment. It decodes the analysis service's `SafeValue` representation; it
does not read raw log storage.

`DATABASE_URL` remains the agent-owned writable database for vector data,
verdicts, remediation records, and engineer decisions. The process refuses to
start the queue worker if both URL strings are the same. The database operator
must still enforce separation with different databases and roles.

### 2.3 AWS identity

The static-analysis Terraform module now owns a separate App Runner instance
role. Its trust principal is `tasks.apprunner.amazonaws.com`. Its inline policy
permits only these actions against the exact assignment queue ARN:

- `sqs:ReceiveMessage`;
- `sqs:DeleteMessage`;
- `sqs:ChangeMessageVisibility`; and
- `sqs:GetQueueAttributes`.

The role cannot publish assignments, inspect the DLQ, read CloudWatch Logs, or
use wildcard SQS resources. The analysis host keeps its existing producer-only
permissions.

## 3. Required deployment inputs

Resolve these at deployment time; none belongs in Git:

| Input | Source | Requirement |
|---|---|---|
| `SQS_QUEUE_URL` | Terraform `assignment_queue_url` output | Exact queue in the configured region |
| `APPRUNNER_INSTANCE_ROLE_ARN` | Terraform `agent_runtime_role_arn` output | Runtime role, not the ECR access role |
| `APPRUNNER_ACCESS_ROLE_ARN` | Existing App Runner/ECR role | Image-pull role only |
| `AGENT_TENANT_ID` | Static-analysis operator configuration | Exact trusted tenant |
| `AGENT_CLASSIFICATION` | Static-analysis operator configuration | Exact trusted classification |
| `ANALYSIS_DATABASE_URL` | Database secret system | Read-only analysis context role |
| `DATABASE_URL` | Database secret system | Agent-owned writable database |
| `API_TOKEN` | Secret system | Required for the public HTTP API |
| LLM and MCP keys | Secret system | Required investigation dependencies |
| `CORS_ORIGINS` | Approved dashboard origin | Never `*` for the deployed service |

The checked-in `.env.example` names the settings, but the populated root
`.env` is ignored. Before use, confirm it is not staged:

```bash
git check-ignore -v .env
git status --short
```

The current shell deployer supplies secrets as App Runner runtime environment
configuration. This keeps them out of the image and repository, but principals
that can describe the service configuration may be able to retrieve them.
Moving the secret values to Secrets Manager references is required before this
is treated as a production-grade deployment.

## 4. Local integrated deployment

AWS is not required to exercise the assignment boundary. The local integration
uses the static-analysis Compose project for CockroachDB and LocalStack, then
joins it through `compose.local-integration.yaml`. Agent-owned tables live in a
separate `agent_space` database. The integration bootstrap creates an
`agent_context_reader` user with `SELECT` only on the three safe context tables;
a no-op write is part of the operator verification and must be denied.

First start static analysis:

```bash
docker compose --env-file services/static-log-analysis/.env \
  -f services/static-log-analysis/compose.yaml up -d --build
```

If an old local journal is intentionally retained and would make a disposable
smoke deployment replay for too long, select a new named journal without
deleting the old one. Use the same value on every subsequent command for that
Compose project:

```bash
SLA_ANALYSIS_JOURNAL_VOLUME=static-log-analysis_analysis-journal-agent-smoke \
docker compose --env-file services/static-log-analysis/.env \
  -f services/static-log-analysis/compose.yaml up -d
```

Copy and configure the agent provider file:

```bash
cp .env.agent-local.example .env.agent-local
```

Choose one real model path:

- set `OPENROUTER_API_KEY` and the OpenRouter model names; or
- set both `OPENAI_BASE_URL` and `EMBEDDING_BASE_URL` to OpenAI-compatible
  servers. From a container, a model on the host is addressed through
  `host.docker.internal`, which the integration Compose file maps explicitly.

The tested Ollama model pair, loopback-only Linux deployment, warm-up probes,
and evidence are in `docs/deployment/local-model-hosting.md`. If the host
firewall drops Docker bridge traffic to `host.docker.internal`, start the
database/MCP prerequisites above and use:

```bash
docker compose --env-file .env.agent-local \
  -f compose.local-models.yaml up -d --build
```

Then start the singleton investigation worker on loopback port 18081:

```bash
docker compose --env-file .env.agent-local \
  -f compose.local-integration.yaml up -d --build
```

The container deployment forces remediation off and never mounts the host
Docker socket. It proves detection, queueing, strict admission, safe context
resolution, investigation, and durable verdicts. Actual patch/build/test
remediation requires a separately reviewed host-side or sandbox runtime.

Generate a real assignment through the detector rather than hand-writing one:

```bash
services/static-log-analysis/scripts/send-smoke-errors.sh payment production
```

The default `example-service/development` smoke arguments may be outside a
deployment's configured admission allowlist; use a service and environment
listed in the local static-analysis configuration.

If CockroachDB reports that a retained volume was last used by a version too old
for the repository pin, do not delete it. Archive the volume, follow the
official supported intermediate-version upgrade path, verify `SHOW CLUSTER
SETTING version`, and only then restart the pinned image.

## 5. Database preparation

Use separate logical databases and roles even if both databases live in one
CockroachDB cluster:

```text
static_log_analysis   analysis owner/migrator; agent context reader
agent_space           agent owner/migrator
```

The context-reader role needs only database/schema connectivity and `SELECT`
on:

- `investigations`;
- `incident_families`; and
- `investigation_contexts`.

It must not receive write privileges, default privileges on future tables, or
access to raw/journal tables. Provision the user and password through the
database administration workflow, not through a committed SQL file. Verify the
role before deploying by connecting with its DSN and proving that the resolver
query succeeds while an `INSERT` is denied.

The agent database must be migrated before App Runner starts. A successful
analysis read does not prove the agent journal is writable, and vice versa.

## 6. Terraform change workflow

Use the existing account-derived workspace safety procedure in
`docs/static-log-analysis/deployment.md`. The expected new resources are:

```text
aws_iam_role.agent_runtime
aws_iam_role_policy.agent_runtime
```

The plan also exposes `agent_runtime_role_arn`. Before apply:

1. verify the active AWS account and explicit region;
2. select the account-specific workspace;
3. run the repository production deployment check;
4. inspect the plan for the two role resources and unrelated drift;
5. reject any replacement of the analysis EC2 host or SQS queues; and
6. apply only after recording the sanitized plan summary.

After apply, resolve the runtime values without copying them into this file:

```bash
terraform output -raw assignment_queue_url
terraform output -raw agent_runtime_role_arn
```

## 7. Agent deployment workflow

From the repository root, prepare the ignored `.env`, export the two App Runner
role ARNs, pin an immutable image tag (prefer the release commit SHA), and run:

```bash
AWS_REGION=<region> \
IMAGE_TAG=<release-sha> \
APPRUNNER_ACCESS_ROLE_ARN=<ecr-access-role-arn> \
APPRUNNER_INSTANCE_ROLE_ARN=<agent-runtime-role-arn> \
./agent_space/scripts/deploy.sh
```

The script fails before changing App Runner when required integration settings
are absent or both database URLs are identical. It creates or reuses an App
Runner auto-scaling configuration with minimum and maximum size `1`, attaches
the queue-consumer instance role on both create and update, and forces
remediation off.

Do not raise the maximum instance count yet. The current durable journal handles
SQS redelivery, but cross-instance ownership and lease behavior has not been
accepted as a scaling contract.

## 8. Release verification

Verify in this order so each failure has a narrow owner:

1. `/ping` returns success from the deployed service.
2. `/tools` confirms MCP discovery (this schema-only route is intentionally
   outside the API-token guard).
3. App Runner logs show the configured region/tenant/classification boundary,
   a durable verdict journal, and an active SQS worker.
4. Queue attributes can be read by the agent role, but a send attempt and DLQ
   read are denied.
5. Publish one normal incident through the static-analysis path—do not inject a
   hand-written message as the acceptance proof.
6. Observe one assignment leave the outbox and become in-flight on SQS.
7. Poll authenticated `GET /agent/{investigation_id}` until a terminal verdict
   is stored.
8. Confirm the assignment is deleted only after the terminal result is durable.
9. Confirm no remediation row, sandbox execution, GitHub branch, or pull request
   is created.
10. Submit a deliberately invalid envelope in a non-production queue and prove
    it is rejected without an LLM call.

Record only investigation UUIDs and aggregate results that are approved for the
public repository. Do not paste context, traces, queue URLs, or service logs
that may contain customer or infrastructure details.

## 9. Rollback

Application rollback is an App Runner image rollback to the previously verified
immutable tag. If the worker is unsafe, remove `SQS_QUEUE_URL` from the service
configuration or set desired App Runner capacity to zero; do not purge the
queue. Unacknowledged work remains available for a corrected consumer.

Do not destroy the queue, DLQ, analysis database, or agent database as part of
an application rollback. The new IAM role can remain unused. Remove it only in
a later reviewed Terraform change after the service no longer references it.

## 10. Work deliberately deferred

- Secrets Manager references for App Runner runtime secrets.
- Stable/private App Runner database egress and CockroachDB network allowlisting.
- Multi-replica claim and lease semantics.
- A private EC2/ECS sandbox runner for remediation.
- Dashboard controls to start an agent run, display progress, approve a
  candidate, or open a pull request.
- A narrow context API if direct read-only SQL access becomes too broad for the
  production tenant model.

The dashboard may integrate only after the investigation-only release is
observed end to end. Its server side should call the authenticated agent API;
the browser must never receive `API_TOKEN`, database credentials, or raw agent
trace payloads.
