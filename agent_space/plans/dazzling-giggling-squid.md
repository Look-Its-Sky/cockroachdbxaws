This file holds two independent plans:

1. **Reconciling crashed runs** — approved, not yet implemented.
2. **Learning from engineers' decisions** — below, planned second.

They touch different code and can land in either order.

---

# Plan 1: Reconciling runs left behind by a crashed process

## Context

Both durable tables write a `running` row when work starts and overwrite it when
work finishes. If the process dies in between — Ctrl-C during development, a
panic, an OOM — the row stays `running` forever, because the only code that would
ever overwrite it lives in the process that just died.

The plan doc filed this as cosmetic ("stale `running` remediations aren't
reconciled at boot"). It isn't. Tracing the worker's dedup path turned up a bug
that silently discards incidents:

`worker/results.go:206`

```go
func (s *Store) Seen(investigationID string) bool {
	rec, ok := s.Get(investigationID)
	return ok && rec.Status != StatusRetrying
}
```

`Get` falls through to the journal on an in-memory miss, so after a crash:

1. The `investigations` row is left at `running`.
2. The heartbeat stops, the SQS visibility timeout lapses, the message is
   redelivered — the recovery path working exactly as designed.
3. The new process has an empty map, so `Seen` reads the journal, finds
   `running`, and returns **true**.
4. `worker/worker.go:112` logs *"already investigated, acknowledging the
   duplicate"* and calls `w.ack(msg)`.

The message is deleted and the incident is never investigated, while the log
claims the opposite. This contradicts the stated design in `main.go:116-118`:

> an investigation cut short here was never acknowledged, so SQS redelivers it
> once the visibility timeout lapses

There is already a precedent for the fix. `StatusRetrying` is excluded from
`Seen` for precisely this reason, and `TestRetryingRecordIsNotTreatedAsSeenAcrossRestart`
pins that behaviour. `StatusRunning` is the same class of non-terminal status and
was simply missed.

A stuck row also poisons downstream reads: `routes/remediation.go:123` rejects
`StartRemediation` while the record is `running`, so a stuck investigation cannot
even be remediated by hand.

**Outcome:** a crashed run is re-investigated rather than dropped, and no table
reports a dead run as live.

## Two problems, two mechanisms

Worth keeping separate — they do different jobs:

| | Job | Applies to |
|---|---|---|
| `Seen` fix | **Recovery** — let the redelivery re-investigate | `investigations` only |
| Boot sweep | **Honesty** — stop showing a dead run as live | both tables |

A sweep cannot substitute for the `Seen` fix (it races the redelivery), and the
`Seen` fix cannot substitute for the sweep. Remediations get no `Seen` fix
because there is nothing to recover *from*: the SQS message is deliberately
acked before the handoff (`worker/worker.go:183`), so no redelivery exists. The
sweep is the only mechanism there.

### Why boot-only, with no periodic re-sweep

Every in-process hang is already bounded and self-healing — `cfg.RunTimeout`
guarantees `Store.Finish` runs, and `jobTimeout` (`remediation/runner.go:42`)
guarantees `Runner.finish` runs. A panic in a worker goroutine is fatal to the
process. So a stale row can only be produced by a process exit, and every process
exit is followed by a boot. Boot-only is complete; a ticker would add a goroutine
that can never find anything.

## Changes

### 1. `worker/results.go` — stop dropping crashed investigations

Split `Get` so callers can tell where a record came from. A record in the
in-memory map is one *this* process started or finished; one that came back from
the journal may belong to a process that is gone.

```go
// get, plus whether this process is the one holding the record. A record from
// the journal alone may belong to a process that is no longer running.
func (s *Store) get(investigationID string) (rec Record, found, live bool) {
	s.mu.RLock()
	held, ok := s.byID[investigationID]
	if ok {
		rec = *held
		s.mu.RUnlock()
		return rec, true, true
	}
	s.mu.RUnlock()

	rec, found = s.recall(investigationID)
	return rec, found, false
}
```

`Get` becomes a thin wrapper (`rec, found, _ := s.get(id)`), so no caller changes.
`Seen` gains one case:

```go
func (s *Store) Seen(investigationID string) bool {
	rec, found, live := s.get(investigationID)
	switch {
	case !found:
		return false
	// the work still has to happen; that is what retrying means
	case rec.Status == StatusRetrying:
		return false
	// a running record this process does not hold was started by one that is no
	// longer here to finish it. The redelivery in hand is the retry.
	case rec.Status == StatusRunning && !live:
		return false
	}
	return true
}
```

This preserves same-process duplicate suppression: a genuinely in-flight run is
in the map, so `live` is true and `Seen` still returns true.

**Accepted trade-off:** two processes sharing a queue, where A is running an
investigation and A's heartbeat fails so B gets a duplicate delivery, now
investigate the same incident twice. It requires a heartbeat failure (already
logged at `worker/worker.go:258`), costs one wasted run, and `Finish` upserts so
nothing is corrupted. Losing an incident on every Ctrl-C is the worse failure —
this is an SRE tool, and dropping work silently is the one thing it must not do.

### 2. `worker/journal.go` — age-gated sweep

Add a package const for the reason string and:

```go
func (j *PGJournal) AbandonStale(ctx context.Context, olderThan time.Duration) (int64, error)
```

```sql
UPDATE investigations
SET status = $1, error = coalesce(nullif(error, ''), $2),
    finished_at = now(), updated_at = now()
WHERE status = $3 AND started_at < $4
```

Compute the cutoff in Go (`time.Now().UTC().Add(-olderThan)`) rather than passing
an interval. Return `tag.RowsAffected()` so boot can log a count.

Age-gated because `Seen`'s doc comment claims correctness "across two processes
polling the same queue", and an unconditional sweep would have process B mark
process A's live runs failed. `started_at` already exists — no new column, no
heartbeat.

### 3. `remediation/store.go` — unconditional sweep

```go
func (s *Solutions) AbandonStale(ctx context.Context) (int64, error)
```

Same `UPDATE` against `remediations`, but `WHERE status = 'running'` with no age
predicate. Document why the asymmetry is correct: the remediation pool is
single-process by construction — `Runner.inFlight` is an in-memory `sync.Map`
(`remediation/runner.go:83`), nothing coordinates across processes, and the queue
message is long gone. A row still `running` at boot has no owner by definition.

This also cleans up instantly, which matters for the demo: age-gating would leave
a crashed remediation showing `running` for 45 minutes.

**No candidate cleanup is needed.** `Runner.Run` only calls `r.save` with
candidates attached at `finish` (`runner.go:231`); the earlier saves at lines 176
and 193 carry none. A remediation that crashed mid-`fanOut` has zero `solutions`
rows, so there is nothing orphaned to reconcile.

### 4. `initialization.go` — wiring

Add `const staleSlack = 5 * time.Minute`.

In `initWorker`, in the branch where the journal was created successfully (~line
202), after the "verdicts persist" log:

```go
if n, err := journal.AbandonStale(sweepCtx, cfg.RunTimeout+staleSlack); err != nil {
	log.Printf("Worker: could not reconcile investigations left running: %v", err)
} else if n > 0 {
	log.Printf("Worker: %d investigation(s) were left running by a previous process; marked failed.", n)
}
```

`cfg` is already in scope from `queue.ConfigFromEnv()`. Use a
`context.WithTimeout` of `schemaLoadTimeout`, consistent with the other boot
calls. Log only when `n > 0` — the boot log is long enough already.

In `initRemediation`, the equivalent in the `NewSolutions` success branch (~line
261), calling `s.AbandonStale(ctx)`.

Ordering already works: `main.go:34-35` runs `initRemediation` then `initWorker`,
and both finish before `remediator.Start` (`main.go:85`) and `sqsWorker.Run`
(`main.go:92`). Both sweeps complete before any new work begins.

### 5. `.env.example`

No new variables. Add a sentence to the SQS section noting that
`WORKER_RUN_TIMEOUT` now also sets how long a `running` investigation row is left
alone at boot before being treated as abandoned.

## Tests

Reuse `fakeJournal` in `worker/journal_test.go:14` — it needs no changes.

**`worker/journal_test.go`** — model on the adjacent
`TestRetryingRecordIsNotTreatedAsSeenAcrossRestart`:

- `TestRunningRecordFromTheJournalIsNotSeen` — `before.Start(a)`, build a fresh
  `NewDurableStore(j)`, assert `!after.Seen("v1")`.
- `TestRunningRecordInThisProcessIsSeen` — `s.Start(a)` then `s.Seen("v1")` is
  true, so same-process duplicate suppression is not lost.

**`worker/worker_test.go`** — the regression test that matters, since it exercises
the actual drop. Uses the existing `fakeQueue` / `fakeAgent` / `newWorker`
helpers:

- `TestCrashedInvestigationIsReinvestigatedOnRedelivery` — preload `fakeJournal`
  with a `running` record under `sampleInvestigation`, attach a fresh
  `NewDurableStore`, call `w.handle(...)`, assert the agent ran once and the
  message was deleted once. **This fails on today's code** (agent runs zero
  times), which is what makes it worth writing.

