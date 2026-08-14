# What's left

> **Superseded — read [`plans/next-steps.md`](plans/next-steps.md) instead.**
>
> This file is from 2026-08-07 and most of it is now wrong: the work is pushed, auth landed,
> embeddings moved to OpenRouter, Cloud MCP works, and the whole remediation half described
> nowhere below exists and has opened a real draft pull request. Kept for the App Runner
> timeout research and the token-cost method, which are still sound. Everything else here
> should be checked against the code before it is believed.

Written 2026-08-07, updated the same day after a working session. Deadline 2026-08-18, so
**11 days**.

## Done since this was written

- **`/agent` is 5.6× faster.** 370.4s → 66.0s [measured]. The tightened prompt did it, and the
  cause was what I guessed: `SHOW JOBS` is gone, tool output fell from 6151 to 1379 chars.
- **Seeded the cluster with a real schema** (`scripts/seed-cluster.sql`). This turned out to matter
  much more than latency — see the new blocker note below.
- **API authentication** on `/store`, `/retrieve`, `/ask`, `/agent`. Verified 401/401/200/200.
- **Container image builds**, 56.5 MB. First time it had ever been built.
- **Fixed a reporting bug of mine**: tool-level errors were recorded as `failed: false`, so a run
  where 6 of 8 calls errored reported "0 failed". This one had been actively misleading me.
- **`bench.sh` percentage rounding** — `0.04%` now, was `0%`.

Still open, and unchanged: push, Cloud MCP, deploy target, embeddings migration, demo video.

Not tracked by git on purpose. Delete it when it stops being useful.

Everything below is labelled by how much I actually know:

- **[measured]** — I ran it and read the number off the output.
- **[verified]** — I observed it directly (a log line, a schema, an API response).
- **[estimated]** — arithmetic on measured inputs; the method is shown so you can check it.
- **[unverified]** — I believe it but have not tested it. Treat with suspicion.

---

## Where things stand

Committed at `21a0964`; **this session's changes are still uncommitted** (see blocker 1). What
works, end to end, fully offline:

| Path | Status | Latency [measured] |
|---|---|---|
| `/store`, `/retrieve` | working | 0.01s |
| `/ask` | working | 15.2s |
| `/agent` | working | **66.0s** — was 370.4s before the prompt fix |
| `/tools` | working | instant, 15 discovered / 10 offered |

The `/agent` breakdown is the number that matters [measured]:

```
MCP tool calls      0.06s    0.04%   5 calls, 0 failed
model + overhead   65.90s   99.96%   5 iterations
```

**CockroachDB is not the bottleneck by three orders of magnitude.** That's a good story for the
submission and it's reproducible with `./scripts/bench.sh`.

---

## Blockers, in the order they'll hurt

### 1. Nothing is pushed yet — but the remote now exists

`origin` is set to `github.com/Look-Its-Sky/cockroachdbxaws.git`, and branch `agent_space` is not on
it yet. One commit (`21a0964`) exists on this laptop only, on a machine that **crashed today
mid-session**. That crash cost us the running containers; the next one could cost the repo.

`git push -u origin agent_space` and this is closed. Still the highest value-per-second item here.

Note there are now **uncommitted changes on top of that commit** from this session — the prompt fix,
the auth middleware, the seed script, the `failed`-flag fix. Those are the changes that made
`/agent` 5.6× faster, so they are worth more than the original commit.

### 2. App Runner will kill `/agent` — this changes the design [verified]

