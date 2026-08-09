# Journal Contract

## Ownership

One process owns one Pebble database on one persistent volume. Shared mounting or
multi-process opening is prohibited. The journal directory includes an owner and
format manifest; incompatible or concurrently held ownership prevents startup.
The immutable manifest also fixes the region, tenant ID, maximum classification,
and exact redaction-policy version, including while the journal is new or fully
drained. All are bounded categorical identifiers; classification is one of
`PUBLIC`, `INTERNAL`, `SENSITIVE`, or `RESTRICTED`. A contradictory reopen is
refused rather than reconfiguring the volume in place. The manifest is readable,
so a coordinator can refuse to start on a contradiction with its own
configuration instead of discovering it one record at a time.

## States

```text
pending -> claimed -> committed -> compactable -> removed
                    \-> quarantined
```

- `pending`: durably accepted but not claimed.
- `claimed`: worker holds a time-bounded local claim.
- `committed`: corresponding CockroachDB transaction succeeded.
- `compactable`: post-commit safety delay elapsed.
- `quarantined`: deterministic processing cannot continue safely.

Claims expire and return to pending after process failure. Claim identity and
attempt count are persisted atomically. A worker may re-adopt the unexpired
claims it already owns after a restart, which observes state without changing it
and neither extends an expiry nor counts an attempt. A worker still retrying a
cohort may renew its unexpired claims by a further claim TTL; renewal does not
count an attempt, and because the claim token is derived from the expiry it
retires the previous token, so a holder of the old token fails closed. Renewal
shares committing's lost-response idempotence: a member this journal already
committed under the same token is satisfied rather than stale, so one lost
response cannot make a whole cohort's valid claims unrenewable. Quarantine stores safe metadata and reason,
never unsafe original content.

## Atomic append

One synchronized Pebble batch MUST write:

```text
batch metadata
all admitted safe records
pending indexes
byte and priority accounting deltas
format version reference
```

Only successful `Batch.Commit(pebble.Sync)` permits OTLP acknowledgement. Partial
disk writes cannot expose a subset of a transport batch as acknowledged.
Before the batch rewrites its format reference, the current reference is verified;
a corrupt reference latches the journal unready instead of being silently repaired.
Append also proves that every newly encoded record can be decoded before it can
acknowledge the transport batch.
Append accepts at most 10,000 admissions, aligned with the transport admission
limit. Claim, batch-commit, and compaction calls accept at most 1,000 transitions;
the exact limit is accepted and one over is rejected before allocation or storage
iteration.
Claim expiry restoration and new claims share that 1,000-record transition budget.
If expiry cleanup consumes the budget, a claim call may return fewer than its
requested limit, including zero, and the caller retries without a sleep.

## Keys

The logical keyspace is:

```text
meta/format
meta/owner
batch/{batch_id}
record/{record_id}
pending/{priority}/{received_time}/{record_id}
claim/{expiry}/{record_id}
committed/{committed_time}/{record_id}
quarantine/{record_id}
```

Exact binary encoding is versioned and tested through golden fixtures. Keys use
length-delimited components, not ambiguous string concatenation.

The directory format manifest is stricter than a Protobuf payload schema. This
implementation opens only directory format `1.0`; an unknown minor as well as an
unknown major refuses startup until an explicit, tested migration supports it.
Keys start with a key-format byte and namespace byte. Variable components are
unsigned-varint-length-delimited, priorities are one byte, and timestamps are
sign-bit-flipped big-endian Unix nanoseconds. Persisted values carry a type/version
magic and SHA-256 checksum.

Every persisted timestamp is nonzero UTC, losslessly round-trips through Unix
nanoseconds, and is valid as a Protobuf timestamp. Claim-TTL and safety-delay
addition is checked at the exact representable boundary. A regional raw reference
requires nonzero `from`, `to`, and `expires_at`, with `from <= to < expires_at`.

Record replay equivalence excludes exactly two transport-attempt fields:
`batch_id` and `source.received_at`. Every other normalized field participates in
the deterministic Protobuf semantic digest. The first durable record remains the
canonical payload; a replay with a different attempt ID/time is idempotent, while
a same-ID change to body, identity, service, event time, deployment, redaction,
regional boundary, or any other semantic field is an identity conflict. Batch
metadata keeps immutable, sorted original member descriptors (`record_id`, semantic
digest, and priority) separately from sorted live membership. Compaction and
quarantine shrink only live membership, so an exact original-batch replay remains
idempotent without resurrecting removed records, while changed membership or a
changed compacted member remains an identity conflict. Batch metadata and reverse
live-membership indexes shrink with live membership. Canonical original-membership
indexes remain while the batch has any live member, so cross-batch redelivery is
checked against compacted descriptors and an exact earlier-batch replay cannot
resurrect them. Batch metadata and both index kinds are removed when the last live
member is quarantined or compacted. A record is limited to 64 live references and
64 retained original-membership references by default.

