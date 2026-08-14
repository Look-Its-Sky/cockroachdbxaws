# SRE agent — state and next steps

Scratch handoff doc, written 2026-08-10. Updated 2026-08-12, when the
remediation half was built. Hackathon deadline is **2026-08-18**.

---

## Scope, as settled

An **SQS message is the trigger**. We do **not** detect anything — another
person's service does detection and produces `agent.assignment.v1`. Our half is:

```
SQS assignment → fetch incident context → investigate → verdict (commit)
              → triage (is the fault real?) → propose N fixes in containers
              → verify → engineer picks → draft PR
```

**The whole chain has now run end to end, for real** (2026-08-13). See
"The first complete run" below. What remains is polish and the frontend.

The frontend is Jude's; the backend exposes the API for it.

---

## What works, end to end

Verified against the live Cloud cluster and LocalStack on 2026-08-10: an
assignment was consumed, the context resolved, the agent investigated in 89s
over 3 iterations and 2 `select_query` calls, and the verdict persisted and was
read back over HTTP. Queue drained, DLQ empty.

| Piece | Where |
| --- | --- |
| SQS consumer, heartbeat, dedup, give-up | `agent_space/worker/worker.go:89` (`handle`) |
| Incident context from CockroachDB | `agent_space/incident/store.go:47` |
| Investigation loop | `agent_space/agent/runner.go:104` |
| Structured verdict (decision, SHA, confidence) | `agent_space/agent/verdict.go` |
| Durable verdicts + dedup across restarts | `agent_space/worker/journal.go` |
| Read a verdict back | `GET /agent/:id` → `routes/private.go` |
| Service → repo + verification profile | `agent_space/remediation/repository.go` |

## What is built, wired, and container-verified as plumbing

Written 2026-08-12, reachable from the binary. The orchestration is tested
against fakes; the container stages underneath it have been run for real:

| Piece | Where |
| --- | --- |
| Whole-file patcher: prompt, parse, apply | `remediation/patch.go` |
| Candidate + Verification types, ranking | `remediation/candidate.go` |
| Orchestrator: triage → inspect → fan out → verify | `remediation/runner.go` |
| `remediations` + `solutions` tables | `remediation/store.go` |
| Draft PR via the Git data API | `remediation/pullrequest.go` |
| Worker handoff, after the ack | `worker/worker.go:166` (`remediate`) |
| API for the frontend | `routes/remediation.go` |
| CORS | `utils/cors.go` |
| Plumbing check, no model involved | `cmd/sandboxcheck` |

---

## The decision that shaped it: no coding harness in the container

Settled 2026-08-12, replacing the earlier "an image containing OpenCode" plan.

The model **rewrites whole files** and the container writes them with `cat`.
It does not emit a unified diff, and no harness runs inside the sandbox.

- A diff has to be right about line numbers and context before it applies at
  all, and a small free model gets *that* wrong far more often than it gets the
  fix wrong — turning good changes into apply failures.
- `git diff` inside the sandbox produces the real unified diff afterwards, so
  what a reviewer reads is always something git computed, never something a
  model claimed.
- No custom image to build and publish; stock images are used exactly as
  mapped. But the image has to be able to *clone* and to build the service,
  which is not free: all three mapped images failed one of those (step 0).
- `OPENROUTER_API_KEY` never enters a container.

The read path is a separate read-only container run, so the repository is still
only ever cloned inside a throwaway container and never onto the host.

```
container #1 (read-only)      host                    container #2
  clone + read files    →   model writes files   →   clone + write + build + test
  triage gate                                        git diff
```

Cost: two clones before any candidate runs. Deliberate — the triage gate exists
to stop the expensive path, so the second clone is only paid for once the gate
has passed.

---

## The first complete run — 2026-08-13

One SQS assignment, no human in the loop, against the live cluster and the real
fork with the planted bug on `demo-bug`:

