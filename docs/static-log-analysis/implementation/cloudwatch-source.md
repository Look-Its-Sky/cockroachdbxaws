# CloudWatch Logs Source Adapter

Normative. MUST, MUST NOT, SHOULD, and MAY carry their usual force.

`internal/cloudwatch` is the pull-based, regional Amazon CloudWatch Logs source
adapter. source-adapters.md makes OTLP push the primary path; this is the
retrieval path for services that write to CloudWatch and do not export OTLP.

## What this adapter owns, and what it does not

It owns source authentication, cursors, native identity extraction, and
transport retries. It does not own rules, grouping, incident lifecycle, or
redaction policy: it applies the policy it is given, and everything it emits has
already been through it.

## The ordering that everything else rests on

A checkpoint update and a journal acknowledgement cannot be one transaction
across CloudWatch and local storage. The order is therefore fixed and MUST NOT
be rearranged:

```text
read -> redact -> map -> deliver to the sink -> sink reports a durable write
     -> and only then advance the checkpoint
```

A crash anywhere in that sequence loses the checkpoint, never the records. The
next cycle rereads a bounded overlap, and the reread events hash to the
identifiers they already had, so the replay contributes nothing new.

The adapter MUST NOT advance a checkpoint on anything less than
`Ack.Acknowledged`. This is the single mistake the ordering exists to prevent,
and it is why `Ack` carries an explicit acknowledgement rather than being
inferred from the absence of an error.

## Record identity

Identity is `cw:v1`, defined in identity-and-admission.md as

```text
SHA-256("cw:v1" || D(account) || D(region) || D(log_group) || D(log_stream) || D(event_id))
```

over length-delimited components. `event_id` is CloudWatch's own native event
identifier.

An event without a native event ID MUST be refused as `ErrUnusableRecord`. There
is no fallback. Content hashing is prohibited, because two legitimate identical
messages do occur — the same error logged twice by the same container is normal
— and hashing them together would merge two occurrences into one and undercount
an incident.

`account` and `region` come from the adapter's own authenticated configuration
and MUST NOT be read back from an API answer. An identifier built from an
unauthenticated locator is forgeable by whatever produced the locator.

## Service identity

CloudWatch events carry no authenticated service identity and a claim inside a
message is untrusted. The log group's `Service` and `Environment` are therefore
operator configuration, and both MUST be inside the envelope's allowed sets. A
configuration that names a service outside its own allow-set authorizes less
than it declares and MUST be refused at construction, not per record.

## Time

- `Timestamp` is the producer's event time.
- `IngestionTime` is when CloudWatch accepted the event, and is the adapter's
  observed time.

CloudWatch filters on **event time**, so an event written late can appear behind
a position the adapter has already passed. The bounded lookback overlap exists
for exactly that gap.

CloudWatch's `endTime` is inclusive while a `Query` is half-open, and both
bounds are milliseconds. The adapter rounds the start down and the end up. That
direction is deliberate: a slightly wide window returns a duplicate, which
stable identity makes harmless, and a slightly narrow one drops an event nothing
would ever read again.

## Failure classification

| Class | Meaning | Recovery |
|---|---|---|
| `ErrThrottled` | the source refused for rate | back off and retry |
| `ErrSourceUnavailable` | any other source API failure | retry |
| `ErrRegionalBoundary` | a read would cross account or region | refuse; operator fault |
| `ErrUnusableRecord` | this one event cannot become a record | record-local; siblings unaffected |
| `ErrNotAcknowledged` | ingestion reported no durable write | do not checkpoint; retry |

A valid empty result is **not** a failure and MUST be counted separately.
Telling "the source answered with nothing" apart from "the source did not
answer" is a requirement: conflating them makes a healthy quiet log group look
like an outage and makes a real outage look like silence.

`ErrRegionalBoundary` is never record-local. Quarantine deletes durable
payloads, so treating a misconfigured endpoint as a per-record fault would
destroy every acknowledged record for what is a deployment typo.

## The ingestion boundary

The adapter delivers through `cloudwatch.Sink`:

```go
IngestRecords(ctx, envelope, []model.NormalizedLog) (Ack, error)
```

This is deliberately **not** `pipeline.Service.Ingest`. That entry point takes
OTLP bytes and derives `otlp:v1` identity from a producer-assigned record UID.
Routing a CloudWatch read through it would discard the native event ID that
`cw:v1` is defined over, and that ID is the whole reason an overlapping replay
is harmless.

`pipeline.Service.IngestRecords` is the second entry point, and
`internal/cloudwatch/cwsink` adapts it to `Sink`. `IngestRecords` MUST enforce
everything `Ingest` enforces:

- the envelope's region must be the replica's region;
- the record-local normalized-size bound, reported as a record-local rejection
  rather than a whole-batch failure;
- an acknowledgement only after the journal commits;
- the journal's own redaction-policy and boundary validation, unchanged.

What it skips is only decoding and normalization, which the source has already
performed under this deployment's policy. It MUST NOT become a way to put
records into the journal that `Ingest` would have refused.

All records in one `IngestRecords` call MUST share one batch identity. The
journal appends under a single batch and refuses any record that disagrees with
it; checking at the entry point turns a caller bug into a rejected request
rather than a whole-append failure that reads like unavailability.

## Duplicate suppression

The adapter remembers record identities it has already delivered and skips them.
This is an **optimization, not the guarantee**. The guarantee is the stable
record identity: the journal deduplicates on it, so a suppression miss costs a
redundant append and nothing else. No behaviour may be built on suppression
having happened.

## Counters

Counters are cumulative, categorical, and process-lifetime. They carry no
labels: operations.md forbids unbounded label cardinality, and a log stream name
is unbounded — an account can hold hundreds of thousands of them, and each would
become a time series that never retires.

`RecordsRead`, `DuplicateRecords`, `RecordsDelivered`, `RecordsRejected`,
`Throttled`, `SourceFailures`, `EmptyResults`, `CheckpointCommits`, and
`BoundaryRejections`.

## The role that runs it

`log-analysis cloudwatch` is the production driver for this adapter. It opens a
journal, a separate checkpoint volume, and the CockroachDB store, binds no
listener, and runs both the poll worker and process worker against the same
coordinator. `log-analysis source` retains the source-only behavior for adapter
diagnostics but is not a deployable production topology because its journal has
no owner that can drain it. The configuration surface is in runtime.md.

The checkpoint volume MUST be separate from the journal volume. The two have
different lifetimes: a journal is drained and may be rebuilt, and a checkpoint
rebuilt alongside it would reread the whole lookback window.

On drain and shutdown the poller is stopped before the process worker and before
the checkpoint volume is closed, so a cycle in flight can still commit the
checkpoint for what it has already delivered and no new journal work races the
drain.

## Still open

- Only one source type exists. `SourceType` admits `cloudwatch` and `otlp`; a
  third would need its own identity version, not a reuse of `cw:v1`.
- The combined role uses the same non-blocking enrichment coordinator as OTLP.
  A real enrichment provider and late-answer context versioning remain absent.
