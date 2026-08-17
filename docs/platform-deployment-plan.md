# Platform deployment and operations dashboard plan

**Status:** Approved SQS-to-agent integration implemented locally; deployment pending

**Last reviewed:** 2026-08-16

**Scope:** Static log analysis, operations dashboard, shared AWS infrastructure, and `agent_space` integration

**Immediate owner:** The engineer performing the deployment

**Execution record:** [Phase 0 release readiness](deployment/phase-0-release-readiness.md)

**Execution status:** Local validation and read-only AWS inventory complete;
operator decisions and the in-place SSM release deployment remain.

## 1. Purpose

This document turns the two recently merged bodies of work into one deployable
platform plan. It deliberately separates what can be deployed from the current
tree from what still needs an integration contract or implementation change.

The first production-like deployment should prove this path:

```text
CloudWatch Logs
    -> static-log-analysis cloudwatch worker
    -> CockroachDB incident and investigation state
    -> transactional outbox
    -> Amazon SQS assignment queue
    -> agent worker
    -> durable verdict state
    -> authenticated dashboard
```

The dashboard should grow from a read-only health page into the operator control
plane. That expansion must not make the browser a trusted network peer, expose
database credentials, or allow an interactive dashboard failure to interrupt
log analysis.

This remains the umbrella planning document. The now-approved assignment
integration is implemented without reorganizing `agent_space`; its concrete
release contract and operator sequence are in
[agent-sqs-deployment.md](agent-sqs-deployment.md).

## 2. Repository boundaries to preserve

The repository currently has three relevant shapes:

```text
docs/
  static-log-analysis/       existing normative analysis documentation
  platform-deployment-plan.md

services/
  static-log-analysis/       Go detector, persistence, outbox, deployment
  dashboard/                 Next.js operations dashboard

agent_space/                 separately merged Go agent/orchestrator
```

For the work described here:

- New deployable application components belong under `services/<service-name>/`.
- Cross-system plans belong directly under `docs/`; service-specific normative
  changes remain with that service's existing documentation.
- Do not reorganize `agent_space` during deployment work. Its HTTP API, strict
  SQS consumer, separate context connection, and runtime settings are explicit
  integration surfaces.
- Do not place a new cross-service API inside the dashboard merely because the
  dashboard needs it. The dashboard can use Next.js server routes as a thin
  backend-for-frontend, but durable orchestration belongs in a service.
- Existing security and regional-boundary requirements in
  `docs/static-log-analysis/security.md` retain precedence.
- This repository is public. Commit architecture, schemas, reproducible
  commands, sanitized results, and placeholders only. Keep live AWS account and
  resource IDs, principal ARNs, public addresses, hostnames, queue URLs,
  operator CIDRs, tenant names, database endpoints, and state inventories in
  ignored operator configuration or the approved secret/state systems.

## 3. Current state, based on the merged tree

### 3.1 Static log analysis

The analysis service is the most deployment-ready component.

- Terraform under `services/static-log-analysis/infra/aws/` provisions one
  Amazon Linux 2023 EC2 host, a stable Elastic IP, IAM instance profile, exact
  CloudWatch Logs permissions, an SQS Standard queue and DLQ, and optional
  dashboard ingress.
- The production Compose file runs migration, the combined `cloudwatch` role,
  the `outbox` role, and the optional dashboard/Caddy profile.
- The analysis host accepts no inbound traffic unless dashboard ports are
  explicitly enabled. Administration uses Systems Manager Session Manager.
- The journal and CloudWatch checkpoints are local Docker volumes on encrypted
  EBS. This makes the analysis worker intentionally single-instance.
- Managed CockroachDB is external. The DSN is installed after provisioning and
  is deliberately excluded from Terraform state and user data.
- The deployment helper fetches and builds a selected repository revision on
  the host. It is functional for a hackathon deployment, but is not yet an
  immutable image promotion pipeline.

### 3.2 Dashboard

The dashboard is a functional, bounded, read-only operations view.

- Better Auth uses a local SQLite volume for users and sessions.
- Only an authenticated administrator can reach the overview page.
- Caddy terminates public TLS; Next.js is not directly internet-facing.
- The dashboard has no CockroachDB credential. It reads a safe local projection,
  readiness endpoints, unlabelled metrics, and SQS queue attributes.