```
enqueue -v 2  →  investigate: 3 iterations, 4 sources, 4m
              →  verdict: HOTFIX 51e85e6, confidence 0.9, declared
              →  ack, then hand off
              →  triage: CONFIRMED, src/checkout/money/money.go, 0.95
              →  3 candidates in containers, ~90s each
              →  2 of 3 build AND pass checkout's own test suite
```

The agent found `51e85e6` unaided. The incident context names only the symptom —
412 orders short by exactly 1.00, cents that carry past a whole unit — and never
mentions `money.Sum`.

The top-ranked fix (strategy: defensive) rewrote `Sum` to do the arithmetic in
int64 with an explicit carry, collapsing the same-sign and different-sign
branches into one path. All 20 `TestSum` cases pass, mixed-sign borrow included,
so it is not passing by accident. It is arguably better than the code it
replaced.

### What the verification caught, before the repair round existed

On the first run **all three candidates failed to build**:

```
money/money.go:92:3: invalid operation: units += nanos / nanosMod
                     (mismatched types int64 and int32)
```

The model had found the exact line and the exact logic and dropped the
`int64(...)` conversion. That diff would pass a human review at 2am. Without the
sandbox it would have been drafted into a pull request as a confident answer.
This is the failure mode the whole design exists for, and it is not hypothetical.

### The repair round

`Runner.candidate` now loops: when a candidate applies but the build or the
tests reject it, the failing stage's output goes back to the model with its own
file contents, and the container runs again. Bounded at one round
(`REMEDIATION_MAX_REPAIRS`), because the failures worth repairing are mechanical
and a model that cannot fix its own compiler error when shown it once will not
fix it on the fifth go — while each round costs a container per candidate.

Honest about its limits: with the current free model the repair fires, logs, and
comes back **empty** —

```
minimal failed at build, repairing (round 1)
repair (minimal) came back with no files to write (""); keeping the unrepaired candidate
```

so the unrepaired candidate is kept and reported as "does not build". The
mechanism is right; `nvidia/nemotron-3-super-120b-a12b:free` returns nothing for
the repair prompt, most likely spending its budget on reasoning tokens the way
`agent/runner.go` already notes it can. **A stronger model is the next lever**,
and the 2-of-3 pass rate came from resampling rather than from repair.

## Next steps, in order

### 0. What `cmd/sandboxcheck` found (2026-08-12) — all fixed and re-verified

The container half now runs end to end for **all three mapped services**,
against the live cluster and the real fork:

| Service | Image | Stages | Time |
| --- | --- | --- | --- |
| checkout | `golang:1.25` | clone, harness, diff, build, **test** | 1m30s |
| payment | `node:22` | clone, harness, diff, setup, build | 12s |
| cart | `mcr.microsoft.com/dotnet/sdk:10.0` | clone, harness, diff, build | 31s |

Getting there took six fixes. **Every mapped image was wrong**, and not one of
these was setup trouble — each would have silently ruined real runs:

1. **`golang:1.24` cannot build checkout.** `src/checkout/go.mod` requires
   `go >= 1.25.0`, and the official Go images pin `GOTOOLCHAIN=local`, so the
   image cannot fetch a newer toolchain to compensate. Every checkout candidate
   would have failed to build — the demo service, and the only one with tests.
2. **`node:22-alpine` ships no git**, and the sandbox clones *inside* the
   container. Payment produced nothing but `git: not found`. Now `node:22`.
3. **`dotnet/sdk:9.0` cannot build cart**, which targets `net10.0`: NETSDK1045,
   no fallback. Now `sdk:10.0`. Same class of mistake as checkout.
4. **The clone stage reported success with no checkout at all.** A stage's exit
   code is its last command's, and the fetch fallbacks end in `|| echo` so an
   unreachable commit stays a finding rather than a dead run — which meant the
   alpine failure above came back as **clone exit 0**. `Runner.candidate` reads
   exactly that to decide the checkout is good. The clone stage now ends in
   `[ -d /workspace/.git ]`, via a shared `cloneStageCommand` all three script
   builders go through.
