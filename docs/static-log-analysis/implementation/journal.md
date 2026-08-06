# Journal Contract

## Ownership

One process owns one Pebble database on one persistent volume. Shared mounting or
multi-process opening is prohibited. The journal directory includes an owner and
format manifest; incompatible or concurrently held ownership prevents startup.

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
attempt count are persisted atomically. Quarantine stores safe metadata and reason,
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

## Capacity

Accounting uses actual encoded journal bytes plus conservative storage overhead,
not raw input or Collector debug output. Capacity is both a byte limit and a
minimum-free-filesystem threshold. The stricter limit governs admission.

## Corruption and upgrades

On detected corruption, the replica stops claiming and accepting data, becomes
unready, preserves the volume read-only where possible, and alerts. It MUST NOT
silently skip corrupt records.

Format upgrades require fixture tests for previous supported versions, a backup or
checkpoint, forward migration, restart verification, and documented rollback.
Unknown future format versions refuse startup.

## Required journal contract tests

Atomic synchronized append, duplicate identity, claim expiry, crash at every state
transition, database-commit/journal-update gap, quarantine, capacity accounting,
compaction, reopen, format upgrade, incompatible downgrade, and corruption refusal
are mandatory before release.