- It currently shows pipeline health, aggregate counts, and recent
  investigations. It has no agent API integration or command workflow.

### 3.3 Agent environment

`agent_space` now implements the bounded investigation-only integration path.

- It has HTTP endpoints for direct questions, investigation lookup,
  remediation lists/details, candidate creation, draft PR creation, and
  recording an engineer decision.
- It consumes `agent.assignment.v1` from SQS and performs one investigation at
  a time with visibility renewal and redelivery handling.
- Its existing deployment script builds an ECR image and creates or updates an
  App Runner service.
- The App Runner script requires the queue, trusted tenant/classification,
  separate analysis connection, runtime IAM role, API authentication, and
  optional CORS. It forces remediation off and pins the service to one instance.
- Full remediation launches build/test containers. The current App Runner
  image has no Docker runtime or safe remote sandbox provider, so App Runner can
  host the HTTP/investigation subset but not the complete remediation path.
- The agent requires LLM, embedding, CockroachDB MCP, CockroachDB SQL, and
  optionally GitHub credentials. Those credentials are currently collected as
  runtime environment variables by a shell deployment script rather than
  managed as Terraform-owned secret references.

## 4. Integration decisions and remaining blockers

These are release blockers for the full end-to-end platform, even if each
individual container starts successfully.

### 4.1 Database schema collision — resolved in the runtime contract

Both applications use a table named `investigations`, but with incompatible
schemas and ownership assumptions.

- Static analysis owns a region- and tenant-scoped `investigations` table with
  lifecycle state and foreign keys into incident families.
- The agent journal attempts `CREATE TABLE IF NOT EXISTS investigations` with a
  different primary key and verdict/result columns, then upserts agent-shaped
  rows into it.

Pointing the agent's current `DATABASE_URL` at the static analysis database will
therefore not integrate the systems. Table creation may appear to succeed
because the table already exists, but the first agent upsert will fail.

The approved layout uses separate logical databases and database users:

```text
CockroachDB cluster
  static_log_analysis   owner: analysis runtime/migrator
  agent_space           owner: agent runtime/migrator
```

`DATABASE_URL` owns agent state. `ANALYSIS_DATABASE_URL` is a distinct read-only
connection to the three analysis context tables. The worker refuses identical
URL strings. Provisioning and verifying the database grants remains deployment
work.

### 4.2 Incident context mismatch — resolved for the first release

The SQS envelope is broadly aligned, but the context lookup is not.

- Static analysis persists immutable `investigation_contexts` snapshots keyed
  by region, tenant, investigation, and version.
- The integrated resolver now joins the analysis-owned investigation, family,
  and context tables using the full scoped identity and decodes the immutable
  `SafeValue` snapshot.
- The normative assignment contract says the message is a pointer and the
  consumer retrieves the full scoped package; it does not authorize replacing
  that package with a demo-only row.

The approved first boundary is a read-only SQL user. The query includes region,
tenant ID, investigation ID, incident ID, and context version, caps the snapshot
size, and cross-checks service, environment, severity, and classification. A
private API remains the production alternative if SQL grants cannot express the
future tenant model narrowly enough.

### 4.3 SQS permission and message validation — implemented, apply pending

The analysis role can publish. The agent runtime also needs a distinct AWS
identity with only:

- `sqs:ReceiveMessage`;
- `sqs:DeleteMessage`;
- `sqs:ChangeMessageVisibility`;
- `sqs:GetQueueAttributes`; and
- the exact assignment queue ARN as its resource.

Terraform defines the distinct App Runner role with precisely those permissions
against the assignment queue. The consumer now strictly validates the closed
body, exact transport attributes, identifiers, routing metadata, trusted region,
tenant, and classification before any context lookup or LLM call.

### 4.4 Agent runtime placement — blocker for remediation

The complete agent requires a sandbox capable of cloning, patching, building,
and testing repositories. Giving a public web container direct control of a
host Docker socket is too broad a privilege boundary.

The first deployment must choose one of these explicitly:

| Option | Capability | Cost/complexity | Decision |
|---|---|---|---|
| Dedicated private EC2 agent host | Supports the current container sandbox with host hardening | Moderate | Candidate for the first remediation demo |
| App Runner | Simple public API and investigation worker; remediation forced off | Low | **Approved first integrated release** |
| ECS tasks for API/worker plus one task per sandbox job | Stronger isolation and scaling | Higher | Production target after the first deployment |

Any later EC2 sandbox host must be separate from the analysis host. A
compromised agent or repository build must not gain access to the analysis
journal, dashboard authentication database, or analysis Docker daemon.

### 4.5 Agent durability and concurrency — blocker before scaling

The current worker processes serially and holds some status in process memory,
with a CockroachDB mirror when its journal initializes. Before running multiple
replicas, the system needs an authoritative lease/claim implementation that is
compatible with the static analysis investigation lifecycle. Until then, deploy
exactly one agent worker.

## 5. Recommended target architecture

### 5.1 First integrated deployment

The approved first release is the singleton, token-protected App Runner
investigation worker defined in
[agent-sqs-deployment.md](agent-sqs-deployment.md). It consumes the exact SQS
queue through its runtime role, reads safe analysis context through a distinct
read-only connection, writes its own database, and has remediation forced off.

### 5.2 First remediation-capable deployment

```text
                                    public HTTPS
Engineer --------------------------------+--------------------+
                                         v                    |
                                Caddy + dashboard             |
                                on analysis host              |
                                  | server-side only          |
                                  | API token + private VPC   |
                                  v                           |
CloudWatch -> analysis EC2 -> CockroachDB Cloud <- agent EC2  |
                 |                |                  |         |
                 +-> SQS ---------+------------------+         |
                                      poll/ack                  |
                                                              |
Agent EC2: private agent API + one worker + isolated build containers
No public agent port; administration through SSM
```

The important properties are:

- one analysis worker owns one local journal;
- one agent worker consumes the assignment queue;
- analysis and agent state use different databases and credentials;
- the agent host has no inbound internet rule;
- the analysis/dashboard host may call the agent API over a security-group
  referenced private rule;
- browsers call only the dashboard origin;
- the dashboard server holds the agent API credential and never serializes it
  to client JavaScript;
- both hosts have stable, reviewable egress for CockroachDB allowlisting;
- public GitHub/LLM/MCP egress is allowed only where required; and
- raw logs and context snapshots never enter dashboard responses.

### 5.3 Later production direction

After the workflow is stable, split the control API, worker, and sandbox runner:

```text
Dashboard -> control API -> durable run/approval records
                         -> SQS agent-work queue -> stateless workers
                         -> sandbox job queue -> isolated ECS tasks
```

That is not required for the first integrated deployment. It becomes justified when there
is a need for multiple workers, per-run isolation, autoscaling, or independent
dashboard/control-plane releases.

## 6. Infrastructure-as-code plan

### 6.1 Terraform structure

Do not immediately rewrite the working static-analysis module. First deploy and
capture its behavior. Then introduce a small environment composition layer:

```text
infra/aws/
  modules/
    analysis-stack/        wraps or promotes existing analysis resources
    agent-stack/           later sandbox compute, IAM, security group, logs
    shared-network/        discovered VPC/subnets and SG relationships
  environments/
    dev/
    demo/
```

During the transition, `services/static-log-analysis/infra/aws/` remains the
source of truth for the existing analysis deployment. Do not maintain copied
Terraform in both locations. Either call it as a module after making its inputs
module-safe, or move it in one reviewed change and update every reference and
deployment test in the same change.

If the team intentionally wants service-owned infrastructure instead, add
`services/<new-service>/infra/aws/` for service-local resources and keep only
shared queues, networking, DNS, and environment composition in `infra/aws/`.

### 6.2 Terraform state

Before a second engineer or CI can apply infrastructure:

- configure an encrypted remote state backend;
- enable state locking;
- use a distinct state key per environment;
- restrict state access to deployment principals;
- never put CockroachDB DSNs, LLM keys, MCP keys, API tokens, Better Auth
  secrets, or GitHub tokens into variables that are persisted in state; and
- record import/move commands for any existing manually created resources.

