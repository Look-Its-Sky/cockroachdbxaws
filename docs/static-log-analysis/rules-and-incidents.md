# Rules, Grouping, and Incident Lifecycle

## Rule principles

Rules are deterministic, versioned, testable, bounded in cost, and immutable once
used to trigger an incident. A rule update creates a new version.

Initial typed rule families are:

- Exact error-code and severity match.
- Safe regular-expression match.
- Exception or stack fingerprint match.
- Repeated count within a window.
- Error rate with a minimum denominator.
- Consecutive health failure.
- Absence of an expected health signal.
- Composite `all` and `any` conditions.

CEL may later express custom stateless predicates. Go code continues to own state,
windows, counters, grouping, and lifecycle. Arbitrary scripts are prohibited.

Rules declare their protected input classes so load shedding cannot discard an
INFO record required by a health, rate-denominator, or composite rule.

## Default execution thresholds

Every triggered rule creates or updates an incident; only investigation policy
starts an agent.

| Condition | Default agent behavior |
|---|---|
| FATAL | Immediate |
| Critical/high qualifying security event | Immediate |
| Ordinary ERROR | Five matching occurrences in five minutes |
| WARN | Explicit rule only |
| INFO/DEBUG | Explicit composite rule only |
| Health failure | Three consecutive failures |
| Error rate | Configured rate plus minimum traffic volume |

Maintenance, suppression, and known-test conditions remain auditable but do not
start agents. Every incident records the exact start or suppression reason.

## Grouping hierarchy

```text
Incident family
  service + environment + normalized fingerprint
  -> incident generation
       deployment identity + episode boundary
       -> occurrences and evidence
       -> zero or one active investigation
```

Container, host, trace, request, and session IDs enrich an incident rather than
split it. A deployment change creates a linked generation in the same family.

When the same failure continues across a deployment, the active agent receives
the new generation as additional context. A second agent does not start merely
because deployment changed.

## Cross-service failures

Different services and fingerprints remain separate incidents. They may be linked
as `possibly_caused_by`, `upstream_of`, `downstream_of`, or `shares_trace_with`.
Shared traces alone never automatically merge incidents.

The orchestrator may assign related incidents to an existing agent when its tool
scope and capacity cover both services. Each incident retains its independent
identity and evidence.

## State model

Detection state and investigation state are separate.

Detection states:

```text
active -> quiet -> active
              -> ended
```

Investigation states:

```text
queued -> claimed -> preparing_workspace -> investigating
       -> forming_hypothesis -> writing_report -> completed
```

Exceptional investigation states are `failed`, `timed_out`, `cancelled`, and
`superseded`.

Defaults:

- A generation becomes quiet after 15 minutes without a matching occurrence.
- Silence never automatically marks the investigation resolved.
- A return within two hours on the same deployment reopens the generation.
- A return after two hours creates a linked generation.
- Rules may override quiet and reopen intervals.
- Severity escalation immediately reprioritizes and notifies the active agent.
- Noise returning while an investigation is active does not launch another agent.

## Deduplication transaction

The processing transaction must atomically:

1. Assert or insert the unique `record_id` contribution.
2. Find or create the incident family and current generation.
3. Add the occurrence and update deterministic aggregates.
4. Add selected evidence references.
5. Create an investigation and outbox message only if policy requires it and no
   active investigation already exists.

CockroachDB unique constraints are correctness controls, not merely indexes.
Serializable retry handling is required for concurrent workers.

## Rule rollout

Rules pass schema validation, type validation, resource-limit validation, fixture
tests, and shadow evaluation before enforcement. Reload builds an immutable rule
set and swaps it atomically. Failure leaves the previous set active. Rule changes,
suppression, and rollback are audited.