5. **`git diff` does not show a file the model created.** The diff stage came
   back empty for a new file, and `ApplyCommand` explicitly supports creating
   one. Now `git add -A && git diff --cached`, and moved to run *before* setup
   and build so it cannot sweep in `node_modules` or build output.
6. **`git fetch origin <short-sha>` never worked.** GitHub will not resolve an
   abbreviated object name in a fetch — `fatal: couldn't find remote ref
   0c6f0ae`, tested against full and short side by side. The deploys table
   stores seven characters, so this was the common path; the seeded culprits
   only ever resolved because they sit inside `--depth 100`. Full SHAs still
   fetch directly; short ones fall back to a conditional `--deepen 500`,
   verified to resolve a commit a depth-5 clone could not see.

`scripts/repos.sql` carries all three image corrections and **has been applied
to the cluster** (`go run ./cmd/seed -file scripts/repos.sql -yes`; UPSERT only,
nothing dropped).

### 1. `cmd/sandboxcheck` as a regression check

`ContainerSandbox.Run` is exercised for real now. Re-run after touching
`script.go`, `patch.go` or the repository mapping — it validates the clone, the
SHA fetch, `args()`, `ApplyCommand`, the stage markers and `ParseSections` in
one shot, with no model and no cost beyond container time.

```bash
cd agent_space && nix develop
go run ./cmd/sandboxcheck                     # checkout, ~1m30s
go run ./cmd/sandboxcheck -service payment -sha 207d6ef
go run ./cmd/sandboxcheck -service cart
go run ./cmd/sandboxcheck -mode triage        # the read-only gate, ~30s
go run ./cmd/sandboxcheck -mode inspect -files src/checkout/money/money.go
go run ./cmd/sandboxcheck -image golang:1.26  # prove an image before mapping it
```

It exits non-zero and prints the failing stage's output if a stage that should
have run did not. `-v` prints every stage, which is how to read the diff. The
expected-stage list follows the mapping, so a service that declares a setup or
test command must pass it — added after payment's `npm ci` was found to be
running unchecked.

### 2. `GITHUB_TOKEN` cannot write ← **the blocker, and bigger than it looks**

The fine-grained PAT in `.env` (`github_pat_…`, owned by Look-Its-Sky, expires
2026-09-09) is **read-only**. Diagnosed rather than assumed:

```
./scripts/tokencheck.sh    ->  cannot write: the token is valid but read-only
                               GitHub says it needs: contents=write
```

- Repository *selection* is not the problem: the token lists **89 repositories
  across 9 owners**, so it was created against all repositories.
- It cannot write to **any** of them — a blob POST to a second repo it owns
  (`Look-Its-Sky/algodash`) also returns 403.
- `x-accepted-github-permissions: contents=write` names the missing grant
  exactly.

The trap: the repo's `permissions` block reports `push: true`, and `GET` calls
all succeed. That is the *account's* access, not the token's grant, so a
read-only token looks completely healthy until the first write.

This blocks two things, and the second matters more:

1. Pushing the planted bug (step 3).
2. **The draft PR path.** `POST /solutions/:candidate/pr` will fail on the
   first blob it creates. "The UI is the picker; picking opens the PR" is wired
   end to end and cannot work until the token is fixed. Nothing else depends on
   it — fixes are still proposed, verified and ranked.

Fix at <https://github.com/settings/personal-access-tokens> — edit the existing
token's **Repository permissions**:

```
Contents:      Read and write      <- the missing one
Pull requests: Read and write      <- needed for the PR itself, untested so far
```

Then `./scripts/tokencheck.sh` should print "can write". It exits non-zero
otherwise, so it can gate a demo run.

