# The API the frontend is built on

Written 2026-08-14 for whoever is building the picker UI, so the shapes come from
here rather than from reading Go or guessing from a `curl` someone pasted in
chat.

Everything below is in the server today and was exercised against a live cluster.
Where something is known to be missing, it says so under
[Not there yet](#not-there-yet) rather than being left for you to discover.

The short version: an incident arrives on SQS, the agent investigates it and
commits to ROLLBACK or HOTFIX, a HOTFIX verdict fans out into three candidate
fixes that are each built and tested in a container, and an engineer picks one.
The UI's job is the last step — showing what was proven about each candidate, and
recording what the engineer concluded.

---

## Basics

**Base URL** — whatever the server is on; `localhost:8080` in development.

**Auth** — a shared secret in the `X-Agent-Token` header. `Authorization: Bearer
<token>` works too.

```
X-Agent-Token: <API_TOKEN>
```

Every route in this document is behind it except `GET /ping` and `GET /tools`. If
`API_TOKEN` is unset on the server the middleware is a no-op, so local
development works with no header at all — do not build on that, the demo
deployment will have it set. A bad or missing token is `401` with
`{"error": "missing or invalid X-Agent-Token"}`.

**CORS** — handled, including preflight, and preflight is answered before routing
so an `OPTIONS` to a path that does not exist still gets a clean `204`. Allowed
methods are `GET, POST, OPTIONS`; allowed headers are `Content-Type`,
`X-Agent-Token`, `Authorization`. With `CORS_ORIGINS` unset any origin is
permitted, which is fine for a Vite dev server on `:5173`. Tell me the origin the
demo will be served from and it goes on the list.

**Content type** — JSON in, JSON out. Every error, at every status code, is
`{"error": "<a sentence>"}`. The sentences are written to be shown to a person;
they name the specific thing that is wrong rather than saying "bad request", so
putting one straight in a toast is a reasonable default.

**Times** are RFC 3339 UTC. Fields ending `_at` that can be absent are omitted
entirely rather than sent as `null`.

---

## The flow

```
  incident on SQS
        │
        ▼
  GET /agent/:id                     the verdict: ROLLBACK or HOTFIX
        │
        │  (HOTFIX only — remediation starts automatically from the queue)
        ▼
  GET /agent/:id/remediation         triage + ranked candidates      ← poll this
        │
        ├── POST /solutions/:candidate/pr      opens a draft PR
        │
        └── POST /agent/:id/decision           records what was chosen, and why
                                               the rest were not
```

`:id` is an **investigation id** throughout. Candidates have their own ids and
the two are not interchangeable — posting a candidate id where an investigation
id belongs gets a `404`, not a wrong answer.

---

## `GET /repositories`

The service → repository mapping. Worth calling once at load: it is the only way
to know whether "verified" means anything for a given service.

```json
{
  "count": 1,
  "repositories": [
    {
      "repository": {
        "service_id": "checkout",
        "owner": "example",
        "repo": "checkout-service",
        "default_branch": "main",
        "subdirectory": "src/checkout",
        "runtime_image": "golang:1.26",
        "setup_command": "go mod download",
        "build_command": "go build ./...",
        "test_command": "go test ./..."
      },
      "has_tests": true,
      "url": "https://github.com/example/checkout-service"
    }
  ]
}
```

`has_tests` is the one to read. A service with no test command can never produce
a candidate stronger than "it compiles", and the UI should say so rather than
showing a green tick that means less than it looks like it means.

`url` is safe to render — any embedded token is stripped before it leaves the
server.

**`503`** if no repository mapping or database is configured.

---

## `GET /remediations`

Recent runs, newest first. For a dashboard or a list page.

Query: `?limit=50` — default 50, values above 200 or below 1 fall back to 50.

```json
{
  "count": 2,
  "remediations": [
    {
      "investigation_id": "7f7ffef8-...",
      "incident_id": "INC-2026-0810-checkout",
      "service_id": "checkout",
      "status": "done",
      "triage": { "...": "see below" },
      "started_at": "2026-08-14T09:12:04Z",
      "finished_at": "2026-08-14T09:18:41Z"
    }
  ]
}
```

**Candidates are deliberately not included here.** Each carries a full diff, and
a list page that shipped every diff for every run would be megabytes. Fetch the
detail endpoint for the run the engineer clicks on.

The same omission means this endpoint cannot yet tell you "3 fixes · 1 chosen"
for a row — see [Not there yet](#not-there-yet).

**`503`** if the database is not configured.

---

## `GET /agent/:id/remediation`

The one the picker is built on. Triage, every candidate ranked best-first, and
the engineer's decision if one has been recorded.

```json
{
  "investigation_id": "7f7ffef8-...",
  "incident_id": "INC-2026-0810-checkout",
  "service_id": "checkout",
  "status": "done",
  "repository": { "...": "as in GET /repositories" },
  "triage": {
    "status": "confirmed",
    "commit": "51e85e6",
    "commit_exists": true,
    "commit_subject": "money: carry nanos in Sum",
    "files": ["money/money.go"],
    "evidence": "Sum accumulates into an int32...",
    "confidence": 0.8
  },
  "candidates": [ "..." ],
  "decision": { "..." },
  "started_at": "2026-08-14T09:12:04Z",
  "finished_at": "2026-08-14T09:18:41Z"
}
```

### `status`

| Value | Meaning |
|---|---|
| `running` | in flight. Poll. |
| `done` | finished; read `candidates`. |
| `stopped` | triage said the fault is not in that commit, so no containers were spent. `candidates` is empty and `triage.status` says why. |
| `failed` | it broke, or the process died and a later boot reclaimed the row. `error` carries the sentence. |

Poll on `running` — every 3–5s is plenty; a full run is minutes, not seconds.
There is no websocket or SSE.

`stopped` is not an error and should not be rendered as one. It is the system
declining to spend money on a commit it could not implicate, and `triage.evidence`
is worth showing: it is the most concise explanation of why there is nothing to
pick.

### `triage.status`

`confirmed` · `inconclusive` · `not_present` · `commit_missing`

The first two proceed to candidates; the last two stop the run. `inconclusive`
proceeding is deliberate — the gate exists to stop obvious waste, not to overrule
an incident because a small model could not decide.

### A candidate

```json
{
  "id": "f3e2f376-...",
  "investigation_id": "7f7ffef8-...",
  "incident_id": "INC-2026-0810-checkout",
  "service_id": "checkout",
  "strategy": "defensive",
  "summary": "Accumulate in int64 and derive the carry arithmetically.",
  "rationale": "The overflow is in the accumulator, not the carry logic...",
  "files": ["money/money.go"],
  "diff": "diff --git a/money/money.go...",
  "verification": {
    "applied": true,
    "build_ran": true,
    "built": true,
    "test_ran": true,
    "tested": true,
    "timed_out": false,
    "duration_ms": 41200,
    "log": "..."
  },
  "repairs": 1,
  "status": "proposed",
  "pr_url": "",
  "rejection_reason": "",
  "error": "",
  "created_at": "2026-08-14T09:15:22Z"
}
```

**`strategy`** is one of `minimal`, `defensive`, `root-cause` — the three prompt
variations that ran. They are deliberately different from each other, so showing
the name next to each candidate is how an engineer tells apart two fixes that
look similar at a glance.

**`repairs`** is how many times that candidate was shown its own build or test
failure and asked again. Non-zero is worth surfacing: a fix that took three goes
deserves a closer read than one that landed first time.

**`diff`** is `git diff` taken from inside the sandbox after the change was
applied — what the change *really* was, not what the model said it would be.
Render this, not `summary`, when the engineer wants to know what will land.

**`error`** means the candidate never got as far as a usable change. Distinct from
being rejected by a person. A candidate with a non-empty `error` and an empty
`diff` is a failure to show honestly, not a fix to offer.

### `verification` — please read this bit

Every field is a fact about a stage that actually executed, and the pairs are
separate on purpose:

| Field | Question it answers |
|---|---|
| `applied` | did the patch land on a clean checkout at all |
| `build_ran` / `built` | was a build attempted / did it succeed |
| `test_ran` / `tested` | were the service's tests attempted / did they pass |
| `timed_out` | the sandbox hit its ceiling; whatever it reached by then still stands, nothing after it ran |

`build_ran: false` and `built: false` do **not** mean the build failed. They mean
no build was attempted, usually because the service declares no build command.
Rendering "✗ build" for that is a false claim about the code.

The server already has the sentence for this. Every candidate's verification maps
to exactly one of:

- `the change could not be applied to a clean checkout`
- `builds, and the service's tests pass`
- `builds, but the service's tests fail`
- `builds; this service declares no tests, so nothing was proven beyond that it compiles`
- `does not build`
- `applied, but the sandbox hit its time limit before it could be verified`
- `applied, but this service declares neither a build nor a test command, so nothing was verified`

**Use these strings, and do not reword them.** "Builds" and "the service's tests
pass" are different sentences because they are different claims, and the whole
discipline this system is built around is not collapsing the second into the
first. A green tick that means both is the one UI decision that would undo it.
Colour-code by all means — but the words are load-bearing.

(They are currently only computed server-side for the decision document, so today
you would derive them from the booleans. If you would rather they came down on
the wire as `verification.summary`, say so and I will add the field — it is five
minutes and it removes the chance of the two implementations drifting.)

### `candidates` ordering

Already ranked, best first. Strength dominates: a candidate whose tests pass
always outranks one that merely compiles. Ties break toward the smaller diff,
because the smallest change carrying the same evidence is the one an engineer can
review under pressure — which is the situation this exists for.

Render in the order given. Re-sorting client-side discards the ranking.

### `decision`

Present once an engineer has decided; absent otherwise. Its absence is how you
tell an undecided run from a decided one in the call you already make.

```json
{
  "id": "b41c...",
  "investigation_id": "7f7ffef8-...",
  "incident_id": "INC-2026-0810-checkout",
  "service_id": "checkout",
  "chosen_candidate_id": "f3e2f376-...",
  "rejections": [
    {
      "candidate_id": "9a1b...",
      "strategy": "minimal",
      "reason": "does not build",
      "source": "verification"
    },
    {
      "candidate_id": "c7d2...",
      "strategy": "root-cause",
      "reason": "rewrote more than the incident justified",
      "source": "engineer"
    }
  ],
  "engineer_fix": "",
  "engineer_pr_url": "",
  "notes": "prefer the arithmetic carry; easier to prove",
  "document": "Remediation decision (2026-08-14) for incident ...",
  "decided_at": "2026-08-14T09:31:02Z",
  "indexed_at": "2026-08-14T09:31:04Z"
}
```

**`source` matters.** `engineer` is a sentence a human typed. `verification` is
one the sandbox derived. Render them differently — an auto-derived "does not
build" presented in quotation marks as somebody's opinion is a small lie that
gets repeated every time the page is opened.

`chosen_candidate_id` absent means every candidate was rejected and the engineer
fixed it themselves; `engineer_fix` is what they did. This is the most valuable
record in the system and deserves to look like a result, not like a failure.

`indexed_at` absent means the decision is recorded but was never embedded, so it
will not influence future incidents. Worth a quiet indicator somewhere; it is the
difference between the feature working and the feature merely appearing to.

**`503`** if the database is not configured. **`404`** if that investigation has
no remediation — which is also what "not remediated yet" and "still waiting for a
free slot in the pool" look like, so a `404` here is not necessarily an error to
show.

---

## `POST /agent/:id/remediation`

Start remediation by hand. The queue path does this automatically for a HOTFIX
verdict; this is the demo's button, and the way to retry a run that was dropped
because the pool was full.

No body.

**`202 Accepted`:**

```json
{
  "investigation_id": "7f7ffef8-...",
  "status": "queued",
  "poll": "/agent/7f7ffef8-.../remediation"
}
```

The work outlives the request by design. Follow `poll`.

| Status | Meaning |
|---|---|
| `202` | queued |
| `404` | no investigation with that id |
| `409` | that investigation has not reached a verdict yet, **or** it is already being remediated / the pool is full. The `error` sentence distinguishes them. |
| `422` | the investigation implicated no commit, so there is nothing for triage to read |
| `503` | no container runtime, no repository mapping, or no worker |

`409` is recoverable — retry once the verdict lands or a slot frees. `422` is
terminal for that investigation: no commit, no fix.

---

## `POST /solutions/:candidate/pr`

Opens a draft pull request from a candidate. **Picking is what touches the
repository — nothing before this point does.**

Note the path takes a **candidate** id, not an investigation id.

No body.

**`201 Created`:**

```json
{
  "candidate_id": "f3e2f376-...",
  "pr_url": "https://github.com/example/checkout-service/pull/42",
  "verified": "builds, and the service's tests pass"
}
```

**`200 OK`** if a draft was already open — a double-click is not an error, and
opening a second identical draft would be worse than saying so:

```json
{ "candidate_id": "f3e2f376-...", "pr_url": "...", "already_open": true }
```

**`200` with a `warning`** is the awkward one: the draft opened, but recording the
URL failed. The response still carries `pr_url`, and it may be the only place
that URL exists — show it, and do not silently retry, which would open a second
PR.

| Status | Meaning |
|---|---|
| `201` | opened |
| `200` | already open, or opened-but-unrecorded (check for `warning`) |
| `404` | no candidate with that id |
| `422` | that candidate never applied cleanly, so there is nothing to open a PR with; or the service has no repository mapping |
| `502` | GitHub refused |
| `503` | no database, no repository mapping, or no GitHub token configured |

Opening a PR is *not* a decision. It moves the candidate to `pr_opened` and
nothing else. The decision is the next endpoint, and the two are independent by
design — an engineer can open a draft to look at it and still choose a different
candidate.

---

## `POST /agent/:id/decision`

Where the loop closes. The engineer's judgement is written down, embedded, and
recalled the next time a similar fault shows up. It is the only point in the
pipeline where a human's reasoning is captured; everything before it records what
a model produced or what a container proved.

```json
{
  "chosen_candidate_id": "f3e2f376-...",
  "rejections": [
    { "candidate_id": "c7d2...", "reason": "rewrote more than the incident justified" }
  ],
  "engineer_fix": "widened the accumulator in Sum and left the branch structure alone",
  "engineer_pr_url": "https://github.com/example/checkout-service/pull/44",
  "notes": "prefer arithmetic carry over branch-per-sign; easier to prove"
}
```

Every field is optional on its own. Send only what the engineer actually typed.

| Field | Cap | Notes |
|---|---|---|
| `chosen_candidate_id` | — | must be a candidate of *this* investigation |
| `rejections[].candidate_id` | — | required if the array is present; same constraint |
| `rejections[].reason` | 1000 | free text |
| `engineer_fix` | 4000 | what they did instead |
| `engineer_pr_url` | 500 | must parse as a URL |
| `notes` | 2000 | free text |

Nothing factual is accepted from the client — not the service, not the strategy,
and above all not what was verified. All of that is read back from the stored run.
This text becomes context that shapes future incidents, so a caller must not be
able to assert that something passed tests it never ran.

**`201 Created`:**

```json
{
  "decision_id": "b41c...",
  "investigation_id": "7f7ffef8-...",
  "chosen_candidate_id": "f3e2f376-...",
  "rejected": 2,
  "reasons_from_engineer": 1,
  "reasons_auto": 1,
  "indexed": true,
  "document": "Remediation decision (2026-08-14) for incident INC-2026-0810-checkout, service checkout.\n\nIncident: ...\n\nChosen fix (defensive strategy): ...\nWhat was verified: builds, and the service's tests pass.\n\nRejected:\n- minimal: does not build [from the sandbox]\n- root-cause: rewrote more than the incident justified [from the engineer]\n\nNote from the engineer: ..."
}
```

`document` is the prose that was embedded — literally what the system learned.
Showing it after a successful save is cheap and makes the feature legible instead
of magic; a bad document becomes obvious now rather than three incidents from now.

`indexed: false` arrives with a `warning` and means the decision is stored but
will not be recalled. Not a failure to retry — the record is safe — but worth
showing.

| Status | Meaning |
|---|---|
| `201` | recorded |
| `400` | malformed body, a field over its cap, or **nothing was actually decided**: no chosen candidate, no `engineer_fix`, no `notes` |
| `404` | no remediation for that investigation |
| `422` | a candidate id that is not part of this investigation |
| `503` | no database |

### Three rules that are not visible in the shapes

**1. Re-deciding replaces. It does not merge.**

Posting again deletes the previous decision outright and writes a new one. If the
second post omits a reason the engineer typed the first time, that prose is gone —
the candidate falls back to its auto-derived reason.

So: when an engineer edits a decision, **send the complete decision every time**,
not a delta. Populate the edit form from the `decision` object on
`GET /agent/:id/remediation` and post all of it back.

This is deliberate. Two contradictory precedents on file are worse than either
one alone, and merging would mean guessing which of two conflicting judgements
the engineer still holds.

**2. Unmentioned candidates are auto-rejected.**

Any candidate that is neither chosen nor explicitly rejected gets a rejection
derived from what the sandbox established, tagged `source: "verification"`. The
common case — engineer picks one and types nothing — still produces a complete
record. The UI does not need to send a rejection for every candidate, and should
not invent reasons to fill the gaps.

The interesting case is the opposite: a candidate that built *and* passed its
tests and was rejected anyway. That is the only signal here the system could not
have computed for itself, so it is worth making the reason field easy to reach
and unintimidating — a text box next to each candidate, not a modal.

**3. A candidate with a PR open is never moved.**

`pr_opened` outranks both `selected` and `rejected` in the stored status. That URL
is the only link from the incident back to the change, and a status that no
longer said `pr_opened` would strand it.

Practically: a chosen candidate may read back as `pr_opened` rather than
`selected`. Do not treat that as the decision failing to save — check
`decision.chosen_candidate_id`, which is authoritative. Candidate `status` is a
lifecycle marker; the decision is the record.

### Candidate `status` values

`proposed` · `selected` · `rejected` · `pr_opened` · `failed`

`rejected` and `failed` are different things. `failed` is the sandbox refusing a
candidate. `rejected` is a person passing over one that may well have built and
passed its tests — which is the judgement worth learning from.

---

## Supporting endpoints

### `GET /agent/:id`

The investigation itself: the verdict, and the full trace of every tool call.

```json
{
  "investigation_id": "7f7ffef8-...",
  "incident_id": "INC-2026-0810-checkout",
  "service_id": "checkout",
  "status": "done",
  "result": {
    "answer": "HOTFIX. The implicated commit is 51e85e6...",
    "verdict": {
      "decision": "HOTFIX",
      "commit_sha": "51e85e6",
      "service": "checkout",
      "confidence": 0.8,
      "source": "declared"
    },
    "sources": 4,
    "iterations": 3,
    "trace": [ "..." ],
    "grounding": ["past incidents recalled from the index"],
    "precedents": ["past decisions this team recorded"],
    "truncated": false
  },
  "started_at": "...",
  "finished_at": "..."
}
```

`status`: `running` · `done` · `failed` · `retrying`.

`verdict.decision` is `ROLLBACK` · `HOTFIX` · `UNKNOWN`. Only HOTFIX leads to
candidate fixes — a ROLLBACK verdict is the answer, and there is nothing to pick.

`verdict.source` says how the verdict was arrived at: `declared` (the model
emitted the structured block it was asked for), `inferred` (read out of the
prose), `absent` (neither). An `inferred` verdict is worth a visual hedge; it is
the parser's reading, not the model's statement.

`precedents` is the decisions loop feeding back — past judgements this team
recorded, recalled because they resembled this incident. Distinct from
`grounding`, which is incident history. Keeping them visually separate is the
honest presentation: one is what happened, the other is what someone thought
about it.

**`404`** if there is no investigation with that id. **`503`** if no queue worker
is configured.

### `GET /ping`

Liveness. No auth, depends on nothing.

### `GET /tools`

The MCP tools discovered at boot with their JSON Schemas. No auth. Useful if you
want a "what can this thing see" panel; not needed for the picker.

---

## Not there yet

Say the word on any of these and they get built — none is large.

- **Candidate counts on `GET /remediations`.** A list row cannot currently show
  "3 fixes · 1 chosen" without an extra fetch per row. Needs a `GROUP BY` in the
  list query.
- **A capability endpoint.** Right now the only way to learn that PR opening is
  unconfigured is to try it and get a `503`. A single `GET /capabilities` would
  let the UI disable the button up front instead of surfacing an error the
  engineer cannot act on.
- **`verification.summary` on the wire.** As above — currently derived
  client-side from the booleans.
- **`?dry_run=1` on the decision endpoint**, returning the `document` that
  *would* be embedded without writing anything. Useful if you want a preview
  before the engineer commits.
- **Websockets / SSE.** Polling only. Given run times of minutes, polling is
  honestly fine.
- **Pagination beyond `limit`.** No cursor, no offset.

## If something here is wrong

The behaviour is what the server does; this file is a description of it, and
descriptions drift. If they disagree, the server wins and this file is the bug —
tell me and I will fix it.