For a one-person first deployment, local state is acceptable only as a brief
bootstrap step if it is encrypted, backed up, ignored by Git, and migrated to
remote state before collaboration begins.

### 6.3 Resources to add for the agent stack

- Dedicated EC2 instance and encrypted gp3 volume.
- No public inbound rules; SSM access through the standard instance profile.
- Security-group ingress on the agent API port from the dashboard/analysis
  security group only.
- IAM policy restricted to receive/delete/change-visibility on the one queue.
- CloudWatch Logs group and agent log shipping.
- Stable outbound address, or a VPC/NAT design whose egress IP can be
  allowlisted by CockroachDB Cloud.
- Root-only secret installation or secret-manager references.
- A systemd unit that starts an already built immutable image and does not fetch
  `latest` on restart.
- Disk, memory, CPU, and concurrent-container limits for sandbox jobs.
- Alarms for worker unavailable, oldest SQS message age, DLQ depth, disk usage,
  and repeated agent failures.

### 6.4 DNS and certificates

- Keep one public dashboard hostname for the initial deployment.
- Do not create a public DNS record for the agent API.
- Continue to terminate TLS at Caddy for the dashboard.
- Use private HTTP only if security groups are the enforced boundary and the
  risk is accepted for the demo; otherwise issue private TLS and validate it
  from the dashboard server.
- Plan DNS before enabling Caddy so certificate issuance does not become the
  deployment's final blocker.

## 7. Secrets and identities

Use separate credentials for separate responsibilities.

| Principal | Required access | Must not have |
|---|---|---|
| Analysis runtime | analysis DB DML, exact CloudWatch reads, SQS publish | agent DB writes, SQS receive/delete |
| Analysis migrator | analysis DB DDL | runtime AWS privileges |
| Agent runtime | agent DB DML, context read boundary, SQS consume, approved MCP reads | analysis DB writes, CloudWatch source reads |
| Agent migrator | agent DB DDL | analysis DB DDL |
| Dashboard server | safe overview endpoints, queue attributes, private agent API | either database DSN, queue receive/delete |
| Sandbox job | one repository/ref and bounded network credentials | database, SQS, host Docker control, production secrets |

Secret handling requirements:

- Generate independent dashboard-session and dashboard-to-agent API secrets.
- Keep `API_TOKEN` on the agent and dashboard server only.
- Set an exact dashboard origin in `CORS_ORIGINS`, even though normal dashboard
  calls should be same-origin through the backend-for-frontend.
- Store GitHub credentials only on the component that opens a PR. Prefer a
  GitHub App installation token over a long-lived personal token.
- Keep remediation disabled until its sandbox and GitHub permission boundary
  is explicitly verified.
- Rotate any credential that was previously copied through an unreviewed `.env`
  deployment path before calling the environment production-like.
- Redact configuration values from logs and support rotation without rebuilding
  an image.

## 8. Deployment phases

Each phase has an independent acceptance condition. Do not continue merely
because containers report `running`.

### Phase 0 — freeze and inventory the release

Tasks:

1. Select and record one immutable Git commit SHA containing both merges.
2. Run the static-analysis fast, generated-proto, deployment-render, Terraform,
   dashboard, and agent unit-test gates from that exact SHA.
3. Produce a deployment inventory: AWS account, region, VPC/subnet, hostnames,
   CloudWatch log groups, CockroachDB cluster/region, databases, queues, and
   owners.
4. Decide `dev` versus `demo` naming and tags before creating resources.
5. Estimate baseline cost and define a deletion date for demo resources.
6. Record all known manual resources and whether Terraform will create or import
   them.
7. Store the detailed live inventory outside Git and commit only a sanitized
   readiness record.

Acceptance:

- The release SHA and test outputs are recorded.
- There are no uncommitted release artifacts.
- Region and tenant/classification values are explicit.
- An operator knows which resources will incur cost and how to remove them.

### Phase 1 — deploy static analysis without the agent

Tasks:

1. Configure and review the existing analysis Terraform plan.
2. Provision the analysis EC2 host, IAM role, Elastic IP, assignment queue, and
   DLQ.
3. Create/verify the managed `static_log_analysis` database in the same physical
   region and allowlist the host egress `/32`.