**Live tests**, opt-in via env var like `journal_live_test.go:24`:

- `worker/journal_live_test.go` — insert one `running` row with an old
  `started_at` and one with a current one; call `AbandonStale`; assert only the
  old row flipped to `failed` and the fresh one is untouched. This is the whole
  point of the age gate, so it needs proving.
- `remediation/store_live_test.go` (new, gated on `REMEDIATION_LIVE_TEST=1`) —
  insert a `running` remediation, call `AbandonStale`, assert it flipped. Also
  closes part of the plan doc's "no live DB test for remediations/solutions" gap.

Both clean up with `t.Cleanup` and obviously-synthetic ids, following the
existing file's pattern.

## Verification

```bash
gofmt -l . && go vet ./... && go test -race ./...
```

Live, against `DATABASE_URL`:

```bash
JOURNAL_LIVE_TEST=1     go test ./worker/      -run Live -v
REMEDIATION_LIVE_TEST=1 go test ./remediation/ -run Live -v
```

End-to-end, which is the behaviour actually being fixed:

1. Confirm the bug first. Enqueue an assignment, and Ctrl-C the server partway
   through the investigation.
2. Check the row is stuck:
   `SELECT investigation_id, status, started_at FROM investigations WHERE status = 'running';`
3. Restart. Expect a boot line naming the count, and the row now `failed` with
   the abandoned reason.