Every claim exposes a sealed, versioned replay identity containing the exact
durable-envelope semantic digest and admission priority already stored by M3.
Persistence recomputes it using the Store's immutable tenant/classification
boundary before its retry callback, persists digest plus priority/version in the
global record gate, and compares them on every duplicate. It does not substitute a
record-only hash, so classification or priority conflicts remain detectable after
the journal payload and history have compacted.

## Processing and commit

A worker claims a bounded batch, performs deterministic processing, and commits
record contribution, incident updates, evidence, and any outbox row in CockroachDB.
Only after database success does it atomically mark journal records committed.

If the process crashes after database commit but before journal update, replay is
safe because CockroachDB's `record_id` uniqueness makes the transaction idempotent.

## Compaction

Committed records remain for a default 15-minute safety delay. Compaction deletes
record payloads and indexes in bounded batches and updates accounting atomically.
Batch metadata may remain until every member is removed.

Quarantine is permitted only from `claimed` with the current unexpired claim
token. It deletes the normalized payload and batch references atomically and
retains only record identity plus categorical reason, time, and attempt metadata.
Committed state retains the last claim token until compaction so an identical
lost-response retry is idempotent while any stale/different token still fails.

## Capacity

Accounting uses actual encoded journal bytes plus conservative storage overhead,
not raw input or Collector debug output. Capacity is both a byte limit and a
minimum-free-filesystem threshold. The stricter limit governs admission.

Accounting covers the exact bytes of every dynamic logical key and value plus a
64-byte conservative per-entry overhead. Fixed manifest/accounting keys are not
charged. The synchronized batch also persists counts for all four priority
classes; reopen recomputes both byte and priority accounting from records and
refuses any mismatch. A pending record persists one versioned, checksummed
transition reserve. Its 198-byte payload is derived from the exact worst-case
dynamic transition delta: a 128-byte owner plus its one-byte length-prefix growth,
a fixed 70-byte claim token, and the one-byte reduction from a pending key to a
claim key. Deleting the reserve key/value and its charged overhead adds further
conservatism. Every claim deletes the reserve and every claim expiry restores it;
claimed and committed records have none. Thus a record accepted at the exact
capacity/free-space boundary cannot be stranded by later journal transitions.
Claim owners are limited to exactly 128 bytes.

The prohibited-content validator is a trusted boundary and exposes the exact
policy `Version()` recorded in the immutable manifest and required on every
record. The journal verifies structural results and version agreement but cannot
prove that an implementation actually scans content; a no-op validator is a test
fixture only and does not meet the production security contract.

## Corruption and upgrades

On detected corruption, the replica stops claiming and accepting data, becomes
unready, preserves the volume read-only where possible, and alerts. It MUST NOT
silently skip corrupt records.

Startup performs a full keyspace scrub. Live operations use constant-time manifest
checks plus validation of every record and index they touch, so bounded operations
do not become proportional to total journal size. Any synchronized write failure
latches the journal unhealthy.
Before deleting or replacing an index, a transition verifies its exact empty value,
state timestamp, and reserve presence or absence. Existing-batch replay verifies
canonical sorted/unique metadata plus live and retained-original bidirectional
record/batch references. New-batch admission checks all bounded retained descriptors
for the record before accepting a cross-batch redelivery.
Touched logical corruption is never repaired by the transition that encounters it.

Format upgrades require fixture tests for previous supported versions, a backup or
checkpoint, forward migration, restart verification, and documented rollback.
Unknown future format versions refuse startup.

## Required journal contract tests

Atomic synchronized append, duplicate identity, claim expiry, claim adoption and
renewal, crash at every state transition, database-commit/journal-update gap, quarantine, capacity accounting,
compaction, reopen, format upgrade, incompatible downgrade, and corruption refusal
are mandatory before release.
The normative harness uses real Pebble with a crashable filesystem to distinguish
synced from unsynced survival, a narrow package-private storage fault seam, and
subprocess abrupt exits. Tests never use sleeps.
