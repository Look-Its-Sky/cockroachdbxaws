# Handoff: Wiring the CockroachDB MCP Server

> **STATUS (2026-08-06): the MCP task described below is implemented.** See the
> [README](../README.md) for the resulting architecture. This file is kept as a record of the
> reasoning, but several of its assumptions turned out to be wrong — corrections below. Trust the
> README and the code over this document.
>
> **Corrections found while implementing:**
>
> - **The endpoint is `https://cockroachlabs.cloud/mcp`**, auth is `Authorization: Bearer
>   <service-account-api-key>`, and the cluster is scoped with an `mcp-cluster-id` header. Env vars
>   are `COCKROACH_MCP_URL`, `COCKROACH_API_KEY`, `COCKROACH_CLUSTER_ID`.
> - **The Go SDK has no `NewStreamableClientTransport` and no `Headers` field.** It is a struct
>   literal, and auth headers must be injected via a custom `http.RoundTripper` on `HTTPClient`.
>   See `utils/mcp/client.go`.
> - **Do not use `agents.NewOpenAIFunctionsAgent` (step 4 below).** It hardcodes every tool's schema
>   to `{"__arg1": string}`, so the MCP JSON Schema never reaches the model — the exact problem step
>   3 identifies as the crux. `agent/runner.go` runs a native `llms.WithTools` loop that passes each
>   tool's real `InputSchema` through instead. The adapter still implements `tools.Tool`, so the
>   langchaingo path remains available.
> - **`OPENROUTER_MODEL` is `z-ai/glm-5.2`, not `cohere/north-mini-code:free`.** The tool-calling
>   risk in step 5 was measured (`go run ./cmd/toolcheck`): 20/20 well-formed calls. The concern
>   about the Cohere model is moot.
> - **The Dockerfile and docker-compose landmines are fixed.**

**For:** the next Claude instance picking up this repo.
**Date written:** 2026-08-06. **Hackathon deadline: 2026-08-18, 5:00pm EDT.**

## What this project is

An SRE agent for the CockroachDB × AWS hackathon. It maps production failures to the
commits that caused them and decides **rollback vs. hotfix** (rollback when a fix can't
land timely; hotfix when it can land same-day). CockroachDB is used as a semantic cache
of past problem→resolution pairs, so prior incidents ground new decisions.

Hackathon requires: **≥2 CockroachDB tools**, **≥1 AWS service**, deployed on AWS, a
public MIT/Apache repo, a demo URL, and a <3min video.

## Current state — what already works

Verified end-to-end against the live CockroachDB Cloud cluster and OpenRouter:

```
POST /store    -> {"message":"Stored successfully"}
POST /retrieve -> returns the matching incident
POST /ask      -> "you should roll back. The implicated commit is a3f9c21."  sources: 2
```

That `/ask` response is the core thesis working: vector recall driving a rollback
decision with commit attribution. `go build ./...` and `go vet ./...` pass.

Layout (module `agent_space`, Go 1.26.5):

- `agent_space/main.go` — Gin server; `initStore()`, `initLLM()`, three routes above.
- `agent_space/utils/llm.go` — the only provider abstraction. `GetLLM()`,
  `GetEmbedder()`, `EmbeddingDimensions()`.
- `agent_space/utils/crdbvector/` — hand-written pgvector-compatible vector store for
  CockroachDB. **This is CockroachDB tool #1 (Distributed Vector Indexing).**

## Decisions already locked in — do not relitigate

- **AWS Bedrock is dead on this account.** Every runtime call returns
  `ValidationException: Operation not allowed`, and `GetFoundationModelAvailability`
  reports `NOT_AUTHORIZED` for every model in every region, under both IAM keys and a
  Bedrock API key, while the control plane works fine. This is not an IAM problem and
  was not fixable with permissions. Don't re-diagnose it. **The AWS requirement is
  satisfied by deploying to ECS/App Runner instead.**
- **LLM is OpenRouter** (OpenAI-compatible; the `openai` langchaingo driver plus a base
  URL override). Chat model comes from `OPENROUTER_MODEL`, defaulting to
  `cohere/north-mini-code:free`.
