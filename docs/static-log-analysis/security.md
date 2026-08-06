# Security, Redaction, and Retention

## Data classification

Every field is classified as `PUBLIC`, `INTERNAL`, `SENSITIVE`, `RESTRICTED`, or
`PROHIBITED`. Policy specifies whether it may enter the Collector queue, analysis
journal, CockroachDB, agent context, or report.

PROHIBITED content includes plaintext credentials, access tokens, private keys,
passwords, and any configured forbidden customer payload. It cannot enter a
persistent layer.

## Two-pass redaction

Before the Collector persistent queue, universal policy removes authorization
headers, cookies, passwords, API keys, JWTs, cloud credentials, connection-string
passwords, and common private-key material.

Before the analysis journal, service-aware policy handles customer identifiers,
emails, sessions, payment attributes, request bodies, query parameters, and custom
classified fields. A final prohibited-pattern scan acts as a safety net.

Selected correlatable values become keyed regional HMAC tokens. Keys are stored
in the regional secret system, versioned, access controlled, and rotated through
a documented dual-read transition. Hashes never use an unkeyed sensitive value.

## Redaction failure

When content cannot be proven safe:

1. Attempt structured redaction.
2. Apply universal patterns to remaining text.
3. Treat parse failures as opaque text and redact conservatively.
4. If still unsafe, replace body and stack with
   `[CONTENT_WITHHELD_REDACTION_FAILURE]`.
5. Preserve safe service, severity, time, deployment, tokenized correlation, and
   regional source reference.
6. Count the occurrence toward deterministic thresholds.
7. Audit and alert repeated failures.

The questionable original is never persisted by this system. Authorized humans,
not general agents, may follow the access-controlled regional source reference.

## Authorization

Distinct identities and least-privilege policies exist for producers, Collector,
analysis roles, rule administrators, outbox publisher, orchestrator, agents, and
operators. Agent tools are read-only by default and scoped to service, environment,
region, incident, and time range.

Audit events cover rule and redaction changes, suppression, evidence access,
agent assignment, tool invocation, report publication, overrides, and retries.

## Retention defaults

| Data | Retention |
|---|---|
| Collector queue | Until delivery; capacity target 10 minutes |
| Unprocessed journal | Until processed; capacity target 60 minutes |
| Processed journal | Compact within 15 minutes after DB commit |
| Safe redaction-failure metadata | 30 days |
| Selected redacted evidence | 30 days |
| Incidents and occurrence summaries | 1 year |
| Agent progress | 90 days |
| Final reports and embeddings | 1 year |
| Published outbox records | 7 days |
| Failed outbox and dead-letter records | 30 days |
| Security and administrative audit | 1 year |

Retention is configurable by environment and classification. Legal holds suspend
deletion for named incidents. Deletion cascades to derived embeddings, cached
context, and unneeded evidence. Reports state when referenced raw evidence expires.

## Threats requiring explicit tests

- Secret exfiltration through log bodies, attributes, stack traces, or reports.
- Prompt injection embedded in logs viewed by an agent.
- Forged service or region metadata.
- Oversized records and decompression bombs.
- Regex or rule resource exhaustion.
- Cross-tenant or cross-region evidence access.
- Queue replay and duplicate agent execution.
- Journal tampering or corruption.
- Malicious raw-log references.
- Agent tool escalation or production mutation.

Log content is untrusted evidence, never an instruction to the agent.

Service, environment, and region attributes inside application logs are also
untrusted. Admission validates them against the authenticated Collector or source
envelope before they can influence grouping, authorization, or regional routing.
