# Source Adapters

Source adapters deliver source records into the canonical ingestion boundary.
They own source authentication, cursors, native identity extraction, and transport
retries. They do not own rules, grouping, redaction policy decisions, or incident
lifecycle.

## OpenTelemetry

The initial source is regional OTLP exported by the existing OpenTelemetry
Collector. Applications push logs to the Collector; the Collector exports batches
to the analysis service over OTLP/gRPC, with OTLP/HTTP as a supported fallback.

The Collector must attach or preserve service name, environment, region,
deployment attributes, trace/span IDs, scope, event and observed timestamps, and
native record identity when the producer provides it. Direct OTLP producers create
a UUIDv7-compatible `log.record.uid` before their first export attempt. Collection
receivers may instead derive a stable UID from a native file offset or source event
ID. The Collector MUST NOT assign a new random UID to each receipt of an otherwise
unidentified direct OTLP record. Universal redaction occurs before its persistent
queue.

When neither native option is available, the analysis service computes the
documented deterministic `derived:v1` identity after redaction. Its use is
counted by `static_log_analysis_derived_identity_total`. A present but malformed
UID never takes this fallback, because doing so could give one source record two
identities across versions of a producer.

The local deployment exposes Collector `0.157.0` on ports 4317/4318 and on the
stable `static-log-analysis-ingress` Docker network. Another repository either
joins instrumented services directly to that network or adds a second logs-only
OTLP exporter to its existing Collector.

## Grafana Alloy

Grafana Alloy, if adopted in another deployment, plays the same collector role:
it pushes OTLP to the analysis service. The core analysis service does not poll
Alloy. Any Alloy-specific labels are mapped into canonical resource attributes at
the collection edge.

## Amazon CloudWatch Logs

The CloudWatch adapter is pull based and regional. Its checkpoint is maintained
per AWS account, region, log group, and log stream. It records the last safely
accepted source position only after corresponding records have reached the
analysis journal.

`FilterLogEvents` reads a whole log group, so the next group-wide retrieval
window is anchored to the greatest stored stream position, then moved backward
by the bounded lookback. Per-stream positions remain useful for native cursor
identity and diagnostics, but an older stream that has gone quiet must not pin
the whole group in the past. A successful exhaustive group query has already
searched that quiet stream through the same horizon; the overlap covers bounded
late delivery on every stream.

The adapter must:

- Use native CloudWatch event IDs in `record_id` construction.
- Expect overlapping pages, duplicate events, and late delivery.
- Use bounded lookback overlap so recent late events can be recovered.
- Paginate until the current retrieval horizon is reached.
- Respect AWS throttling with exponential backoff and jitter.
- Separate source API failure from a valid empty result.
- Discover streams incrementally without rescanning unbounded history.
- Maintain region-local credentials and endpoints.
- Expose checkpoint age, API throttling, records read, duplicate records, and
  source lag metrics.

A checkpoint update and journal acknowledgement cannot be one transaction across
CloudWatch and local storage. Recovery therefore intentionally rereads an overlap;
stable record IDs make the replay harmless.

## Adapter contract

Every adapter supplies:

```text
source type and source version
account/project/cluster identity
region
native source locator
native record ID when available
event and observed timestamps
raw payload in memory for immediate redaction
safe reference metadata
checkpoint or acknowledgement callback
```

The authenticated adapter or Collector adds a trusted envelope containing source
account, region, environment, allowed service identities, collector instance, and
credential identity. Application-provided attributes are untrusted claims. A
service or region claim that conflicts with the trusted envelope is rejected and
audited; it is never silently accepted or rewritten.

An adapter cannot declare a record successfully delivered until the ingestion
service has acknowledged its durable journal write.

## Adding a source

A new source requires identity fixtures, cursor/replay tests, time and severity
mapping, metadata mapping, regional-boundary review, redaction fixtures, outage
behavior, rate-limit handling, and an acceptance test demonstrating harmless
duplicate delivery.
