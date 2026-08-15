# Canonical Data Model

All stored and transmitted domain structures include `schema_version`. Unknown
fields are rejected in operator-managed configuration but tolerated by readers
of forward-compatible event payloads.

## Normalized log

```go
type NormalizedLog struct {
    SchemaVersion     string
    RecordID          string
    IngestionBatchID  string
    Source            SourceIdentity
    EventTime         time.Time
    ObservedTime      time.Time
    TimestampInferred bool
    ObservedTimeInferred bool
    SeverityNumber    int32
    SeverityText      string
    SeverityClass     string
    Body               SafeValue
    Attributes         map[string]SafeValue
    ResourceAttributes map[string]SafeValue
    ScopeAttributes    map[string]SafeValue
    Service            ServiceIdentity
    Deployment         DeploymentIdentity
    Correlation        CorrelationIdentity
    Exception          *NormalizedException
    Redaction          RedactionMetadata
    RawReference       *RegionalLogReference
}
```

The actual implementation uses OTLP Protobuf types at the transport edge and
explicit internal types for domain processing. Arbitrary OTLP maps must not leak
directly into rule or persistence code.

## Minimum ingestion identity

A record is acceptable when it has:

- Source identity.
- Observed timestamp.
- A body or event name.
- Region.
- A native record ID or sufficient stable inputs to construct one.

Service-specific agent execution additionally requires service, environment,
severity or a rule-recognizable event type, and enough safe content to calculate
a useful fingerprint.

## Missing data policy

- Missing event time uses observed time and is marked inferred.
- Missing observed time uses event time and is marked with separate observed-time
  inference provenance. This deterministic fallback keeps upstream replays on
  the same `derived:v1` identity.
- A record missing both event and observed time is rejected locally; there is no
  stable time from which to infer either value.
- Missing severity becomes `UNSPECIFIED`; the normalizer does not guess.
- A source-specific default environment is allowed only for a source dedicated
  to one environment.
- Missing service identity prevents service-specific agent execution.
- Missing deployment groups under `unknown-deployment` and may be enriched later.
- Valid attributes survive malformed siblings; normalization errors are counted.
- Unidentifiable records retain only safe quarantine metadata.
- Repeated metadata failures create a telemetry data-quality incident.

## Record identity

Native source identity is preferred.

CloudWatch identity includes account, region, log group, log stream, and native
event ID. Direct OTLP producers must create `log.record.uid` before their first
at-least-once export attempt. Collector receivers for files or cloud sources may
derive it from a native stable source position. A random ID added only after an
OTLP retry boundary is prohibited because a retried record would receive a new ID.
The analysis service validates the ID with the trusted source envelope. Legacy or
unsupported inputs fall back to a versioned derived identity from source, service,
event and observed times, trace and span IDs, severity, safe body, and selected
stable attributes. Fallback use is measured and alerted.

Content hashing alone is prohibited because two legitimate identical messages
may occur. Every derived identity stores `record_id_version`. Collision behavior
is observable, and a later identity version never silently rewrites history.

## Error fingerprint

The versioned fingerprint includes, when available:

- Service identity.
- Error code.
- Exception type.
- Normalized message template.
- Selected in-application stack frames.
- Operation or normalized route.

It excludes request, trace, session, customer, container, and host identifiers;
timestamps; memory addresses; raw line numbers; and library-only stack frames.
Dynamic IDs, timestamps, and addresses in messages become typed placeholders.

Each fingerprint stores a safe explanation of the fields that produced it.

## Evidence

CockroachDB stores selected redacted evidence and references rather than the full
raw stream. A regional reference contains source type, region, source locator,
time range, access classification, and expiry time. It must never contain source
credentials.

Evidence records are immutable. Corrections or new enrichment create new evidence
versions and preserve provenance.