4. Wait out the visibility timeout (default 120s) and watch the redelivery: the
   log must show `worker: <id>: starting`, **not** `already investigated`. That
   single line is the fix.
5. Repeat for remediation: Ctrl-C during a `fanOut`, restart, confirm
   `GET /agent/{id}/remediation` reports `failed` rather than spinning on
   `running`, and that `POST /agent/{id}/remediation` is now accepted instead of
   returning 409.

## Out of scope

- Automatically re-running an abandoned remediation at boot. It costs containers
  and model calls, and the verdict is already stored — `POST /agent/{id}/remediation`
  re-triggers it deliberately.
- A lease/owner column. The ceilings already answer "could this still be alive",
  and a real lease is schema plus heartbeat machinery for a topology that does
  not exist here.

---

# Plan 2: Learning from the engineer's decision

## Context

The pipeline currently ends at the pick. An engineer reads three candidates,
chooses one, and everything about *why* — including why the other two were
wrong — is lost the moment the page closes. The next incident starts from
exactly the same place, so the system never gets better at proposing fixes this
particular team will accept.

The goal is learning in the retrieval sense, not the training sense: record each
decision as prose, embed it, and recall it the next time a similar fault shows
up. Past choices then bear on future ones without touching a model.

The slot was left open: `CandidateSelected` is declared at
`remediation/candidate.go:115` and written nowhere in the codebase.

**Outcome:** an engineer's judgement — including "all three of these were wrong,
here is what I did instead" — becomes context the agent reads on the next
similar incident.

## What exists to build on

- **The vector index** is a langchaingo pgvector store (`utils/crdbvector/`),
  written to from exactly one place today: `POST /store` at
  `routes/private.go:35`, as plain prose with `metadata: {"source": "api"}`.
- **The prose shape that retrieves well** is set by the seeded incidents in
  `scripts/demo.sh:66` — *"INC-412 (date): symptom after commit X, which caused
  Y. Resolution: ROLLBACK. Why."* A decision record should mirror it.
- **`agent.Runner.recall`** (`agent/runner.go:196`) does the similarity search
  and discards metadata, keeping only `PageContent`. `buildPrompt`
  (`agent/runner.go:340`) frames the results.
- **`buildProposalPrompt`** (`remediation/patch.go:280`) assembles markdown
  sections — "## The incident", "## What triage found in the code", "## The
  files as they stand now". A new section drops straight in, and because
  `Repair` reuses it (`patch.go:365`) precedent reaches the repair round for
  free.
- **`Verification.Summary()`** (`remediation/candidate.go:90`) already produces
  the objective rejection reason in plain words, and is careful never to conflate
  a build with a test run.

### The constraint that decides the storage layout

The store's filters are equality-only and built by string interpolation
(`pgvector.go:266`). There is no "not equal", so decision records **cannot** be
excluded from the ordinary incident recall by filtering — they would silently
crowd out real incident history in the agent's four recalled sources.

Use a **separate collection** instead, via the existing
`crdbvector.WithCollectionName` option (`options.go:38`). Same physical tables,
different `collection_id`, so `cmd/nuke` and the DDL are unaffected, and no user
input ever reaches that interpolated SQL.

## Data model

### New table `decisions` (`remediation/decision.go`)

