# SRE agent — state and next steps

**The handoff doc.** There is deliberately only one. Written 2026-08-10,
rewritten 2026-08-14 after the decision-learning session, when the root `NEXT.md`
was merged into it and deleted — its App Runner research and cost method survive
under [Deploy and demo](#deploy-and-demo), which is the only part of it that was
still true.

Hackathon deadline is **2026-08-18**, so **4 days**.

The two files under `plans/` other than this one, and
`agent_space/plans/dazzling-giggling-squid.md`, are *records of reasoning* for
work that is now done. They are not to-do lists and nothing in them is pending.

---

## Start here: the next piece of work

**Candidate counts on `GET /remediations`.** Small, self-contained, and the last
thing the frontend is blocked on.

The list endpoint returns remediation rows with no candidate information at all —
deliberately, because a diff per candidate per row turns a list page into
megabytes. But that also means a list row cannot show *"3 fixes · 1 chosen"*
without an extra fetch per row, which is the one thing the picker's landing page
needs.

- `Solutions.Recent` — `agent_space/remediation/store.go:525`
- `Outcome` — `agent_space/remediation/candidate.go:171`

Add two aggregates: how many candidates the run produced, and whether a decision
has been recorded. Both are counts against tables that are already indexed on
`investigation_id`.

Two fields on `Outcome`, set only by `Recent` — the detail endpoint carries the
candidates and the decision themselves, so a count there would be a second way to
say the same thing and a second thing to keep in sync:

```go
// CandidateCount is how many fixes this run produced. Set by Recent only; the
// detail endpoint carries the candidates themselves.
CandidateCount int `json:"candidate_count,omitempty"`
// Decided reports whether an engineer has recorded a decision on this run, so a
// list row can distinguish "waiting for someone" from "dealt with".
Decided bool `json:"decided"`
```

On the SQL: the obvious `LEFT JOIN ... GROUP BY` aggregates every row in both
tables before `LIMIT` throws almost all of it away. Prefer limiting first and
counting against the survivors — a CTE that selects the page of `remediations`,
then joins. Worth reading the plan CockroachDB actually picks rather than
assuming; this runs against a Cloud Basic cluster where scans cost Request Units
(see the note under [Watch the Request Units](#watch-the-request-units)).

`Decided` needs a `decisions` count, not a status check on the candidates — a
chosen candidate can read back as `pr_opened` rather than `selected`, so
candidate status is not a reliable signal that a decision exists.

Then:

1. Extend the live test in `remediation/store_live_test.go` — a run with three
   candidates and a decision reads back `candidate_count: 3, decided: true`.
   The existing `TestLiveDecisionRoundTrip` already builds exactly that fixture.
2. Delete the "Candidate counts on `GET /remediations`" bullet from **Not there
   yet** in `agent_space/docs/frontend-api.md`, and document the two fields in
   the `GET /remediations` section.

After that, the remaining gaps are listed under
[Worth doing if there is time](#worth-doing-if-there-is-time). None blocks the
demo.

---

## Scope, as settled

An **SQS message is the trigger**. We do **not** detect anything — another
person's service does detection and produces `agent.assignment.v1`. Our half is:

```
SQS assignment → fetch incident context → investigate → verdict (commit)
              → triage (is the fault real?) → propose N fixes in containers
              → verify → engineer picks → draft PR
              → record the decision → recall it on the next similar incident
```

**The whole chain has run end to end, for real**, including the draft PR and the
repair round. What remains is the frontend (someone else's) and the demo.

---

## What works, end to end

| Piece | Where |
| --- | --- |
| SQS consumer, heartbeat, dedup, give-up | `worker/worker.go:89` (`handle`) |
| Incident context from CockroachDB | `incident/store.go:47` |
| Investigation loop | `agent/runner.go:104` |
| Structured verdict (decision, SHA, confidence) | `agent/verdict.go` |
| Durable verdicts + dedup across restarts | `worker/journal.go` |
| Reclaiming runs a crashed process left behind | `worker/journal.go`, `remediation/store.go` |
| Service → repo + verification profile | `remediation/repository.go` |
| Whole-file patcher: prompt, parse, apply | `remediation/patch.go` |
| Orchestrator: triage → inspect → fan out → verify → repair | `remediation/runner.go` |
| Draft PR via the Git data API | `remediation/pullrequest.go` |
| Recording an engineer's decision | `remediation/decision.go`, `routes/remediation.go` |
| Recalling past decisions as precedent | `remediation/precedent.go`, `agent/runner.go` |
| API for the frontend | `routes/remediation.go`, documented in `agent_space/docs/frontend-api.md` |
| Plumbing check, no model involved | `cmd/sandboxcheck` |

### Proof it is real, not wired

- **Draft PR #1 is open** on `Look-Its-Sky/opentelemetry-demo-auto-sre-test`,
  branch `sre-agent/checkout-f3e2f376`, opened 2026-08-13 by the pipeline. The
  whole "UI is the picker; picking opens the PR" claim is demonstrated, not
  asserted.
- **The bug is planted.** `demo-bug` is on the fork at `51e85e6`.
- **The agent found `51e85e6` unaided.** The incident context names only the
  symptom — 412 orders short by exactly 1.00 — and never mentions `money.Sum`.
- **The sandbox caught a fix that would pass human review.** On an early run all
  three candidates dropped an `int64(...)` conversion:
  `invalid operation: units += nanos / nanosMod (mismatched types int64 and int32)`.
  That diff would pass a review at 2am. This is the failure mode the whole design
  exists for, and it is not hypothetical.

---

## The repair round now works — and why the first two diagnoses were wrong

This is the most useful thing to carry forward, because I got it wrong twice
before getting it right, and the wrong answers were both plausible.

The symptom: a candidate fails to build, the repair round fires, and the model
comes back with **no files at all**.

```
minimal failed at build, repairing (round 1)
repair (minimal) came back with no files to write (""); keeping the unrepaired candidate
```

- **Wrong diagnosis 1: the model is too weak.** It is not. Buying a stronger
  model would have papered over it and cost money for the whole hackathon.
- **Wrong diagnosis 2: reasoning tokens ate the completion budget.** This was a
  *real* latent bug — `proposalBudget` was 16000, and reasoning tokens count
  against the completion budget, so a reasoning model could spend the entire
  allowance thinking and emit nothing. Fixed in `6014967` (now 32000). But it was
  not the cause; the repair still came back empty afterwards.
- **The actual cause:** `repairPrompt` said *"reply in the same format as
  before"*. Calls are stateless, and the repair call **replaces** the proposal
  prompt rather than extending it — so "before" referred to a message the model
  had never seen. It had no idea what format to answer in. Fixed in `35453f2` by
  extracting a shared `outputFormat` const used by both prompts.

Measured across three runs on identical input:

| Run | Model | Result |
| --- | --- | --- |
| 1 | nemotron-3-super-120b (free) | 2/3 build failures, repair empty, 1 usable |
| 2 | qwen3.7-flash + budget fix | 1/3 failed, repair empty, 2 usable |
| 3 | qwen3.7-flash + format fix | 2/3 failed, **both repaired**, 3/3 pass tests |

The repaired `minimal` diff was one line: `units += int64(nanos) / nanosMod`.

**Lesson worth keeping:** a prompt that refers to conversational context in a
stateless call fails silently and looks exactly like model weakness. Check what
the model can actually see before concluding it cannot think.

---

## Decision learning — what was built 2026-08-14

`POST /agent/:id/decision` records which candidate an engineer took and why the
others were not, embeds it, and recalls it on the next similar incident. Learning
in the retrieval sense, not the training sense.

Three constraints shaped it, and each is load-bearing:

- **The vector store's filters are equality-only** and built by string
  interpolation (`crdbvector/pgvector.go:266`). There is no "not equal", so
  decision records **cannot** be excluded from ordinary incident recall by
  filtering — they would silently crowd out real incident history. They live in a
  separate collection (`sre_decisions`) instead: same physical tables, different
  `collection_id`, no user input anywhere near that interpolated SQL. Proven
  isolated: `/retrieve` with a query written to match a stored decision returned
  only the seeded incidents.
- **The `decisions` table is the source of truth; the embedding is an index over
  it.** The `document` column stores the exact prose that was embedded, so a
  change of embedding model is a re-index rather than the loss of the team's
  accumulated judgement. Deliberately absent from `seed-cluster.sql` so a demo
  reset cannot throw decisions away.
- **Precedent is framed as advisory**, and capped at 2 recalled documents against
  the investigation's 4 incidents. The failure mode being guarded against is
  confidently repeating a past mistake because it is written down, and strategy
  collapse — if precedent reads as "imitate this fix", all three strategies
  converge and the ranking has nothing to choose between.

Verified: three POSTs to the same investigation leave one decision row and one
vector (supersede works); a decided remediation reads back with its decision over
HTTP.

**Still unproven:** the loop closing. Nobody has recorded a decision, re-run a
similar incident, and confirmed the precedent section changed the proposal. That
is the demo-worthy moment and it has not been filmed or measured.

---

## Watch the Request Units

The Cloud cluster is on the **Basic** plan and it hit its monthly Request Unit
limit mid-session on 2026-08-13, which disabled it outright:

```
ERROR: This cluster has reached its Request Unit limit for the month (SQLSTATE 53300)
```

Everything stopped — boot, tests, the API. Jude raised the limit manually; the
billing API was deliberately **not** touched from here without authorisation.

Practical consequence: **do not run live tests or full pipeline runs casually.**
`./scripts/test.sh -r` costs nothing (no DB). The `-run Live` suites and every
`enqueue.sh` run cost RUs. Batch them.

---

## The decision that shaped it: no coding harness in the container

The model **rewrites whole files** and the container writes them with `cat`. It
does not emit a unified diff, and no harness runs inside the sandbox.

- A diff has to be right about line numbers and context before it applies at all,
  and a small model gets *that* wrong far more often than it gets the fix wrong —
  turning good changes into apply failures.
- `git diff` inside the sandbox produces the real unified diff afterwards, so
  what a reviewer reads is always something git computed, never something a model
  claimed.
- `OPENROUTER_API_KEY` never enters a container.

```
container #1 (read-only)      host                    container #2
  clone + read files    →   model writes files   →   clone + write + build + test
  triage gate                                        git diff
```

Two clones before any candidate runs. Deliberate — the triage gate exists to stop
the expensive path, so the second clone is only paid for once the gate has passed.

### One known hole in this

`validateEdits` (`remediation/patch.go:511`) has **no guard against a truncated
file**. The model returns whole files; if one comes back with a lazy
`// ... rest unchanged`, that file is written as-is and the rest of the code is
silently deleted. The build would usually catch it, but "usually" is doing a lot
of work there, and a service with no test command would not catch it at all.

Not fixed. A length-ratio check against the original, or a scan for
`rest unchanged`-style markers, would close it. Roughly twenty lines.

---

## Next steps, in order

### 1. Candidate counts on `GET /remediations`

See [Start here](#start-here-the-next-piece-of-work).

### 2. Close the learning loop on camera

Record a decision on the checkout incident, re-run a similar investigation, and
confirm the precedent appears in `result.precedents` and in the proposal prompt.
This is the part of the submission nothing else demonstrates, and it is currently
built but unwitnessed.

```bash
cd agent_space
# 1. a decision on the run that already has candidates
curl -sX POST localhost:8080/agent/<id>/decision -H 'Content-Type: application/json' \
  -d '{"chosen_candidate_id":"<id>","rejections":[{"candidate_id":"<other>","reason":"rewrote more than the incident justified"}],"notes":"prefer arithmetic carry over branch-per-sign"}' | jq .document
# 2. re-run and read the precedent section back
./scripts/enqueue.sh -v 2
curl -s localhost:8080/agent/<new-id> | jq '{grounding: (.result.grounding|length), precedents: .result.precedents}'
```

The success condition: `precedents` is non-empty **and** `grounding` is still 4.
Precedent must not eat incident recall.

### 3. Real SQS rehearsal

Config-only but **unproven**. Development runs against LocalStack on `:4566`; the
demo is meant to run against real SQS.

The switch is `SQS_QUEUE_URL` plus AWS credentials, and there is now a guard
(`b62fbd6`) so a half-finished switch cannot silently fail: if `SQS_QUEUE_URL`
points at an `amazonaws.com` host, a leftover `AWS_ENDPOINT_URL` is ignored and
logged rather than quietly sending every poll to LocalStack.

What has never happened: a real queue created, real credentials set, one
assignment delivered end to end. Do it before the demo, not during.

### 4. Pick the deploy target

Still undecided, and it is a **design input** rather than a detail — see
[Deploy and demo](#deploy-and-demo). Decide before building anything for it.

### 5. Run `demo.sh` end to end, against a seeded cluster

Still the thing you will lean on while recording, and steps 3-4 of the script
have never been run in one pass. **Run `scripts/seed-cluster.sql` first** — see
the seeding trap below, which is the single most expensive thing on this page to
get wrong.

### 6. `GIN_MODE=release` and an OpenRouter spend cap

Neither is code. The server is in debug mode, logging every route and request.
The spend cap is the only control that still works when the code is wrong.

### Worth doing if there is time

- **A truncation guard in `validateEdits`.** See above. The highest-value of
  these — it is a silent data-loss path.
- **A capability endpoint.** The UI currently learns that PR opening is
  unconfigured by trying it and getting a `503`. One `GET /capabilities` would
  let it disable the button up front.
- **`verification.summary` on the wire.** The seven summary strings are computed
  server-side for the decision document but not sent with a candidate, so the
  frontend re-derives them from booleans — two implementations that can drift on
  the one thing that must not be got wrong.
- **`?dry_run=1` on the decision endpoint**, returning the document that *would*
  be embedded without writing anything.
- **Re-embedding decisions after an embedding-model change.** The `document`
  column makes it a loop over rows. Write it when it is needed.
- **Concurrency is still unverified.** Two simultaneous `/agent` calls have never
  been tested.

---

## Deploy and demo

Merged from the old `NEXT.md` (2026-08-07) and re-checked on 2026-08-14. Numbers
marked [measured] were read off real output; [unverified] means believed but not
tested — treat those with suspicion.

### The seeding trap — do not skip this

The most expensive thing on this page. Before `scripts/seed-cluster.sql` existed,
the agent's live-cluster half was **doing nothing**. Against a cluster holding
only the langchain vector tables, it invented a `prod_db` database with
`checkouts` and `cart_items`, and six of its eight tool calls errored:

```
list_tables      {"database":"prod_db"}   → target database or schema does not exist
get_table_schema {"table":"checkouts"}    → relation ... does not exist
```

It still answered **ROLLBACK, the right commit, citing the right past incident**.
That is the trap: the answer came entirely from the vector store, the live-cluster
integration contributed zero, and nothing in the output said so. A judge reading
that trace sees a system that looks grounded and isn't.

After seeding, five calls, zero failures, and the commit is *derived from the
cluster* and corroborated against past incidents — which is the actual pitch.

**Run the seed script against whatever cluster the demo points at, including the
cloud one, before recording anything.**

### App Runner would kill the synchronous `/agent` [verified]

App Runner's per-request timeout is **120 seconds and is not configurable**
(roadmap issue below still open; some users report being cut off at 30s).

**Much less severe than when this was first written**, because the SQS worker now
carries the long work: an assignment is consumed, investigated and remediated
entirely outside any HTTP request. The pipeline is immune to the cap.

What is *not* immune is `POST /agent`, the synchronous demo route, which calls
`SREAgent.Run(c.Request.Context(), ...)`. Two consequences, both live today:

- The first complete run took **~4 minutes** to investigate. That breaches 120s
  outright, so on App Runner the manual route would 504 while the queue path
  beside it worked fine.
- **A client disconnect cancels the run server-side** and returns 500, because it
  is the request's context. Cost me three confusing failures with `curl -m 300`
  before I realised it was me, not the server. A judge closing a tab does the same.

So: if the demo drives everything through `enqueue.sh` and reads results back
with `GET /agent/:id`, App Runner is fine. If it drives `POST /agent` live on
stage, pick something without a hard request cap (ECS behind an ALB with a long
idle timeout, or a plain EC2 box) — or make `POST /agent` enqueue-and-poll like
the queue path already does, which is ~80 lines and removes the disconnect
problem too.

### AWS access is unproven

- No `aws` CLI on this machine.
- IAM user `jude` (account 071954287023) has almost no attached policies — S3 and
  Lambda both returned plain `AccessDenied`.
- `scripts/deploy.sh` is 160 lines and **has never been executed once**.
- **Bedrock is dead on this account.** Known dead end, do not re-diagnose.

Three unknowns stacked, and first-run debugging of a deploy script is exactly
where a day disappears with four left.

### Cost per `/agent` run [estimated]

Method: ~4 chars/token, anchored on one measurement — a bare tool-calling request
with 15 tool schemas came back at **3182 prompt tokens** [measured], so the schema
block dominates the fixed cost.

| Component | Tokens |
|---|---|
| System prompt + 10 tool schemas | ~2400 |
| Grounding: 4 recalled incidents | ~200 |
| Tool results accumulated by iteration 3 | ~1400 |
| **Prompt tokens, summed over 3 iterations + summarise** | **~14,700** |
| Completion tokens | ~1000 |

No dollar figures on purpose — multiply by whatever the model actually costs.
For scale, ~30 full remediation runs was priced between $0.15 (qwen3.7-flash) and
$10.20 (Sonnet-class) when this was last checked, which is why cost is not the
binding constraint on model choice; the format bug was.

**Prompt caching** is the unexplored win: the static prefix is re-sent on every
iteration plus the final summarise, so ~7000 redundant prompt tokens per run
[estimated] are recoverable if OpenRouter exposes caching for the chosen model.

### Latency, and the good story in it [measured]

`/agent` went 370.4s → 66.0s on a local model when one bad tool call was removed
from the prompt (`SHOW JOBS` migration history, 65% of all tool output, re-sent
every iteration). The breakdown is the number worth quoting:

```
MCP tool calls      0.06s    0.04%   5 calls, 0 failed
model + overhead   65.90s   99.96%   5 iterations
```

**CockroachDB is not the bottleneck by three orders of magnitude** — a good line
for the submission, and reproducible with `./scripts/bench.sh`.

Caveat: run-to-run variance is high (370s, 135s, 66s across three runs, the last
two on identical code). Do not quote 66s as reliable without
`./scripts/bench.sh -e agent -n 5`.

### Tuning knobs, none urgent

- **`maxToolOutputChars` is 4000** (`agent/runner.go:33`). Dropping to ~1500 cuts
  the worst case but truncates legitimately large `SELECT` results. Fix tool
  *choice* before tool *output*.
- **`defaultMaxIterations` is 6** (`agent/runner.go:26`). Real runs used 3, so 4
  would cap the tail without changing any observed run.
- **`maxResultChars` is 6000** (`utils/mcp/tool.go:16`) and never binds, because
  the runner's 4000 truncates first. Not a bug, but someone will eventually tune
  the wrong one.
- **`/store` and `/ask` use `context.Background()`**, so they ignore cancellation
  entirely — the opposite failure to `/agent`, which is too sensitive to it. Both
  should use the request context with an explicit timeout.

---

## Decisions only you can make

- **Deploy target.** Per above, this is a design input, not a detail.
- **Synthetic incident rows in the live cloud cluster.** INC-412 and INC-388 are
  still sitting in the production cluster from early testing. Keep them as demo
  seed data, or clear them out? Harmless, but they are fake data in a real
  database.
- **The LICENSE name.** Deferred previously; still says what it says.
- **Whether to rebase out the `Co-Authored-By` trailers** on the six commits that
  carry them.

---

## Small stuff

- `agent_space/MCP_HANDOFF.md` is committed and partly stale — it contradicts the
  code in places. Update it or drop it.
- `scripts/test_api.sh` predates everything and still tests "Agent Antigravity".
  Harmless, but it is the first script a judge might open.

---

## Decisions already made — do not re-litigate

- **Whole-file rewrites, not diffs; no harness in the container.**
- **Container CLI, not the Docker SDK.** podman and nerdctl work by changing one
  string; every invocation is pasteable.
- **The repo is only ever cloned inside the throwaway container.** Never on the
  host.
- **`--depth 100`, plus a targeted fetch for full SHAs and a conditional
  `--deepen` for short ones.** Not `--filter=blob:none`: a blobless clone needs
  the network for the container's whole life. Depth is a context budget.
- **Draft PRs via `go-github` + `GITHUB_TOKEN`**, through the Git data API rather
  than by pushing from the sandbox. The token never enters a container that just
  ran model-written code.
- **The UI is the picker; picking opens the PR.** Jude builds the frontend.
- **Verification is per service, and reports what actually ran.** "Builds" must
  never be presented as "tests passed". `Verification.Summary()` is the only
  thing that phrases this, it is tested, and `docs/frontend-api.md` tells the
  frontend not to reword it.
- **ROLLBACK is never remediated.** Rolling back is not a code change.
- **The chaos/bug-planting agent stays separate** from the investigator, which is
  read-only by construction (`utils/mcp/readonly.go`, fail-closed).
- **`API_TOKEN` stays unset for the hackathon.** Decided 2026-08-12. Set it
  before anything is exposed publicly.
- **Decisions are stored durably first, embedded second.** The table is the
  record; the vector is an index over it.
- **Re-deciding replaces rather than merges.** Two contradictory precedents on
  file are worse than either alone.
- **Bedrock is a dead end on this AWS account.** Do not re-diagnose it.
- **No `Co-Authored-By` / AI-attribution trailers on commits.**

---

## Facts that were expensive to discover

- **`src/checkout` is Go and has `money/money_test.go`.** The only mapped service
  where a fix can be *verified* rather than merely compiled. Prefer it for demos;
  `enqueue.sh` defaults to it, `-p` switches to payment.
- **`src/payment` has zero tests.** `package.json` declares only `start`. Its
  verification is `node --check` — a build signal, nothing more.
- **Every originally-mapped image was wrong**, and none of it was setup trouble:
  `golang:1.24` cannot build checkout (`go.mod` needs ≥1.25 and the official
  images pin `GOTOOLCHAIN=local`); `node:22-alpine` ships no git and the sandbox
  clones *inside* the container; `dotnet/sdk:9.0` cannot build cart, which targets
  `net10.0`. Corrected in `scripts/repos.sql` and applied to the cluster.
- **A stage's exit code is its last command's.** The clone stage once reported
  success with no checkout at all, because the fetch fallbacks end in `|| echo`.
  It now ends in `[ -d /workspace/.git ]`.
- **`git fetch origin <short-sha>` never works.** GitHub will not resolve an
  abbreviated object name in a fetch. The deploys table stores seven characters,
  so this was the common path.
- **`git diff` does not show a file the model created.** Now `git add -A && git
  diff --cached`, run *before* setup and build so it cannot sweep in
  `node_modules`.
- **Commit distance to `main`:** `0c6f0ae` (checkout culprit) is 67 behind,
  `207d6ef` (payment culprit) is 40. Depth 50 would miss checkout.
- **A read-only GitHub token looks completely healthy until the first write.**
  The repo's `permissions` block reports `push: true` — that is the *account's*
  access, not the token's grant. `./scripts/tokencheck.sh` settles it in seconds
  and exits non-zero, so it can gate a demo run. **Currently passing** (classic
  token, no expiry).
- **Nothing is on `PATH` on this NixOS box** — not `go`, not `docker`. Use
  `nix develop` in `agent_space/`. Flakes only see git-tracked files, so
  `flake.nix` and `flake.lock` must be `git add`ed before `nix develop` works at
  all. There is no container runtime available here to test against.
- **`~/.gitconfig` points the credential helper at `/usr/bin/gh`, which does not
  exist** — `gh` is at `~/.local/bin/gh`. Pushes fail with
  `No such file or directory`. Workaround, not fixed (it is global config):
  ```bash
  git -c credential."https://github.com".helper='!gh auth git-credential' push
  ```

---

## Environment

- Repo under test: `Look-Its-Sky/opentelemetry-demo-auto-sre-test`, public, fork
  of `open-telemetry/opentelemetry-demo`, ~57 MB.
  Branches: `main`, `demo-bug` (`51e85e6`, the planted regression),
  `sre-agent/checkout-f3e2f376` (the pipeline's own PR branch).
- Mapped services, as corrected and applied to the cluster:
  `checkout` → `src/checkout`, `golang:1.25`, `go build ./...` + `go test ./...`
  `payment` → `src/payment`, `node:22`, `node --check` only
  `cart` → `src/cart`, `mcr.microsoft.com/dotnet/sdk:10.0`, `dotnet build` only
- Database is **CockroachDB Cloud Basic** (`DATABASE_URL`), cluster
  `8fdfa73f-06b5-4fbb-a47a-7888a1bda51f`. `psql` needs `&sslrootcert=system`
  appended. Watch the Request Units.
- Models: `OPENROUTER_MODEL=qwen/qwen3.7-flash`,
  `OPENROUTER_EMBEDDING_MODEL=nvidia/nemotron-3-embed-1b:free` at
  `OPENROUTER_EMBEDDING_DIMENSIONS=2048`. Changing the embedding model or width
  means dropping and recreating the vector tables (`go run ./cmd/nuke
  -mode=drop`), because the column is sized at creation.
- Long-lived tables (`investigations`, `service_repositories`, `remediations`,
  `solutions`, `decisions`) are created at boot and are deliberately absent from
  `seed-cluster.sql`, which drops what it recreates. Reseeding demo data does not
  destroy them.
- LocalStack SQS on `:4566`, queue `static-log-analysis`, DLQ after 5 deliveries.
- Seeded incident ids: checkout `3f8a1c94…`, payment `6424cd11…`.
- `.env` is gitignored (`*.env`) and has never been committed. A secret scan
  (`ghp_`, `sk-or-v1-`, `CCDB1_`, `AKIA`) runs before commits.

## Repo state

Branch `agent_space`, **pushed and in sync** with
`origin/agent_space`. Working tree clean at `b03fcc9`.

```
b03fcc9  Write down the API the frontend is being built against
7fd0778  Return the recorded decision with the remediation that produced it
35453f2  Tell the repair prompt what format to answer in
6014967  Stop reasoning tokens silently eating the whole completion budget
94ae047  Learn from which fix an engineer picked, and which they turned down
70c8145  Stop discarding investigations whose process died mid-run
cd0a429  Write down the next two pieces of work and the decisions behind them
b62fbd6  Ignore a stale endpoint override when the queue URL is AWS's own
833ea68  Turn a HOTFIX verdict into verified candidate fixes and a draft PR
```

Six commits before `7fd0778` carry a `Co-Authored-By` trailer that should not be
there. Left alone rather than rebased — say the word if you want the history
cleaned.

`agent_space/plans/dazzling-giggling-squid.md` holds the two plans from
2026-08-13. **Both are now implemented**; the file is kept for the reasoning, not
as a to-do.

Handy commands:

```bash
cd agent_space && nix develop
./scripts/test.sh -r                      # fmt, build, vet, race — costs nothing
./scripts/tokencheck.sh                   # can the GitHub token write?
./scripts/enqueue.sh                      # send a checkout assignment (costs RUs)
go run ./cmd/sandboxcheck                 # prove the container plumbing, no model
go run ./cmd/seed -file scripts/repos.sql # reapply the repo mapping (UPSERTs)

# live suites — these cost Request Units, batch them
JOURNAL_LIVE_TEST=1     go test ./worker/      -run Live -v
REMEDIATION_LIVE_TEST=1 go test ./remediation/ -run Live -v
```

Killing a dev server: `pkill -f "go run ."` kills the parent, not the compiled
child, and has killed this shell more than once. Take the PID from
`ss -ltnp | grep 8080` instead. A stale server holding `:8080` means the new one
exits with "address already in use" and your test silently hits the *old* build.

---

## Sources for the things that were looked up rather than recalled

- [Configurable request timeout · aws/apprunner-roadmap#104](https://github.com/aws/apprunner-roadmap/issues/104)
- [AWS App Runner: what exactly does the timeout limit mean? (re:Post)](https://repost.aws/questions/QUnozEpub5Tnq-ilmrB4l2Sg/aws-app-runner-what-exactly-does-the-timeout-limit-documentation-mean)
- [Text Embedding Models — OpenRouter](https://openrouter.ai/collections/embedding-models)
- [Embeddings — OpenRouter docs](https://openrouter.ai/docs/client-sdks/typescript/api-reference/embeddings)
