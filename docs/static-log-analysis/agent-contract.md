# Agent Assignment and Context Contract

## Queue message

SQS carries a small immutable pointer, not full evidence:

```json
{
  "schema_version": "1.0",
  "message_id": "uuidv7",
  "message_type": "investigation.requested",
  "incident_id": "uuidv7",
  "incident_generation": 1,
  "investigation_id": "uuidv7",
  "region": "us-east-1",
  "service_id": "payment",
  "environment": "production",
  "severity": "critical",
  "created_at": "2026-08-06T18:04:51Z",
  "context_version": 1
}
```

The orchestrator retrieves the full package from CockroachDB using its scoped
identity. Queue duplication and reordering are expected.

## Initial investigation package

The package contains:

- Incident, generation, and investigation identities.
- Affected service, owner, environment, region, and criticality.
- Deployment version, commit, deployment ID, and deployment time.
- Exact rule ID, version, threshold, observed values, and trigger time.
- Canonical error, safe message, error code, exception, and stack fingerprint.
- First/latest occurrence, frequency, affected requests, containers, and hosts.
- Tokenized trace, request, session, container, and host correlations.
- Related services and linked incidents with relationship evidence.
- Representative redacted logs and regional raw-log references.
- Recent deployments, commits, configuration, dependencies, and flag changes.
- Similar historical incidents and their resolved reports.
- Missing-enrichment statuses.
- Objective, approved tools, prohibited actions, limits, and report schema.

## Evidence selection

Initial context is curated deterministically. It includes triggering records,
first and latest occurrences, up to 20 representative errors, relevant context
before and after them, and one example per distinct stack fingerprint. Diversity
across containers, traces, and variants takes priority over repeated identical
records.

The queue and initial prompt do not contain thousands of raw logs. Agents use
scoped tools to request additional regional evidence.

## Workspace

Each investigation receives an isolated workspace containing:

```text
/workspace
  investigation.json
  evidence/representative-logs.jsonl
  evidence/stack-traces.json
  evidence/references.json
  repository/     # clean checkout at deployed commit
  output/investigation-report.json
```

The workspace has time-limited, read-only, region- and service-scoped credentials.
It contains no production secret material and no writable state shared with other
investigations. The repository starts at the deployed commit, not the latest
default branch.

## Concurrency and leases

- One active investigation per incident family.
- Default maximum: two concurrent agents per service and environment.
- Additional work queues by severity and age.
- Default lease: five minutes, renewed every minute.
- Lease expiry permits reassignment of the same investigation.
- Reassignment resumes stored progress and never creates a new investigation.
- Default execution window: 30 minutes, with controlled progress-based extension.
- Regional concurrency limits protect telemetry and source systems.
- A critical service may receive an explicitly configured higher cap.

## Progressive enrichment

Context packages are versioned. New deployments, traces, commits, related
incidents, and source metadata increment the context version and notify the active
agent. In the initial implementation, the orchestrator checks the latest context
version at investigation checkpoints. A separate update queue is deferred unless
measured polling latency is inadequate. Enrichment never restarts an agent.

## Agent progress and report

Progress updates use `(investigation_id, sequence)` for idempotency and include
state, safe status, evidence added, and update time.

The final structured report contains summary, impact, timeline, findings, root
cause status and confidence, supporting and contradicting evidence, recommended
actions, unanswered questions, and evidence references. Recommendations are not
authorization to mutate production. Remediation uses a separate approval path.

Report completion does not resolve an incident automatically. A new generation
starts another investigation only after a deployment change, material severity
increase, explicit per-generation rule, or the default 24-hour reinvestigation
interval, and only when no investigation for the family is active.