The vector index is derived data, not the record. `.env.example:60` warns that
changing the embedding width means dropping and recreating the vector tables —
and the accumulated judgement of the team is the last thing in this system that
should be lost to a model swap. So the table is the source of truth and the
embedding is an index over it.

```sql
CREATE TABLE IF NOT EXISTS decisions (
    decision_id         STRING PRIMARY KEY,
    investigation_id    STRING NOT NULL,
    incident_id         STRING,
    service_id          STRING,
    -- null when every candidate was rejected and the engineer fixed it themselves
    chosen_candidate_id STRING,
    -- what the engineer did instead. The most valuable record in the table:
    -- a demonstration rather than a rejection.
    engineer_fix        STRING,
    engineer_pr_url     STRING,
    notes               STRING,
    -- [{candidate_id, strategy, reason, source: "engineer"|"verification"}]
    rejections          JSONB,
    -- the prose that was embedded, kept so a change of embedding model is a
    -- re-index rather than a data loss
    document            STRING NOT NULL,
    decided_at          TIMESTAMPTZ NOT NULL,
    -- null until the vector write lands, so a failed embed can be retried
    indexed_at          TIMESTAMPTZ,
    INDEX (investigation_id),
    INDEX (decided_at DESC)
)
```

Created the same way as `remediations`/`solutions` (`remediation/store.go:43`) —
`CREATE TABLE IF NOT EXISTS` at boot, deliberately absent from
`scripts/seed-cluster.sql` so a demo reset cannot throw decisions away.

### `solutions` gains

- `CandidateRejected CandidateStatus = "rejected"` alongside the existing
  `CandidateSelected`, which this finally uses.
- A `rejection_reason STRING` column, added with the same
  `ADD COLUMN IF NOT EXISTS` pattern as `repairs` (`store.go:95`). Not folded
  into `error`, which means "this candidate failed technically" — a candidate
  can build, pass, and still be rejected on taste.
- **Never downgrade a status:** a candidate already at `pr_opened` stays there.
  Marking it `selected` would lose the link to the open PR.

## The endpoint

`POST /agent/:id/decision`, in `routes/remediation.go` beside the handlers that
already read this data.

```json
{
  "chosen_candidate_id": "f3e2f376-...",
  "rejections": [
    {"candidate_id": "abc-...", "reason": "rewrote more than the incident justified"}
  ],
  "engineer_fix": "widened the accumulator in Sum and left the branch structure alone",
  "engineer_pr_url": "https://github.com/...",
  "notes": "prefer arithmetic carry over branch-per-sign; easier to prove"
}
```

Every field optional; the frontend sends only what the engineer typed.

**Validation**, following the existing handlers' habit of naming the specific
problem:

- 404 if the investigation has no stored remediation (reuse `Solutions.Load`).
- 422 if `chosen_candidate_id` or any rejection id is not a candidate of *this*
  investigation — a mistyped id must not write a decision about someone else's
  incident.
- 400 if the body is empty of meaning: no chosen candidate, no `engineer_fix`,
  no `notes`. A record with nothing in it teaches nothing and would still be
  embedded and recalled.

**Auto-fill.** Any candidate that is neither chosen nor explicitly rejected gets
a rejection derived from `Verification.Summary()`, tagged `source: "verification"`.
So the common case — engineer picks one, types nothing — still produces a
complete record, and the interesting case (a candidate that built and passed but
was rejected anyway) is exactly the one where the engineer's prose survives.

**Response** 201, echoing the `document` that was embedded, so the UI can show
what the system actually learned. Cheap, and it makes the feature legible
instead of magic.

## The precedent document

`func (d Decision) Document() string` in `remediation/decision.go` — pure, so it
can be tested without a database or a model. Shaped to mirror `demo.sh`'s
incidents so it retrieves alongside them and reads naturally in the prompt:

```
Remediation decision for INC-2026-0810-checkout (2026-08-13), service checkout.

Incident: checkout returned 500s on every order over $10 within 6 minutes of
deploying commit 51e85e6.
Cause: 51e85e6 changed money.Sum to carry nanos without widening to int64, so
any sum past one unit overflowed.

Chosen fix (defensive): accumulate in int64 and derive the carry arithmetically.
Verified: builds, and the service's tests pass.

Rejected:
- minimal: does not build (missing int64 conversion). [verification]
- root-cause: rewrote more than the incident justified. [engineer]

Note: prefer the arithmetic carry over branch-per-sign; easier to prove correct.
```

And the reject-all case, which is the one worth getting right:

```
Chosen fix: none. All 3 proposed fixes were rejected.
The engineer fixed it instead by: widened the accumulator in Sum and left the
branch structure alone.
```