`Publisher.Open` now translates these itself: a 403 becomes `ErrTokenCannotWrite`
naming the permission GitHub asked for and the settings URL, and a 404 says the
repository is not in the token's selected list. Previously the frontend would
have shown the raw "Resource not accessible by personal access token" with no
indication of what to do.

### 3. Plant the bug — prepared, one push away

`main` contains no bug: both seeded culprits are *fixes* present in HEAD, so
triage correctly returns `not_present` and stops. That is the pipeline working,
but it is not a demo.

The regression is written and **verified to fail the real test suite**, on a
`demo-bug` branch so `main` stays clean for the detector upstream. One line
removed from `money.Sum` — the units carry — so every order whose line-item
nanos sum past a whole unit is undercharged by exactly 1.00:

```
--- FAIL: TestSum/both_positive_(carry)
    Sum([units:2 nanos:200000000],[units:2 nanos:900000000])
      = units:4 nanos:100000000, want units:5 nanos:100000000
--- FAIL: TestSum/both_negative_(carry)
```

Two existing cases catch it, which is the point: checkout is the only mapped
service where a candidate can be *verified* rather than merely compiled.

- `scripts/demo-bug.patch` — the commit, `51e85e6`, so it survives `/tmp`
- `scripts/demo-bug.sql` — the deploy row, a version-2 incident context
  describing only the symptom, and the flip of checkout's `default_branch`

Once the token can write:

```bash
git clone https://github.com/Look-Its-Sky/opentelemetry-demo-auto-sre-test /tmp/f
cd /tmp/f && git checkout -b demo-bug
git am < ~/Nextcloud/Repos/cockroachdbxaws/agent_space/scripts/demo-bug.patch
git push origin demo-bug                    # must produce 51e85e6

cd agent_space
go run ./cmd/seed -file scripts/demo-bug.sql
go run ./cmd/sandboxcheck                   # test SHOULD now fail: the bug is real
./scripts/enqueue.sh -v 2                   # the full chain
```

`sandboxcheck` failing at `test` on that branch is the success condition, not a
regression — it is what proves the fault is present for triage to confirm.

### 4. Frontend

The API is up. `API_TOKEN` is deliberately **left unset** (decided 2026-08-12) —
everything is unauthenticated and CORS allows any origin. Set both before this
is exposed anywhere.

| Route | What it gives the UI |
| --- | --- |
| `GET /repositories` | the mapping, and whether each service has tests |
| `GET /remediations?limit=` | recent runs, newest first, no diffs |
| `GET /agent/:id` | the verdict |
| `GET /agent/:id/remediation` | triage + ranked candidates, with diffs |
| `POST /agent/:id/remediation` | run remediation by hand; 202, then poll |
| `POST /solutions/:candidate/pr` | pick a candidate, open the draft |

`GET /agent/:id/remediation` 404s until a remediation exists — that is also what
"still queued" looks like, which the UI should say rather than treating as an
error.

### 5. Worth doing if there is time

- **Nothing reconciles a crashed run.** A remediation interrupted mid-flight
  stays `running` in the table forever. A sweep at boot marking stale rows
  failed would take twenty lines.
- **`Recent` does not join candidate counts**, so a list page cannot show "3
  fixes" without N queries.
- **No live test against the cluster** for `remediations`/`solutions`, the way
  `JOURNAL_LIVE_TEST=1` covers the journal.

---

## Decisions already made — do not re-litigate

- **Whole-file rewrites, not diffs; no harness in the container.** See above.
- **Container CLI, not the Docker SDK.** Huge dependency avoided; podman and
  nerdctl work by changing one string; every invocation is pasteable.
- **The repo is only ever cloned inside the throwaway container.** Never on the
  host.
- **`--depth 100`, plus a targeted fetch for full SHAs and a conditional
  `--deepen` for short ones.** Not `--filter=blob:none`: a blobless clone needs
  the network for the container's whole life. Depth is a context budget.
  See the correction below about what the fetch can and cannot do.