4. Install the TLS DSN through Session Manager.
5. Deploy the pinned SHA and verify migrations, CloudWatch polling, journal
   persistence, incident creation, outbox publish, and SQS depth.
6. Reboot the host and verify the same image and persistent volumes return.
7. Exercise the database outage and SQS outage behaviors described in the
   existing runbooks.

Acceptance:

- A known CloudWatch log reaches a committed investigation assignment in SQS.
- No public application port is open.
- Reboot does not change the release or lose journal/checkpoint state.
- An SQS outage leaves the transactional outbox recoverable.

Rollback:

- Re-deploy the previously recorded SHA with the existing deployment helper.
- Do not roll back a database migration unless a separately reviewed reverse
  migration exists; prefer forward repair.

### Phase 2 — enable the read-only dashboard

Tasks:

1. Configure the hostname, DNS, restricted ingress CIDR where practical, and
   `dashboard_enabled` Terraform input.
2. Install Better Auth state and create named administrator accounts. Avoid a
   shared `admin` credential outside the short demo window.
3. Verify HTTPS, session expiry, unauthorized responses, rate limiting, and
   partial-source degradation.
4. Confirm through browser/network inspection that only the documented safe
   overview projection reaches the client.
5. Back up or explicitly accept loss of the local dashboard-auth volume.

Acceptance:

- An authorized engineer can view health and recent investigation metadata.
- An anonymous, non-admin, or banned user cannot view it.
- Stopping the dashboard/Caddy profile does not stop analysis or outbox workers.
- No raw log, context snapshot, queue payload, secret, or DSN appears in the
  browser response.

### Phase 3 — settle agent integration contracts without deploying it

Tasks:

1. Version and test the complete SQS consumer contract, including body schema,
   SQS attributes, region/tenant/classification, deduplication, and permanent
   versus retryable failures.
2. Choose the context-reader boundary and write its request/response schema.
3. Separate agent state from analysis state at the database and credential
   level.
4. Define how agent progress and final reports update the normative analysis
   investigation lifecycle without allowing the agent to overwrite detection
   state.
5. Define ownership for migrations of agent tables.
6. Define the runtime/sandbox boundary and decide what remediation features are
   enabled in the first deployment.
7. Add a cross-repository integration test using a real CockroachDB and
   LocalStack: producer assignment -> consumer validation -> context retrieval
   -> durable result, with the LLM replaced only at its narrow interface.

Acceptance:

- No component relies on the incompatible shared `investigations` table.
- One fixture generated by the producer is accepted by the consumer unchanged.
- The consumer retrieves the exact immutable context version named by the
  assignment.
- Duplicate delivery does not create a second run or decision.
- A poison message cannot starve valid messages indefinitely.

### Phase 4 — provision and deploy one private agent worker

Tasks:

1. Add and review the agent Terraform module and IAM policy.
2. Provision the separate host and allowlist its stable egress.
3. Create the `agent_space` database and dedicated runtime/migration users.
4. Install secrets out of band or through secret references.
5. Deploy an immutable image built from the same release SHA.
6. Start with remediation disabled; prove queue consumption, context retrieval,
   investigation, durable result, acknowledgement, restart recovery, and DLQ
   behavior.
7. Enable remediation only after sandbox resource/network isolation and GitHub
   permission tests pass.
8. Run exactly one worker replica.

Acceptance:

- A real static-analysis assignment is consumed and acknowledged only after a
  durable result.
- The dashboard/analysis host can reach the private agent API; the public
  internet cannot.
- Restart does not cause completed work to run twice.
- The worker does not have analysis write permission.
- With remediation disabled, no repository or GitHub write occurs.

### Phase 5 — add read-only agent visibility to the dashboard

Implement this before adding buttons.

**Progress:** The bounded read-only slice is implemented locally. It includes
server-only authenticated clients for `/ping`, `/agent/:id`, `/repositories`,
`/remediations`, and `/agent/:id/remediation`; the agent health pill;
detector-to-agent status; investigation detail; remediation list/detail pages;
bounded candidate diffs; and factual verification and decision state. The UI
now distinguishes a model recommendation from remediation execution and never
uses investigation `done` to claim a rollback occurred. `agent_space` exposes
empty remediation history while execution is forced off, and independently
rejects every remediation mutation. The agent journal now persists a maximum
of 50 categorical context/model/tool/verdict events during a run, and the
investigation detail page renders that timeline without exposing reasoning,
arguments, output, provider errors, or context.

