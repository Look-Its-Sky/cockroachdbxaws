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

Opaque text normalizes CRLF and bare CR boundaries before applying line-based
rules, so alternate line endings cannot hide authorization, cookie, or secret
assignments. JSON carried inside a string is token-preflighted before recursive
materialization, with a maximum depth of 16 and 10,000 container/scalar value
nodes. An over-limit JSON value is withheld rather than partially represented.
The same bounded preflight tracks decoded keys within each object. Duplicate
keys, including escape-equivalent spellings, withhold the whole structured value
before map materialization; reusing a key in separate sibling objects is valid.
Opaque assignment labels adjacent to `:` or `=` fail closed when they contain
non-ASCII characters or Unicode escapes. This deliberately withholds some
ordinary Unicode `label: value` prose: finite confusable-character tables cannot
prove an unknown label is not a disguised credential name. ASCII timestamps and
ASCII labels carrying Unicode values remain admissible.

Structural field names use a stricter boundary than prose: any non-ASCII,
invalid-UTF-8, or Unicode-escaped OTLP key is withheld together with its value.
When duplicate or redaction-normalized keys collide, every ambiguous member is
withheld under deterministic non-secret ordinals; no first value is retained.
The persistence-facing final scan reapplies the same strict key validation to
top-level and nested maps. It accepts generated withheld-key ordinals only when
their values are typed as withheld metadata. A known-sensitive ASCII key is
accepted only with the canonical `[REDACTED_FIELD]` value produced by the
normalizer.

Errors crossing admission, normalization, model-validation, and safe-value JSON
boundaries are categorical and opaque. They identify stable error classes and
non-secret structural ordinals, but never copy rejected field names, values,
claims, identifiers, enum text, schema text, locations, or parser details from
untrusted input into an error string.

The baseline service-aware policy version `2.2` removes complete assignments
for customer/session identifiers, request bodies, query parameters, and payment
card/PAN/CVV/CVC fields across underscore, dot, and hyphen variants. It also
recognizes Unicode local parts and internationalized email domains. Truly
service-specific customer payloads remain configured forbidden values; the
baseline does not claim to infer arbitrary custom field semantics.

Policy-generated text is byte-idempotent: applying the text policy again does
not change its bytes. A marker already present in raw input is retained only with
`safety.preexisting_marker` provenance; an exact
`[CONTENT_WITHHELD_REDACTION_FAILURE]` input becomes typed withheld metadata at
the first safe-value boundary. Thus provenance and type may be added on that
first boundary even though the marker bytes themselves remain stable.

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