- **Draft PRs via `go-github` + a fine-grained PAT** (`GITHUB_TOKEN`, already in
  `.env`), through the Git data API rather than by pushing from the sandbox. The
  token never enters a container that just ran model-written code.
- **The UI is the picker; picking opens the PR.** Jude builds the frontend.
- **Verification is per service, and reports what actually ran.** "Builds" must
  never be presented as "tests passed" — `Verification.Summary()` is the only
  thing that phrases this, and it is tested.
- **ROLLBACK is never remediated.** Rolling back is not a code change.
- **The chaos/bug-planting agent must stay separate** from the investigator,
  which is read-only by construction (`utils/mcp/readonly.go`, fail-closed).
- **`API_TOKEN` stays unset for the hackathon.** Decided 2026-08-12.

## Facts that were expensive to discover

- **`src/payment` has zero tests.** `package.json` declares only `start`. Its
  verification is `node --check` — a build signal, nothing more.
- **`src/checkout` is Go and has `money/money_test.go`.** It is the only mapped
  service where a fix can be verified rather than merely compiled. Prefer it
  for demos; `enqueue.sh` defaults to it, `-p` switches to payment.
- **Commit distance to `main`:** `0c6f0ae` (checkout culprit) is **67** behind,
  `207d6ef` (payment culprit) is **40**. Depth 50 would miss checkout. The fork
  takes dependabot bumps most days, so depth 100 ≈ a month of runway — hence
  the explicit fetch by SHA.
- **`main` contains no bug.** See step 2.
- **`cmd/seed`'s splitter was broken** for empty string literals (`''` read as
  an escaped quote, never leaving string state). Fixed, with tests.
- **Nothing is on `PATH` on this NixOS box** — not `go`, not `docker`. Use
  `nix develop` in `agent_space/` rather than debugging "command not found".
  Note that flakes only see git-tracked files, so `flake.nix` and `flake.lock`
  have to be `git add`ed before `nix develop` will find them at all.

## Environment

- Repo under test: `Look-Its-Sky/opentelemetry-demo-auto-sre-test`, public,
  fork of `open-telemetry/opentelemetry-demo`, branch `main`, ~57 MB.
- Mapped services (`service_repositories`, applied to the cluster):
  `checkout` → `src/checkout`, `golang:1.24`, `go build ./...` + `go test ./...`
  `payment` → `src/payment`, `node:22-alpine`, `node --check` only
  `cart` → `src/cart`, `dotnet/sdk:9.0`, `dotnet build` only
- Database is **CockroachDB Cloud** (`DATABASE_URL` in `.env`), not the local
  container. `psql` needs `&sslrootcert=system` appended.
- Long-lived tables (`investigations`, `service_repositories`, `remediations`,
  `solutions`) are created at boot and are deliberately absent from
  `seed-cluster.sql`, which drops what it recreates. Reseeding demo data does
  not destroy them.
- LocalStack SQS on `:4566`, queue `static-log-analysis`, DLQ after 5
  deliveries. `scripts/enqueue.sh` sends a real assignment.
- Seeded incident ids: checkout `3f8a1c94…`, payment `6424cd11…`.

## Repo state

Three commits on `agent_space`, **not pushed**:

```
02e4d8a  Drive investigations from SQS, and lay the groundwork for proposing fixes
043b651  Trim doc comments to one line where the code already says the rest
(uncommitted) the remediation half
```

`git push -u origin agent_space` is still outstanding.

Handy commands:

```bash
cd agent_space
./scripts/test.sh -r                      # fmt, build, vet, race
./scripts/enqueue.sh                      # send a checkout assignment
go run ./cmd/sandboxcheck                 # prove the container plumbing
go run ./cmd/seed                         # reseed demo data (DROPs)
go run ./cmd/seed -file scripts/repos.sql # reapply the repo mapping (UPSERTs)
JOURNAL_LIVE_TEST=1 go test ./worker/ -run Live -v   # journal against the cluster
```