- **Embeddings are `qwen/qwen3-embedding-8b` at 1024 dims** — $0.01/MTok, half the price
  of `text-embedding-3-small`, with Matryoshka truncation confirmed working.
- **CockroachDB tool #2 is the Cloud Managed MCP Server.** That is the task below.

## The MCP task

**Verified constraint: langchaingo v0.1.14 has no MCP support.** There is no `mcp`
package anywhere in the module. So this needs a real MCP client plus an adapter.

### 1. Pick a client library

Use the official Go SDK, `github.com/modelcontextprotocol/go-sdk`. (`mark3labs/mcp-go`
is the main alternative.) The CockroachDB Cloud Managed MCP Server is **remote**, so you
need the streamable-HTTP/SSE transport, not stdio.

### 2. Get the endpoint and credentials

**Do not guess the endpoint URL — I did not verify it and won't invent one.** Get the
server URL and auth scheme from the CockroachDB Cloud console / current docs. Expect
auth via a Cloud API key from a service account. Put it in `.env` as something like
`COCKROACH_MCP_URL` and `COCKROACH_API_KEY`, and read it through the same `envOr`
pattern already in `utils/llm.go`.

### 3. Bridge MCP tools onto `tools.Tool`

langchaingo's interface is minimal:

```go
type Tool interface {
	Name() string
	Description() string
	Call(ctx context.Context, input string) (string, error)
}
```

**The impedance mismatch is the crux of this task.** MCP tools take *structured JSON
arguments* described by a JSON Schema; `Call` gives you one opaque string. The adapter
must accept the model's string, `json.Unmarshal` it into `map[string]any`, and pass that
as the MCP tool's arguments — and surface the tool's input schema in `Description()` so
the model knows what JSON to produce. Handle unmarshal failure by returning a
descriptive `error` rather than panicking; agents recover by retrying when the tool
explains what went wrong. Put this in a new `agent_space/utils/mcp/` package; keep
`main.go` thin.

### 4. Swap the hand-rolled prompt for a real agent loop

`/ask` currently does one-shot retrieve→generate with `llms.GenerateFromSinglePrompt`.
Once tools exist, use `agents.NewOpenAIFunctionsAgent(model, tools)` wrapped in
`agents.NewExecutor(...)`. Keep `/ask` working as-is and add the agent behind a new
route so there's always a demoable path.

### 5. Verify tool calling actually works on the chosen model

**This is the highest-risk unknown.** `cohere/north-mini-code:free` is a reasoning model
that burns completion budget before emitting content — it returned `content: null` and
`finish_reason: "length"` at `max_tokens` 20 and 60, which is why `/ask` pins
`llms.WithMaxTokens(2000)`. Whether it emits *well-formed tool calls* reliably is
untested. Run ~20 loops and count malformed calls before building on it. If it's flaky,
switch `OPENROUTER_MODEL` to a paid Qwen coder model and pin provider/quantization in
the OpenRouter request to stop silent backend swaps.

## Landmines

- **`agent_space/Dockerfile` pins `golang:1.23-alpine` but `go.mod` says `go 1.26.5`.**
  The container build will fail. Fix before attempting any AWS deploy.
- **`docker-compose.yml` is broken**: `depends_on: cockroach` references a service that
  doesn't exist, and it still passes `OPENAI_*` vars that `llm.go` no longer reads.
- **Two synthetic test rows (INC-412, INC-388) are still in the live cluster.** Jude was
  told; it's his call whether to keep them as seed data.
- **IAM user `jude` (account 071954287023) has almost no attached policies** — S3 and
  Lambda both return plain `AccessDenied`. Real policies are needed before deploying.
- **`.env` holds live secrets** (`OPENROUTER_API_KEY`, and a `DATABASE_URL` containing
  the cluster password). Confirmed git-ignored via `*.env`. Redact when printing.
- **Changing embedding dimensions requires recreating the tables** —
  `DROP TABLE langchain_pg_embedding, langchain_pg_collection;`

## Suggested order

1. Smoke-test tool calling on the current model (cheap, de-risks everything else).
2. MCP client + `tools.Tool` adapter.
3. Agent executor route.
4. Fix Dockerfile/compose, then deploy to App Runner for the AWS requirement + demo URL.
5. Add the license, record the video.