Tasks:

1. [x] Add server-side dashboard client code for agent `/ping`, investigation,
   repository, remediation-list, and remediation-detail endpoints.
2. [x] Validate every upstream response with a schema before rendering it.
3. [x] Add an agent health pill, queue-to-run status, investigation detail page,
   trace summary, remediation candidates, verification output, and recorded PR
   links.
4. [x] Bound list size, diff size, trace size, response size, and polling
   frequency. Raw verification logs remain excluded rather than truncated.
5. [x] Render upstream partial failure without hiding analysis health.
6. [x] Keep the upstream API token in server-only configuration.

Acceptance:

- An operator can follow an assignment from queue to verdict/remediation.
- Agent unavailability degrades only agent panels.
- No browser request is made directly to the private agent API.
- Large diffs/traces cannot cause unbounded page payloads.

### Phase 6 — add controlled operator actions

Start with actions already represented by the agent API:

1. trigger remediation for an existing investigation;
2. record a chosen candidate and rejection reasons;
3. request a draft PR for an approved candidate; and
4. retry only operations whose idempotency contract permits it.

For every action:

- require a current authenticated session and an explicit role;
- validate input server-side;
- use CSRF protection and same-origin requests;
- attach an idempotency key;
- show a confirmation containing the target and effect;
- write an append-only audit event before/with execution;
- return a durable operation ID and poll status rather than holding a browser
  request open for a multi-minute job; and
- never treat the model's recommendation as authorization.

Suggested roles:

| Role | Capabilities |
|---|---|
| `viewer` | Health, investigations, verdicts, safe traces |
| `operator` | Start/retry bounded investigations and remediation |
| `approver` | Record decisions and authorize draft PR creation |
| `admin` | Account/role management and configuration visibility |

Acceptance:

- Every state-changing request is attributable to an authenticated user.
- Refresh/retry cannot duplicate a run, decision, or PR.
- A viewer cannot invoke a mutation.
- Audit history contains actor, action, target, request ID, time, result, and a
  safe reason without secrets or raw evidence.

### Phase 7 — support engineer-started agents

Do not implement this as an unrestricted text box that calls the synchronous
`POST /agent` endpoint. Introduce a durable request resource.

Minimum request fields:

- service and environment;
- region and tenant derived from the operator's authorized scope;
- objective selected from an approved type or bounded text;
- optional existing incident/investigation reference;
- evidence/context version;
- permitted tools and explicit prohibited actions;
- time, token, and monetary budget;
- repository/ref if code inspection is allowed; and
- idempotency key and human reason.

Recommended workflow:

```text
POST /operator-agent-runs -> 202 + run_id
        -> validate authorization and scope
        -> persist queued request and audit event
        -> publish internal work item
worker  -> claim/lease -> progress -> report
GET /operator-agent-runs/:id -> bounded status/result
POST /operator-agent-runs/:id/cancel -> cooperative cancellation
```

The existing static-analysis assignment remains detector-owned. Manual runs use
a distinct schema such as `operator.agent.request.v1`; they must not forge an
incident assignment or mutate deterministic incident-generation identity.

Acceptance:

- Manual work is visibly distinct from detector-triggered investigations.
- Scope and tool permissions are enforced outside the prompt.
- Budgets and concurrency limits are enforced by the orchestrator.
- Cancel, timeout, retry, and duplicate-submit behavior are deterministic.
- A manual run cannot perform production remediation without a separate
  approval.

### Phase 8 — hardening and repeatable releases

Tasks:

1. Build images in CI, scan them, generate an SBOM, and push immutable digest
   references to ECR.
2. Promote one digest between environments; do not rebuild source on a host.
3. Add deployment identity/version to readiness and dashboard views.
4. Introduce automated pre-deploy migration checks and post-deploy smoke tests.
5. Add alarms and an operator runbook for each alert.
6. Test restore of CockroachDB data, analysis journal/checkpoints, dashboard
   auth state, and agent state according to their different recovery goals.
