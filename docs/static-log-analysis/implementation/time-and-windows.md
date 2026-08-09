# Time Windows and Lifecycle Boundaries

## Timestamp validation

`event_time` is used when present and within 24 hours before or five minutes after
`observed_time`. Values outside this bound are retained as source metadata but
processing uses `observed_time` and sets `timestamp_inferred=true` plus a reason.

All times are UTC with nanosecond-capable storage. Rule duration arithmetic uses
monotonic test clocks where possible and never local wall-clock timezone rules.

## Window interval

A window of duration `D` evaluated at watermark `W` is half-open:

```text
(W - D, W]
```

A record exactly at `W-D` is excluded; a record at `W` is included. Count and
denominator use unique `record_id` contributions only.

The per-rule, per-group watermark is:

```text
maximum valid event_time observed - allowed_lateness
```

Default allowed lateness is two minutes. Watermarks never move backward. State is
retained through `window + allowed_lateness + reopen_window` where needed for
evidence, but inactive counters MAY be compacted into summaries.

In-memory rule state is bounded by exactly that retention. Records behind the
horizon are released, and a family whose horizon has passed with nothing
retained is dropped once it can no longer suppress anything: immediately when no
investigation is active, and otherwise once the reinvestigation interval below
has expired, after which a new investigation is permitted anyway.

A deployment episode's identity is the pair `(deployment_id, episode_start)`,
and it is assigned once, when the episode begins. Compaction releases payloads;
it never re-derives that pair from whichever records survived, because storage
keys a generation on it and a re-derived pair would give one real episode a
second durable generation. The display ordinal is a separate, positional value.
While a family is retained, a compacted episode keeps an ordinal-only shell, so
a live generation's ordinal and the agent-visible context version never move
backwards. Once the family shell itself is released, a later recurrence of the
same fingerprint is a new family and its ordinals start again at one. Nothing
durable depends on that: `incident_generations` is keyed on the opaque episode
identity, never on the ordinal, and `context_version` is advanced monotonically
in the database, so a restarted in-memory ordinal cannot move a stored context
version or an existing generation.

Compaction also releases the local duplicate-suppression identity:
`records_seen` in CockroachDB is the durable global gate, so a compacted
identity delivered again still contributes exactly one occurrence. Identities a
worker still holds in flight are exempt until it releases them.

Records at or newer than the watermark are provisional. While they are
provisional, deployment episodes are reconstructed in event-time order; equal
timestamps use `deployment_id`, then `record_id`, as the deterministic tie-break.
An irreversible investigation request is evaluated only from topology strictly
behind a finalized frontier.

An idle group cannot advance its event-time watermark. Its deadline timer advances
a separate finalization frontier to
`min(successor(maximum_event_time), now - allowed_lateness)`, where `successor` is
one nanosecond later when representable. The boundary is strict: an event time `T`
remains provisional at `T + allowed_lateness` and finalizes immediately after that
deadline. This timer does not redefine or move the stored event-time watermark.
Threshold evaluation retains finalized candidate endpoints, so a later watermark
jump cannot skip a cluster that previously satisfied its window. After either
frontier has passed an event time, a later arrival strictly behind that frontier is
evidence-only and cannot rewrite episode topology or launch an agent.

Persistence consumes an opaque M1 decision, not a caller reconstruction of the
current display ordinal. A normal record receives that decision only after the
record's own event-time, deployment, and record-ID tie-break position is strictly
behind the irreversible frontier. The decision binds the record to the finalized
deployment episode start and deterministic storage key. It also carries the
generation's detection status, its observed-time-bounded quiet and reopen
instants, and M1's own late classification. Those belong to the finalized
generation, which alone can see the episode's other contributions; re-deriving
them from a single record is rejected by the storage boundary. Only a record the
frontier had already passed can be late, so a late non-evidence decision is
invalid by construction. Until then the claimed
journal record remains pending/replayable; M4 rejects a missing, forged, stale, or
record-mismatched decision. Evidence-only decisions attach only to topology M1 has
already frozen.

This strict finalization comparison does not change the rule window: once an
endpoint `W` is finalized, its membership remains `(W - D, W]` and includes `W`.

## Late records

- A record at or newer than the watermark participates provisionally.
- A record strictly behind the watermark but inside retained state may update
  evidence.
- A late record MUST NOT change finalized episode topology or launch an agent by
  itself.
- Very late records become evidence-only occurrences and are labeled accordingly.

Every rule fixture MUST include exact-boundary and late-arrival examples.

## Rate denominators

Rate rules explicitly declare a denominator selector, normally a request-complete
or operation-complete record/span-derived health signal. Absence of a declared
denominator is a configuration error. A rate threshold is not evaluated until its
minimum denominator volume is satisfied.

## Quiet and reopen

- Quiet time is measured from the latest unique matching event time, bounded by
  observed time so a future-skewed record cannot hold an incident open forever.
- Default quiet period is 15 minutes.
- Quiet changes detection state only; it does not resolve an investigation.
- Recurrence within the default two-hour reopen window and same deployment
  reactivates the same generation.
- Recurrence after that window creates a linked generation.
- Deployment change always creates a linked generation.

## Reinvestigation

A completed investigation is not automatically reopened for every recurrence.
A new investigation may start only when one of these deterministic conditions is
true:

- A later generation follows a deployment change and no active investigation
  covers the family.
- Severity rises by a configured material level, such as ERROR to FATAL.
- The previous report's configured reinvestigation interval has expired; default
  24 hours.
- A rule explicitly declares that each generation requires investigation.

Otherwise the new generation links to the completed report and accumulates
evidence without agent execution. Human suppression or resolution remains audited.
