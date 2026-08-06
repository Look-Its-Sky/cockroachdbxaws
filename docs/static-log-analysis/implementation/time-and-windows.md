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

## Late records

- A record newer than the watermark participates normally.
- A record behind the watermark but inside retained state may update evidence and
  an open generation.
- A late record MAY satisfy a threshold only while the generation has never been
  completed and the threshold evaluation horizon is retained.
- A late record MUST NOT launch another agent for an active or already completed
  investigation by itself.
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