7. Run a rollback rehearsal and a credential-rotation rehearsal.
8. Add retention and deletion policies for agent traces, diffs, build logs,
   reports, and audit events.

Acceptance:

- A release is identified by commit and image digest everywhere.
- Deployment is repeatable without manual source editing on hosts.
- Restore and rollback procedures have measured results.
- All state-changing dashboard actions have alerts/audit coverage.

## 9. Dashboard product roadmap

### 9.1 Information architecture

Build toward these pages without attempting all of them in the first iteration:

```text
/                         platform overview
/investigations           detector and agent run list
/investigations/:id       context, progress, verdict, report, remediation
/remediations             candidate/decision queue
/remediations/:id         diffs, verification, selection, PR action
/agents/new               operator-started run form
/agents/:id               manual run progress/result
/audit                    operator action history
/settings/integrations    read-only configuration and connectivity state
```

### 9.2 Near-term dashboard additions

- Show the deployed commit/image and last successful collection time.
- Link queue counts to bounded lists of investigation IDs, not queue bodies.
- Show agent availability separately from analysis and outbox health.
- Add investigation state history and timestamps.
- Display verdict, confidence, grounding source count, and a bounded safe trace.
- Show remediation strategies side by side with build/test outcomes.
- Make destructive or external actions visually distinct from read-only actions.
- Use polling initially; add SSE only when measured polling load or latency
  justifies it.

### 9.3 Dashboard backend-for-frontend rules

- All database and agent access remains server-side.
- Define one typed client module per upstream service.
- Apply timeouts, response-size limits, schema validation, and categorical error
  mapping at the boundary.
- Do not pass upstream error bodies through verbatim.
- Never cache user-specific or sensitive operator responses in shared Next.js
  caches.
- Use request IDs across browser, dashboard server, control API, agent, and
  audit records.
- Mutations should call a control service once durable orchestration exists;
  avoid growing durable business logic inside Next.js route handlers.

## 10. Observability and operational readiness

Minimum signals:

| Component | Health | Metrics/alarms | Logs |
|---|---|---|---|
| Analysis worker | journal + DB + source readiness | journal bytes/pending, rejection categories, lag | structured, no raw payloads |
| Outbox worker | DB + SQS readiness | pending age/count, retries, permanent failure | message IDs only |
| SQS | AWS service | visible, in-flight, oldest age, DLQ > 0 | CloudTrail for configuration |
| Agent worker | DB + MCP + queue readiness | run duration/status, token/cost budget, lease renewal | safe trace categories |
| Sandbox | scheduler readiness | queued/running, CPU/memory/disk, timeout/failure | bounded build logs |
| Dashboard | auth store + upstream partial health | latency, 5xx, auth failures, action outcomes | actor/request IDs, no secrets |

Required runbooks:

- analysis host/journal full;
- CockroachDB unavailable or region mismatch;
- CloudWatch permission/source failure;
- outbox backlog and SQS unavailable;
- poison message and DLQ redrive;
- agent MCP/LLM/embedding provider unavailable;
- agent worker restart with in-flight work;
- remediation sandbox timeout or disk exhaustion;
- dashboard authentication recovery;
- credential rotation;
- release rollback; and
- complete demo-environment teardown.

## 11. Testing and release gates

Keep the existing static-analysis gates. Add platform gates progressively:

### Per change

- Go unit tests for the affected Go module.
- Dashboard typecheck/tests/build for dashboard changes.
- Terraform formatting, validation, and a reviewed saved plan.
- Compose rendering and configuration validation.
- Secret scanning and image scanning.

### Integration

- Real CockroachDB for storage schemas and concurrent claims.
- LocalStack for SQS publish/consume attributes, retry, visibility, and DLQ.
- Producer-owned assignment fixtures consumed by agent tests.
- Browser tests for auth, role gates, read-only views, mutation confirmation,
  idempotency, and upstream degradation.
- A sandbox fixture repository that contains no external credentials and proves
  clone/build/test/time-limit behavior.

### Pre-deployment smoke

1. Verify AWS caller/account/region.
2. Verify the selected Git SHA and image digest.
3. Verify CockroachDB database region and TLS.
4. Verify exact CloudWatch log groups exist.
5. Verify queue and DLQ redrive policies.
6. Verify no unexpected public security-group ingress.
7. Verify required secret references exist without reading their values.