## Retrieval and injection

A second store at boot in `initialization.go`, beside `initStore`:

```go
crdbvector.New(ctx, crdbvector.WithConn(pool), crdbvector.WithEmbedder(embedder),
    crdbvector.WithCollectionName(decisionCollection))
```

Nil when it cannot be built, matching how MCP, the queue and the container
runtime already degrade — no precedent store means today's behaviour exactly.

### Investigation (`agent/runner.go`)

- New `Precedents vectorstores.VectorStore` field on `Runner`; nil disables.
- A sibling to `recall` that searches the decision collection with its **own
  small limit (2)**, so precedent cannot eat the four incident slots.
- `buildPrompt` gains a "Past remediation decisions by this team" section, kept
  distinct from the recalled incidents.
- One line in `systemPrompt` framing them as **this team's judgements, to be
  weighed and not obeyed**. The failure mode being guarded against is
  confidently repeating a past mistake because it is written down.

### Proposal (`remediation/patch.go`)

- `ProposalInput` (line 214) gains `Precedents []string`.
- `Runner.Run` fetches them before `fanOut`, querying on the incident summary
  plus triage evidence — prose describing a fault, which is what the documents
  lead with.
- `buildProposalPrompt` gains "## How this team has judged past fixes".

**Guard the strategy diversity.** The three strategies (minimal / defensive /
root-cause, `patch.go` `DefaultStrategies`) are deliberately different, and
fan-out is only worth its containers if they stay that way. Precedent must be
framed as *constraints to respect* ("this team rejects changes broader than the
incident justifies") rather than *a fix to imitate*, or all three strategies
converge and the ranking has nothing to choose between.

## Risks worth stating

- **Echo chamber.** Precedent reinforces itself: early decisions get outsized
  weight, and a bad one is recalled forever. Mitigated by the small retrieval
  budget and advisory framing; `decided_at` is stored so recency weighting is
  possible later without a migration.
- **Strategy collapse**, as above.
- **A decision is only as good as its reasons.** Auto-derived reasons are
  honest but shallow ("does not build"); the signal that matters is the engineer
  explaining a rejection of something that worked.

## Tests

- `Document()` shaping: chosen-candidate case, reject-all-with-engineer-fix
  case, and auto-derived reasons — pure string assertions, no DB.
- Handler validation: foreign candidate id → 422, empty body → 400, unknown
  investigation → 404. `routes/routes_test.go` already exists to extend.
- Auto-fill: a candidate the engineer never mentioned is recorded rejected with
  its `Verification.Summary()`.
- Status handling: a `pr_opened` candidate is not downgraded to `selected`.
- `buildPrompt` includes the precedent section, and omits it cleanly at zero —
  `agent/runner_test.go:73` already has a `stubStore` to extend.
- Precedent retrieval does not reduce the number of incidents recalled.
- Live round-trip (opt-in env gate, like `worker/journal_live_test.go:24`):
  write a decision, confirm the row and that the document is retrievable from
  the decision collection but **absent** from the default collection — the
  isolation this design rests on.

## Verification

```bash
gofmt -l . && go vet ./... && go test -race ./...
```

End to end, against the demo incident already in the database:

1. `GET /agent/{id}/remediation` for the checkout investigation — three
   candidates, one at `pr_opened`.
2. `POST /agent/{id}/decision` choosing it, rejecting one with prose and leaving
   the third unmentioned. Expect 201 and the embedded document echoed back.
3. Check the write landed:
   `SELECT chosen_candidate_id, indexed_at FROM decisions;` and
   `SELECT status, rejection_reason FROM solutions WHERE investigation_id = ...;`
   — the unmentioned candidate should carry its auto-derived reason, and the
   `pr_opened` one must still say `pr_opened`.
4. `POST /retrieve` with a query describing a similar overflow fault: the
   decision must **not** come back, proving collection isolation.
5. Re-run the investigation (`./scripts/enqueue.sh`) and read the boot log plus
   `result.Grounding` — the decision should appear in the precedent section, and
   the four recalled incidents should still be four.
6. Confirm the reject-all path: post a decision with no chosen candidate and an
   `engineer_fix`, and check the document reads as a demonstration.

## Out of scope

- Re-embedding existing decisions when the embedding model changes. The
  `document` column makes it a loop over rows; worth writing when it is needed.
- Weighting precedent by recency or by how often a strategy is accepted.
- Any use of the decision data at ranking time (`Rank`, `candidate.go:197`).
  Ranking on evidence is currently honest and explainable; mixing in learned
  preference deserves its own decision.
