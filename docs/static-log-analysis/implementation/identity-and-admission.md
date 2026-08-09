# Identity, Trust, and Admission

## Trusted source envelope

Every admitted batch MUST have an authenticated envelope established outside
application-controlled log attributes:

```text
source_type
source_account
region
environment or allowed environment set
allowed service identity set
collector/source instance identity
credential identity
received_at
```

For OTLP, the regional Collector authenticates to ingestion using workload
identity or mutually authenticated TLS. For CloudWatch, the adapter derives the
envelope from its regional AWS credentials and configured account/log groups.

An authenticated envelope is not necessarily addressed to the replica that
received it. An envelope whose region is not the replica's configured scope MUST
be rejected permanently and counted, before the request reaches the journal whose
immutable regional boundary would otherwise report it as retryable. Any batch the
journal permanently refuses is likewise a permanent rejection, never
unavailability, so a transport cannot replay a poisoned batch forever and starve
its valid siblings.

Application attributes such as `service.name`, region, and environment are claims.
A claim outside the envelope's allowed set MUST be rejected, audited, and counted.
Missing claims may use an envelope default only when the source configuration is
dedicated to exactly one value.

## Record identity

Identity versions are immutable. Each one is recorded in `record_id_version`
using the exact token below, and each produces a SHA-256 digest rendered as 64
lowercase hexadecimal characters. A record whose identifier does not have its
version's shape MUST be rejected, because a later algorithm must never silently
rewrite history.

| `record_id_version` | Source | `identity_quality` |
|---|---|---|
| `otlp:v1` | Producer-assigned OTLP record UID | `native` |
| `cw:v1` | CloudWatch native event identity | `native` |
| `derived:v1` | Canonical-encoding fallback | `derived` |

A record MUST NOT declare a quality other than the one its version produces, or
the fallback's use escapes the measurement it exists to be visible to.

### CloudWatch version 1

```text
SHA-256("cw:v1" || D(account) || D(region) || D(log_group) || D(log_stream) || D(event_id))

D(s) = uint32 big-endian byte length of s, followed by the UTF-8 bytes of s
```

The components are length-delimited because, unlike the fixed-width UUID of
`otlp:v1`, a log group and a log stream are free-form operator-chosen names.
Plain concatenation would let `(group "ab", stream "c")` and `(group "a",
stream "bc")` hash to one identifier, and two distinct streams sharing a
`record_id` would merge their occurrences into one incident contribution and
lose the other's evidence. Delimiting is part of the definition of `cw:v1`, not
a later change to it: no build has ever emitted this version.

An adapter MUST derive `account` and `region` from its own authenticated
configuration and MUST refuse an event whose stream reports a different account
or region, because an identifier built from an unauthenticated locator is
forgeable by whatever produced the locator.

### OTLP version 1

The preferred identity is:

```text
SHA-256("otlp:v1" || trusted_source_instance || log.record.uid)
```

`log.record.uid` MUST be a valid UUIDv7-compatible identifier created by a direct
OTLP producer before its first export attempt, or deterministically derived by a
collection receiver from native source identity/position. A fresh random ID MUST
NOT be added after an upstream retry boundary. The UID is a uniqueness claim, not
authorization; pairing it with the authenticated source envelope prevents another
source from colliding accidentally or impersonating service identity.

### Fallback version 1

Recorded as `derived:v1`. Legacy inputs lacking a trusted UID hash a
length-delimited canonical encoding of:

```text
trusted source envelope
service
event timestamp
observed timestamp
trace ID
span ID
severity number
safe body
sorted selected stable attributes
```

Map keys are UTF-8 byte-sorted; values use deterministic Protobuf encoding. The
fallback is explicitly marked `identity_quality=derived`, measured, and alerted.
It MUST NOT be the normal OTLP path.

## Batch identity

An ingestion batch ID identifies a transport attempt, not a domain occurrence.
It is useful for diagnostics and fast duplicate-batch detection but MUST NOT
replace per-record deduplication.

## Admission stages

```text
authenticate envelope
-> enforce compressed and uncompressed size limits
-> decode bounded OTLP structure
-> validate trusted claims
-> universal and service redaction
-> assign/validate identities
-> classify protection
-> apply capacity policy
-> synchronized journal commit
-> acknowledge
```

Defaults before measurement:

- Maximum compressed request: 4 MiB.
- Maximum uncompressed request: 16 MiB.
- Maximum records per request: 10,000.
- Maximum safe normalized record: 256 KiB.
- Maximum projected aggregate materialization: 16 MiB at both raw admission
  and normalization boundaries.
- Maximum attribute nesting: 16 levels.
- Maximum raw structural nodes: 320,000 resource, scope, record, attribute,
  container, and value nodes.

Limits are configurable downward per source. Oversized individual records use
partial success; oversized requests are rejected before allocation grows without
bound. Raw-wire record indexes are assigned before partial rejection. The
returned decoded request contains accepted records only (never empty placeholder
records), while each rejection retains its original raw-wire index. Decode also
returns a strictly increasing original-index mapping aligned with the flattened
accepted request. Every later record-local boundary, including normalized-size
enforcement, MUST consume that mapping; a missing, mismatched, duplicate, or
nonmonotonic mapping is rejected rather than reconstructed by guesswork.
Normalization consumes that accepted-record mapping, uses original indexes for
its own record-local rejections, and returns a new mapping aligned only with
successfully normalized records for the normalized-size boundary.

Raw admission projects resource and scope payload weights once per descendant
record, including conservative redaction expansion and node/container overhead,
before protobuf materialization. Normalization independently projects decoded
record, resource, scope, trusted-envelope clone, and embedded structured-JSON
weights before batch-ID generation. Either aggregate-budget failure rejects the
request as one opaque request-level error with no partial records. Limits are
configurable downward. Embedded JSON rejected for duplicate decoded keys is
projected conservatively as one opaque value, matching its single withheld-value
output; an exact reported projection is accepted and one byte less is rejected.

The normalization boundary owns returned metadata: after validating the trusted
envelope, it clones it and gives every returned record independent copies of the
allowed-service and allowed-environment slices. Explicitly nil options, nil
policies, nil/typed-nil ID sources, and invalid materialization limits are
configuration errors rather than defaults or panics. Trusted-envelope
prohibited-content validation runs once before ID generation and record
processing.

## Protected-input classifier

Rule compilation produces a conservative classifier over source, service,
environment, severity, event type, and required attribute presence. A record is
protected if any enabled rule could consume it. Uncertain or failed classification
means protected.

The classifier MUST NOT evaluate thresholds, mutate rule state, or trigger an
incident. It only determines shedding eligibility.

Until production capacity behavior is validated, shedding is permitted only for
sources explicitly configured as eligible and records classified as unprotected
DEBUG/INFO. All other records receive backpressure when they cannot be persisted.