### Post-deployment smoke

1. Read readiness from each private component.
2. Submit one known log and observe its safe record count.
3. Observe incident/investigation/outbox state.
4. Observe one SQS assignment.
5. In the agent-enabled phase, observe claim, context retrieval, durable verdict,
   and acknowledgement.
6. Sign in to the dashboard and follow the same correlation ID.
7. Confirm the DLQ remains empty and no secret/raw evidence appears in logs or
   browser responses.

## 12. Recommended work packages

These can become issues or pull requests. Keep each package independently
reviewable.

1. **Release inventory:** pin SHA, run gates, record resources and owners.
2. **Analysis Terraform apply:** provision and prove the current supported stack.
3. **Dashboard activation:** DNS, TLS, named admin, safe-data verification.
4. **Remote Terraform state:** backend, locking, environment keys, CI plan role.
5. **Contract reconciliation:** assignment schema/attributes and shared fixtures.
6. **Context reader design:** schema, authorization, read path, threat model.
7. **Agent database separation:** connection roles, migrations, collision tests.
8. **Agent runtime ADR:** EC2 demo decision and ECS production direction.
9. **Agent Terraform:** host, IAM, SG, logs, egress, secret references.
10. **End-to-end worker test:** real DB + LocalStack, duplicate and poison cases.
11. **Dashboard agent read model:** typed server client and read-only pages.
12. **Audit subsystem:** append-only event contract and safe projection.
13. **Dashboard command model:** RBAC, CSRF, idempotency, durable operations.
14. **Manual agent runs:** separate request schema, queue, budgets, cancellation.
15. **Immutable release pipeline:** CI images, digest promotion, smoke/rollback.

Packages 5–7 and the investigation-only portions of 8–10 are now implemented;
their infrastructure apply and end-to-end release evidence remain. Packages
12–14 should not start until the read-only integration is stable in deployment.

## 13. Decisions to record before implementation

Create ADRs or explicit decisions for the following. Recommended defaults are
included so planning does not stall.

| Decision | Recommended default |
|---|---|
| First environment | One disposable `demo` environment in the target region |
| Analysis placement | Existing singleton EC2/Compose deployment |
| Agent placement | Singleton App Runner for investigation-only; separate sandbox compute later |
| Database layout | Separate `static_log_analysis` and `agent_space` databases |
| Context boundary | Dedicated read-only SQL connection for the first release; private API remains an option |
| Public entry points | Dashboard and token-protected agent API for the first App Runner release |
| Dashboard-to-agent access | Server-side private VPC call with independent token |
| Worker replicas | One until durable leases are reconciled |
| Remediation on first boot | Disabled, then explicitly enabled after sandbox verification |
| Manual run transport | Separate durable `operator.agent.request.v1` workflow |
| Image release | Pin SHA now; move to immutable ECR digest in hardening phase |
| Terraform state | Remote encrypted state with locking before shared applies |

## 14. Definition of “everything is deployed”

The phrase should mean all of the following, not merely that Terraform applied:

- Infrastructure is represented in reviewed Terraform and uses a known state.
- Static analysis ingests a real configured log source and preserves its
  journal/checkpoint invariants.
- CockroachDB stores the expected analysis state in the correct region.
- The outbox publishes a contract-valid assignment to SQS.
- One independently secured agent worker consumes that assignment, retrieves
  the exact safe context version, records a durable result, and acknowledges it.
- The dashboard shows health and the investigation path through authenticated,
  bounded, server-side integrations.
- State-changing operator features are either disabled or protected by roles,
  idempotency, approval, and audit.
- No incompatible table sharing, unauthenticated agent API, broad IAM grant,
  unmanaged secret, or raw log exposure is being accepted silently.
- Smoke, restart, outage, rollback, and teardown procedures have named owners
  and recorded results.

The schema and context mismatches are resolved in code. The accurate status is:
**analysis and dashboard deployed previously; strict investigation-only agent
integration implemented and locally verified; IAM/database provisioning,
App Runner deployment, and end-to-end evidence still pending**.