App Runner's per-request timeout is **120 seconds and is not configurable**. There's an open
roadmap issue asking for it ([aws/apprunner-roadmap#104](https://github.com/aws/apprunner-roadmap/issues/104)),
still open. Some users report being cut off at 30s rather than 120s.

**This got much less severe.** `/agent` is now **66s** [measured], down from 370s, and OpenRouter
should be faster still. 66s fits inside 120s.

But it fits with 54 seconds of headroom, on a run that varied between 66s and 135s across three
measurements today. That variance is the problem: the ceiling is hard, and a 2× bad day breaches it.
I would not bet a judged demo on it without either the async path or a measured OpenRouter number
showing a comfortable margin.

Three ways out, in order of how much I'd trust them:

- **Make `/agent` asynchronous.** `POST /agent` enqueues, returns `{"id": ...}` immediately;
  `GET /agent/{id}` polls for the result. This is maybe 80 lines with an in-memory map, and it is
  immune to *any* provider latency. It also fixes the client-disconnect problem below. **This is
  what I'd do.**
- **Stream with SSE.** Keeps the single-request UX, but App Runner + SSE has its own reported
  trouble, so this trades a known problem for an unknown one.
- **Get the run under ~60s and hope.** Fragile. One slow provider day and the demo 504s in front of
  a judge.

Related, and true today even locally: the `/agent` handler uses `c.Request.Context()`, so **a client
disconnect cancels the run server-side** and returns 500. I hit this three times with `curl -m 300`
before realising it was me, not the server. A judge closing a tab, or a proxy timing out, kills the
request. The async design removes this entirely.

### 2b. The demo needs seeded cluster data — do not skip this [measured]

This wasn't in the original doc and it's the most important thing I found today.

Before seeding, the agent's live-cluster half was **doing nothing**. On a cluster holding only
`langchain_pg_embedding` and `langchain_pg_collection`, it invented a `prod_db` database with
`checkouts` and `cart_items` tables. Six of its eight tool calls errored:

```
list_tables      {"database":"prod_db"}   → target database or schema does not exist
get_table_schema {"database":"prod_db"}   → database "prod_db" does not exist
get_table_schema {"table":"checkouts"}    → relation ... does not exist
```

It still answered **ROLLBACK, commit a91f3c2, citing INC-412** — correctly. That is the trap: the
answer came entirely from the vector store, the live-cluster integration contributed zero, and
nothing in the output said so. A judge reading that trace would see a system that looks grounded and
isn't.

`scripts/seed-cluster.sql` fixes it — `services`, `deploys`, `request_latency`, with latency
degrading exactly when `a91f3c2` ships. After seeding [measured]:

```
1  list_databases
2  list_tables       {"database":"defaultdb"}
3  get_table_schema  {"database":"defaultdb","table":"deploys"}
4  select_query      SELECT commit_sha, deployed_at FROM deploys WHERE service='checkout' ...
5  select_query      SELECT summary FROM deploys WHERE commit_sha='a91f3c2'
```

Five calls, zero failures, and the commit is now *derived from the cluster* and corroborated against
past incidents — which is the actual pitch. **Run the seed script before recording anything**, and
run it against whatever cluster the demo points at, including the cloud one.

Related: this is also why the `failed`-flag bug mattered. The trace reported all six failing calls
as `failed: false`, so the empty-cluster problem was invisible in exactly the artifact meant to
reveal it. Fixed.

### 3. The Cloud MCP path has never once succeeded [unverified]

Every MCP thing I've proven ran against the **self-hosted** `cockroachdb-mcp-server` container.
`COCKROACH_API_KEY` has been empty for the whole project, so `https://cockroachlabs.cloud/mcp` has
never received a successful request from this code.

The submission's headline is the Cloud Managed MCP Server. Discovering on the 17th that the auth
shape is wrong would be very bad.

Honest risk assessment, having thought about it more than I did the first time I raised it: **lower
than it sounds.** The adapter discovers tools at runtime and passes their schemas through verbatim,
so a different tool list genuinely cannot break it — that's by construction, and it's the reason I
built it that way instead of hard-coding tools. What's actually untested is narrow:

- Does `Authorization: Bearer <service-account-key>` work, versus the self-hosted bearer token?
- Does the `mcp-cluster-id` header do what I assume?
- Does the streamable-HTTP transport behave the same against their edge?

That's a ~10 minute check once a key exists. Make a service account key in the Cloud Console under
Access Management, then:

```sh
COCKROACH_API_KEY=<key> COCKROACH_CLUSTER_ID=<uuid> \
COCKROACH_MCP_URL=https://cockroachlabs.cloud/mcp \
DATABASE_URL='<your cloud url>' go run .
curl -s localhost:8080/tools | jq '{discovered, offered_to_agent, withheld_writes}'
```

If the tool list comes back non-empty, this is done. One thing to watch: the Cloud server caps
queries at 20s while our client allows 90s per request (`utils/mcp/client.go:37`), so a slow query
surfaces as *their* error, not a client timeout. That's the correct behaviour, just don't be
confused by it.

### 4. AWS access is unproven

- No `aws` CLI on this machine.
- IAM user `jude` (account 071954287023) has almost no attached policies — S3 and Lambda both
  returned plain `AccessDenied` per the handoff.
- `scripts/deploy.sh` is 160 lines and **has never been executed once**.
- Bedrock is dead on this account. Don't re-diagnose it; it's a known dead end.

Three unknowns stacked, and first-run debugging of a deploy script is exactly where a day
disappears. You said you're not sure what you're deploying to yet — that's fine, but decide early,
because item 2 above means the target's request-timeout behaviour is a *design input*, not a
detail. If you pick something without a hard request cap (ECS behind an ALB with a long idle
timeout, or a plain EC2 box), the async work in item 2 becomes optional rather than mandatory.

---

## What needs to be faster

**Mostly solved.** The prompt fix landed and the hypothesis held [measured]:

| | before | after |
|---|---|---|
| wall clock | 370.4s | 66.0s |
| tool output | 6151 chars | 1379 chars |
| worst single call | `show_statement` 4011 chars | `list_databases` 381 chars |
| `SHOW JOBS` / `get_cluster` | called | not called |

The diagnosis was right: one bad tool call was 65% of all tool output, `SHOW JOBS` migration
history, re-sent on every subsequent iteration. Steering the model away from cluster introspection
removed it, and 5.6× fell out.

Caveat worth keeping in view: **run-to-run variance is high.** Three measurements today gave 370s,
135s and 66s, and the last two were the same code. Some of that is genuine (different tool paths),
some is a local model under varying load. Do not quote 66s as if it were reliable without more
runs — `./scripts/bench.sh -e agent -n 5` would settle it.

### Still on the table

- **`maxToolOutputChars` is 4000** (`agent/runner.go:33`). `show_statement` hit the cap exactly, so
  the cap is doing something, just not enough. Dropping to ~1500 would cut the worst case, but it
  also truncates legitimately large `SELECT` results, which is a real cost — that's why I'd rather
  fix tool *choice* than tool *output*. Try the prompt fix first and only reach for this if it
  isn't enough.
- **`defaultMaxIterations` is 6** (`agent/runner.go:26`). Real runs used 3, so lowering it to 4
  wouldn't have changed any observed run and would cap the tail. Cheap insurance against a model
  that loops.
- **`maxResultChars` is 6000** in `utils/mcp/tool.go:16`, and `maxToolOutputChars` is 4000 in the
  runner. Two independent truncations in series; the 6000 one never binds. Not a bug, but it's
  confusing and someone will eventually tune the wrong one.
- **Prompt caching.** The static prefix — system prompt plus 10 tool schemas, ~2400 tokens
  [estimated] — is re-sent on all 3 iterations plus the final summarise. If the provider supports
  caching that prefix, that's ~7000 redundant prompt tokens per run recovered [estimated]. Worth
  checking what OpenRouter exposes for the model you settle on.

### The honest framing

Most of the 370s is that a 30B model on local hardware reprocesses a growing prompt four times.
**You've already said you're using OpenRouter for the real demo**, which makes most of this moot for
the video. The reason to still do the prompt and tool-choice work is that on OpenRouter the same
bloat converts from *seconds* into *money*, and into headroom against that 120s wall.

---

## What needs to be cheaper

### Token math per `/agent` run [estimated]

Method: ~4 chars/token, plus one measured anchor — a bare tool-calling request with 15 tool schemas
came back at **3182 prompt tokens** [measured], so the schema block dominates the fixed cost.

| Component | Tokens |
|---|---|
| System prompt + 10 tool schemas (now 10, was 15) | ~2400 |
| Grounding: 4 recalled incidents | ~200 |
| Tool results accumulated by iteration 3 | ~1400 |
| **Prompt tokens, summed over 3 iterations + summarise** | **~14,700** |
| Completion tokens | ~1000 |

Killing the `SHOW JOBS` call alone removes ~1000 tokens that get re-sent twice — roughly **14% of
the run** [estimated]. Prompt caching on the static prefix would be a much bigger win if available.

I have deliberately **not** put dollar figures here. I don't have current OpenRouter pricing for
`z-ai/glm-5.2` and I'm not going to invent it. Multiply the table above by whatever the model
actually costs.

### The expensive mistake waiting to happen

**The API has no authentication at all.** Every route is open. Deploy it to a public URL with an
OpenRouter key in the environment and:

- Anyone who finds the URL can call `/agent`, and each call is ~15k tokens on your account.
- `/store` lets anyone write into your vector index, including into your **live cloud cluster**.
- There is no rate limit, so a bored scraper is an unbounded bill.

**Item 1 is now done.** `/store`, `/retrieve`, `/ask` and `/agent` require `X-Agent-Token`
(or `Authorization: Bearer`), constant-time compared, from `API_TOKEN`. `/ping` and `/tools` stay
open. Verified: no token 401, wrong token 401, right token 200. With `API_TOKEN` unset it is a no-op
and the server warns at boot. `demo.sh` and `bench.sh` pick the token up automatically.

I gated `/retrieve` as well as the obvious ones — it embeds its query (a billable call) and reads
your stored incidents back out, so leaving it open would have been a data leak as much as a cost
one.

Two still on you, and neither is code:

1. **A hard spend cap on the OpenRouter side.** Do this even if you do nothing else — it is the only
   control that still works when the code is wrong.
2. **`GIN_MODE=release`** in the deployed environment. It is in debug mode right now, logging every
   route and every request.

### Embeddings on OpenRouter — checked, and it's fine [verified via docs, not by calling it]

I was worried OpenRouter had no embeddings endpoint, which would have been a deploy blocker. It
does: `POST https://openrouter.ai/api/v1/embeddings`, OpenAI-compatible, bearer auth, fronting
OpenAI/Cohere/Google/Mistral models.

But switching embedding providers is **not** a config-only change:

- The new model will have a different width (1024, 1536, …) than the current 768.
- `VECTOR_DIMENSIONS` must be updated to match **and** the tables recreated, because the column is
  sized at creation: `go run ./cmd/nuke -mode=drop`, then restart.
- Everything already stored gets re-embedded, since old vectors are meaningless in the new space.

Do this migration deliberately and early, not the night before. It is exactly the failure I already
walked into once with `expected 1024 dimensions, not 768`.

**Done.** Chat and embeddings now resolve endpoints separately. `OPENAI_BASE_URL` moves chat only;
embeddings stay on OpenRouter unless `EMBEDDING_BASE_URL` says otherwise. The split went the
opposite way to what this note assumed — embeddings are the half you want *hosted*, since the
column width is baked in at `CREATE TABLE` and a silent 1024→768 swap costs a full re-embed, while
the tokens cost ~$0.01/M. The old per-provider model/dimension variables are gone in favour of one
name each: `OPENROUTER_MODEL`, `OPENROUTER_EMBEDDING_MODEL`, `OPENROUTER_EMBEDDING_DIMENSIONS`.

---

## Correctness and robustness

- **Concurrency is unverified.** `Runner` claims to be safe for concurrent use, and I believe the
  MCP session is too, but two simultaneous `/agent` calls have never been tested. If the demo
  involves anyone else poking the URL while you present, find out first.
- **`/store` and `/ask` use `context.Background()`**, not the request context, so they ignore
  cancellation entirely. Inconsistent with `/agent`, which uses the request context and is *too*
  sensitive to it. Both should probably use the request context with an explicit timeout.
- **No structured logging or request IDs.** Fine for a hackathon; painful if something misbehaves
  live and you're reading interleaved gin output on a stage.
- **The image now builds** — `docker build -t agent_space:test ./agent_space`, 56.5 MB, first time
  it had ever been run. The image has still never been *started*, and the production
  `docker-compose.yml` is still unexercised.
- **`scripts/demo.sh` steps 3-4 have never been run.** The demo script is the thing you'll lean on
  while recording. Run it end to end at least once before you're recording.

---

## Decisions only you can make

- **Synthetic incident rows in the live cloud cluster.** INC-412 and INC-388 are still sitting in
  the production CockroachDB Cloud cluster from earlier testing. Keep them as demo seed data, or
  clear them out? They're harmless but they are fake data in a real database.
- **The LICENSE name.** Deferred at your request; still says what it says.
- **Deploy target.** Per blocker 2, this is a design input. Pick before building the async path, so
  you don't build it for a platform that didn't need it.

---

## Small stuff

- `agent_space/MCP_HANDOFF.md` is committed and now partly stale. Either update it or drop it;
  right now it's a document that contradicts the code in places.
- `scripts/test_api.sh` predates everything and still tests "Agent Antigravity". Harmless, but it's
  the first script a judge might open.

---

## If I had the 11 days, in this order

1. **Commit and push.** `git push -u origin agent_space`. The uncommitted work is the valuable
   part — it is what made `/agent` 5.6× faster and closed the auth hole.
2. **Verify Cloud MCP.** ~10 min once a key exists. De-risks the headline claim.
3. **Set the OpenRouter spend cap.** Two minutes in their dashboard, and it is the backstop for
   every mistake below.
4. **Pick the deploy target**, then decide whether `/agent` needs to become async. Don't invert
   these two — 66s fits in App Runner's 120s, but not with margin I'd trust.
5. **Migrate embeddings to OpenRouter**, with the `nuke -mode=drop` dimension change, then re-seed
   both the vector store *and* the cluster (`scripts/seed-cluster.sql`).
6. **Run `demo.sh` end to end** and start the built image at least once.
7. **Record the video** against OpenRouter, not the local model — and against a *seeded* cluster.

Items 1-3 are about twenty minutes combined and remove most of the catastrophic risk. Item 4 is the
one that can eat days if the answer is "async", so decide it early.

---

Sources for the two things I looked up rather than recalled:

- [Configurable request timeout · aws/apprunner-roadmap#104](https://github.com/aws/apprunner-roadmap/issues/104)
- [AWS App Runner: what exactly does the timeout limit mean? (re:Post)](https://repost.aws/questions/QUnozEpub5Tnq-ilmrB4l2Sg/aws-app-runner-what-exactly-does-the-timeout-limit-documentation-mean)
- [Text Embedding Models — OpenRouter](https://openrouter.ai/collections/embedding-models)
- [Embeddings — OpenRouter docs](https://openrouter.ai/docs/client-sdks/typescript/api-reference/embeddings)
