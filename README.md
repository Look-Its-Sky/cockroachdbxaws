# agent_space — an SRE agent that decides rollback vs. hotfix

When production breaks, the expensive question is not *what* broke but *what do we do in the
next ten minutes*. `agent_space` answers it: given an incident description, it recalls similar
past incidents, inspects the live database, names the commit that most likely caused the failure,
and commits to **ROLLBACK** or **HOTFIX**.

The judgement it encodes is deliberate and stated in the prompt, not left to the model's priors:

- **ROLLBACK** when a correct fix cannot land in time to stop the bleeding.
- **HOTFIX** when a fix can land the same day.

CockroachDB is what makes this more than a chat wrapper. Past incident→resolution pairs live in a
vector index, so a new incident is grounded in what actually happened last time; and the agent can
query the live cluster mid-decision, so it reasons about the present rather than only the past.

## Architecture

```
                POST /agent  {"question": "checkout 500s after this morning's deploy"}
                      │
                      ▼
        ┌─────────────────────────────┐
        │  agent.Runner (agent/)      │
        └─────────────┬───────────────┘
                      │
      ┌───────────────┴────────────────┐
      │ 1. recall past incidents       │──► CockroachDB tool #1
      │    utils/crdbvector            │    pgvector-compatible vector index
      │    (similarity search)         │    over the live Cloud cluster
      └───────────────┬────────────────┘
                      │
      ┌───────────────┴────────────────┐
      │ 2. native tool-calling loop    │──► OpenRouter (chat + embeddings)
      │    real JSON Schemas → model   │
      └───────────────┬────────────────┘
                      │
      ┌───────────────┴────────────────┐
      │ 3. inspect the live cluster    │──► CockroachDB tool #2
      │    utils/mcp                   │    Cloud Managed MCP Server
      │    (select_query, list_tables, │    https://cockroachlabs.cloud/mcp
      │     get_table_schema, …)       │
      └───────────────┬────────────────┘
                      │
                      ▼
       {"answer": "ROLLBACK. The implicated commit is a3f9c21…",
        "sources": 2, "iterations": 3, "trace": [ …every tool call… ]}
```

Deployed on **AWS App Runner** from an image in **Amazon ECR**.

### The two CockroachDB integrations

| # | Integration | Where | What it does |
|---|---|---|---|
| 1 | **Distributed vector indexing** | `agent_space/utils/crdbvector/` | A hand-written pgvector-compatible vector store for CockroachDB. Holds the semantic cache of past incident→resolution pairs. |
| 2 | **Cloud Managed MCP Server** | `agent_space/utils/mcp/` | A Model Context Protocol client against `https://cockroachlabs.cloud/mcp`, exposing the cluster's read tools to the agent mid-decision. |

## API

| Route | Purpose |
|---|---|
| `GET /ping` | Liveness. Depends on nothing, so it is the container healthcheck. |
| `POST /store` | `{"text": "…"}` — embed an incident and write it to the vector index. |
| `POST /retrieve` | `{"query": "…", "limit": 2}` — similarity search over past incidents. |
| `POST /ask` | `{"question": "…", "limit": 4}` — one-shot retrieve→generate. No tools. |
| `GET /tools` | The MCP tools discovered at boot, with their JSON Schemas. Proof the MCP handshake is live. |
| `POST /agent` | `{"question": "…", "limit": 4}` — the full loop: recall, query the cluster, decide. Returns the answer **and the trace of every tool call**. |

`/store`, `/retrieve` and `/ask` never depend on MCP. If the MCP handshake fails at boot the
service still starts, logs why, and `/tools` and `/agent` return `503` with an actionable message.
A session that drops *mid-run* is the same class of failure, so it gets the same `503` rather than a
generic `500` — with the partial trace attached, since that is the evidence of how far the
investigation got before the cluster went away.

### Why a step failed

Every failed step in the trace carries a `cause`, because `failed: true` on its own cannot tell
"the model guessed at a table name" from "the MCP server fell over" — and those call for opposite
responses:

| `cause` | Meaning | The run |
|---|---|---|
| `tool_error` | The server ran the tool and rejected the call — refused query, missing table. | continues; the model reads the rejection and corrects itself |
| `rejected` | The server refused to run the tool at all — a blocked schema, an argument it will not take. Arrives as a protocol error rather than a result, but it is a verdict on one call. | continues |
| `invalid_arguments` | Arguments did not match the tool's schema, so nothing was sent. | continues; the error quotes the expected schema |
| `unknown_tool` | The model named a tool that was never offered. | continues; the message lists what is available |
| `malformed_call` | A tool call carrying no function at all. | continues |
| `transport` | The call failed before the tool ran: dropped session, protocol-level rejection, cancelled context — and, most often in practice, a credential that is not authorised for the cluster. | **aborts** |

The line between `rejected` and `transport` is load-bearing and easy to get wrong: the Cloud server
answers a blocked query with a JSON-RPC error, not a result, so at the call site it is
indistinguishable from a dropped session unless you look. Treating every returned error as fatal
killed a live run three good tool calls in. The client therefore tests for the failures known to be
unrecoverable — the SDK's `ErrConnectionClosed` and `ErrSessionMissing`, a cancelled context, a
timeout — and treats everything else as the server's verdict on one call.

Only `transport` aborts, and that asymmetry is the point. The first four are the agent working as
intended — a wrong guess it can recover from. A dropped session cannot be recovered from by trying
again, and each retry costs a full model round trip, so retrying one six times spends real money to
arrive at a worse answer. It used to do exactly that, and then reported "the investigation exceeded
its step budget", blaming the model for an outage.

## Setup

Requires Go 1.26.5+. Copy [`.env.example`](.env.example) to `.env` at the repository root (it is
git-ignored) and fill it in. Then:

```sh
cd agent_space
go run .
```

Or, against the cloud, from the repository root:

```sh
docker compose up --build
```

## Running locally

Everything runs offline: no CockroachDB Cloud account, no license key, no API-billed model.
[`docker-compose.local.yml`](docker-compose.local.yml) runs the backing services only — the API
runs on the host via `go run .`, and the LLM is whatever OpenAI-compatible server you point
`OPENAI_BASE_URL` at.

```sh
docker compose -f docker-compose.local.yml up -d    # CockroachDB + MCP server
cd agent_space && go run .
```

This is the one to use: it exercises **both** CockroachDB integrations, so `/tools` and `/agent`
behave as they do against the Cloud. The DB Console is on [localhost:8090](http://localhost:8090).

`cockroachdb/cockroach` is the same binary CockroachDB Cloud runs, started as a single insecure
node. It needs no license key for this — verified on a clean v26.2.5 container with
`enterprise.license` empty: `VECTOR(N)`, `CREATE INDEX ... USING hnsw` and the `<=>` operator all
work, and the startup log carries no license or throttling warning.

The self-hosted MCP server refuses a TLS-free database connection, and refuses to serve HTTP
without TLS, unless both are opted into explicitly; the compose file opts in
(`CRDB_MCP_ALLOW_INSECURE_DB`, `CRDB_MCP_ALLOW_INSECURE_HTTP`). Fine on a throwaway local cluster,
never anywhere else. Its bearer token must be at least 16 characters or it exits at boot.

Set these in `.env` for the host-side process:

```sh
DATABASE_URL=postgresql://root@localhost:26257/defaultdb?sslmode=disable
VECTOR_DIMENSIONS=768     # must match your embedding model's width
COCKROACH_MCP_URL=http://localhost:8443
COCKROACH_API_KEY=local-dev-token-insecure
COCKROACH_CLUSTER_ID=     # must be empty: cluster scoping is a Cloud concept
```

Nothing in the compose file reads `DATABASE_URL`, so a value meant for CockroachDB Cloud cannot
accidentally repoint the containers. The boot log states which host it is dialling, and the failure
message lists the expected local URL — `connection refused` here almost always means `.env` still
points at the cloud cluster or at a port nothing is listening on.

### PostgreSQL + pgvector instead

```sh
docker compose -f docker-compose.local.yml up -d postgres
```

Naming the service activates its profile. The vector store works against stock PostgreSQL
unchanged — `crdbvector` emits plain pgvector SQL (`vector(N)`, the `<=>` cosine operator,
`vector_dims()`, `USING hnsw`). The one difference is that `createVectorExtensionIfNotExists` is a
deliberate no-op, because CockroachDB ships the vector type built in and errors if you try to
create it. PostgreSQL does need it, so [`docker/postgres-init/`](docker/postgres-init/) creates it
at initdb time. Point `DATABASE_URL` at
`postgres://postgres:postgres@localhost:5432/agent_space?sslmode=disable`.

**There is no MCP on this path.** The CockroachDB MCP Server reads `crdb_internal` and speaks only
to CockroachDB; it cannot be pointed at PostgreSQL. So `/tools` and `/agent` return 503 and you
lose exactly the half of the system that is hardest to iterate on. Use this only as a portability
check on the vector store.

### Pointing at your own LLM

Any OpenAI-compatible server works, and **one variable names each model wherever it runs**:
`OPENROUTER_MODEL` is sent to a local server unchanged, so switching endpoints does not mean
switching settings.

```sh
OPENAI_BASE_URL=http://localhost:11434/v1     # Ollama
OPENROUTER_MODEL=qwen2.5-coder:32b
```

**`OPENAI_BASE_URL` moves chat only.** Embeddings stay on OpenRouter, because the vector column is
sized to the embedder at `CREATE TABLE`: pointing chat at a local model for cheap iteration must
not silently swap a 1024-wide embedder for a 768-wide one and invalidate every stored vector. Chat
is the expensive half worth running locally — one `/agent` run is ~15k tokens — while embeddings
are ~$0.01 per million tokens, so there is little to gain and a migration to lose.

The API key follows the endpoint, not the setting. `OPENAI_API_KEY` and `LLM_API_KEY` are used for
a self-hosted server if set; **`OPENROUTER_API_KEY` is only ever sent to OpenRouter** — it is a
live billable credential, and forwarding it to an arbitrary process on localhost would leak it
silently. It still reaches the embedding endpoint, which is how embeddings keep working while chat
is local.

The boot log states both resolutions separately, so an endpoint that is not being picked up is
visible immediately:

```
LLM: chat: qwen2.5-coder:32b via self-hosted at http://localhost:11434/v1 |
     embeddings: qwen/qwen3-embedding-8b via OpenRouter at https://openrouter.ai/api/v1
     (request 1024 dims, column 1024)
```

Two sharp edges:

- **Model names are required.** Without `OPENROUTER_MODEL` the literal string `local-model` is
  sent. Servers that serve one loaded model (llama.cpp, LM Studio) ignore the field; Ollama and
  vLLM route by name and will reject it. A warning is logged either way.
- **To go fully offline**, set `EMBEDDING_BASE_URL` as well. Only then is the `dimensions` field
  omitted — most local embedding servers reject it outright — and the column has to be sized with
  `VECTOR_DIMENSIONS` instead (`nomic-embed-text` is 768, `mxbai-embed-large` is 1024). If your
  server does accept the field, set `OPENROUTER_EMBEDDING_DIMENSIONS` and it sizes the column too.

From inside a container, reach a server on your host at `http://host.docker.internal:11434/v1`;
the compose file sets `extra_hosts` so that resolves on Linux too.

Confirm the model you pick actually emits tool calls before building on it — `cmd/toolcheck`
below measures exactly that, and many small local models score badly.

## Resetting the vector store

```sh
./agent_space/scripts/nuke.sh                 # empty the tables, keep the schema
./agent_space/scripts/nuke.sh -mode=drop      # remove them; required after changing the embedding width
./agent_space/scripts/nuke.sh -mode=drop -yes # unattended
```

`nuke.sh` is a wrapper over `cmd/nuke` that works from any directory; `cd agent_space && go run
./cmd/nuke` is equivalent. Flags pass straight through, and the wrapper deliberately adds no `-yes`
of its own.

It prints the target with the password redacted and requires you to type `nuke` unless `-yes` is
passed. It talks to the database directly rather than through `crdbvector`, so it needs no LLM
credentials and still works when the schema is in a state the store refuses to open. Dropped tables
are recreated on the next server start, sized to the configured width.

Pass `-database-url` to target something other than `DATABASE_URL` — worth doing deliberately, since
the default is whatever your `.env` points at, which may be the production cluster.

## Design note: why not langchaingo's agent?

langchaingo ships `agents.NewOpenAIFunctionsAgent`, and the obvious move is to hand it the MCP
tools. It does not work well here, for a specific reason: it declares every tool to the model with
the same hardcoded schema, `{"__arg1": string}` (`agents/openai_functions_agent.go`). MCP tools are
defined by a JSON Schema, so that flattening throws away exactly the information the model needs —
`select_query` and `list_tables` become indistinguishable single-string functions, and the model is
left guessing at argument shapes it was never shown.

`agent/runner.go` runs its own loop over `llms.GenerateContent` + `llms.WithTools` instead, passing
each tool's real `InputSchema` straight through to `llms.FunctionDefinition.Parameters`. The model
sees what the server actually published.

The adapter in `utils/mcp` still implements `tools.Tool`, so the langchaingo path stays available;
it simply is not the default. That path is why `mcp.ParseArgs` exists — it accepts fenced JSON,
double-encoded JSON, and bare text for single-field tools, and when it truly cannot parse the input
it returns an error quoting the expected schema, because an error the model can read is an error
the model can recover from.

## Tool-calling reliability

The whole design rests on the model emitting well-formed tool calls. OpenRouter advertising `tools`
in `supported_parameters` is a claim about the API surface, not about behaviour, so it is measured
directly:

```sh
./agent_space/scripts/toolcheck.sh              # 20 iterations against OPENROUTER_MODEL
./agent_space/scripts/toolcheck.sh -n 40 -v     # more samples, print every iteration
./agent_space/scripts/toolcheck.sh -model qwen/qwen3-coder-next
```

Measured on **2026-08-06** against `z-ai/glm-5.2`, n=20:

| Stage | Passed |
|---|---|
| emitted a tool call | 20/20 |
| used a published tool name | 20/20 |
| arguments parsed | 20/20 |
| required fields present | 20/20 |
| picked the expected tool | 12/20 |

**100% well-formed.** The 12/20 on tool *selection* is not a defect: asked to count rows in a
table, the model called `list_tables` first to confirm the table exists. That is correct multi-step
behaviour, which is why it is reported separately and does not gate the pass rate.

`toolcheck` exits non-zero below `-threshold` (default 0.8), so it can gate CI.

## What the agent is allowed to do

The agent diagnoses; it does not remediate. It is offered only the tools that cannot modify the
cluster. The exact counts depend on which server you are pointed at, because the two publish
different tool sets — measured on both:

```
self-hosted cockroachdb-mcp-server 0.1.0
  discovered 15 tools, offering 10 to the agent
  withholding 5 write tools: create_database, create_table, delete_rows, insert_rows, update_rows

Cloud Managed MCP Server (https://cockroachlabs.cloud/mcp)
  discovered 12 tools, offering 9 to the agent
  withholding 3 write tools: create_database, create_table, insert_rows
```

`delete_rows` and `update_rows` simply do not exist on the Cloud server, which is why its withheld
count is lower rather than its exposure higher. Compare either against live `/tools` output rather
than trusting these numbers.

Separately from safety, two read-only tools are also withheld:

```
withholding 2 introspection tools from the agent: show_running_queries, show_statement
```

Nothing about them is dangerous. They are cluster introspection, which the system prompt already
tells the model to avoid — and telling it was not enough. It reached for `show_statement` anyway,
and against a CockroachDB Cloud Basic cluster that call does not return inside the client's 90s
ceiling, so a run that was moments from an answer died on a tool it had been told not to use.
Removing them makes the instruction structural rather than advisory. `AGENT_EXCLUDE_TOOLS`
overrides the list; setting it empty offers everything.

### The schema is read once, not once per run

At boot the agent describes the cluster's application tables and puts them in the system prompt, so
a run starts already knowing what exists. Without it the model spends most of its budget
rediscovering a schema that never changes — and then runs out:

| | discovering per run | schema preloaded |
|---|---|---|
| iterations | 6, hitting the cap | 3 |
| tool calls | 6 | 2 |
| of which discovery | 4 | 0 |
| `truncated` | `true` | `false` |
| wall clock | 47s | 35s |

The four discovery results were also re-sent on every later iteration, so they cost tokens
repeatedly. This is why `truncated` used to be true on every run: the model was not refusing to
stop, it was never reaching a point worth stopping at.

It is deliberately best-effort. A cluster that cannot be described at boot logs why and leaves the
model to discover the schema itself, exactly as before, so this can never become a new reason for
startup to fail.

The same reasoning applies to `cluster_id`. When `COCKROACH_CLUSTER_ID` is set, the scope travels
as a header and the Cloud server rejects any call that *also* passes the argument — so the property
is stripped from the schema the model is shown. It had been calling `list_clusters`, reading the
real UUID out of the result, and passing it along in good faith.

`/tools` reports the same split (`discovered`, `offered_to_agent`, `withheld_writes`, and a per-tool
`offered_to_agent` flag), so the boundary is inspectable at runtime rather than implied.

The filter reads the server's own `readOnlyHint` annotation, which is authoritative. It is
deliberately **fail-closed**: a tool that declares nothing is treated as a write. That is not
theoretical — on `cockroachdb-mcp-server` 0.1.0, `delete_rows` and `update_rows` declare
`readOnlyHint: false`, but `create_database`, `create_table` and `insert_rows` publish no
annotations at all, so a permissive default would hand the agent exactly the undeclared destructive
tools.

If *no* tool declares the hint, the server publishes no annotations and failing closed would leave
the agent with nothing to call. That case falls back to matching names against known write verbs and
logs that it has done so, because a silent downgrade from "the server told us" to "we guessed" is
worth seeing.

Set `AGENT_ALLOW_WRITE_TOOLS=true` to hand over the full set. There is no reason to do this for the
demo.

## API authentication

`/store`, `/retrieve`, `/ask` and `/agent` sit behind a shared secret; `/ping` and `/tools` stay
open because they are free, read-only and the parts worth demonstrating.

```sh
API_TOKEN=$(openssl rand -hex 24) go run .
curl -X POST localhost:8080/agent -H "X-Agent-Token: $API_TOKEN" -d '{"question":"..."}'
```

`Authorization: Bearer <token>` works too. Comparison is constant-time. `scripts/demo.sh` and
`scripts/bench.sh` pick the token up from `API_TOKEN` automatically.

With `API_TOKEN` unset the middleware is a no-op so local development stays frictionless — but the
server says so at boot, because a public deployment without it means anyone can spend your model
credits through `/agent` and read your stored incidents through `/retrieve`.

## Seeding the cluster for a demo

The agent's whole premise is that it queries the *live* cluster. On an empty cluster it cannot: it
invents plausible table names, every call fails, and it silently falls back to answering from the
vector store alone. The answer still comes out right, which is precisely what makes it a trap.

```sh
./agent_space/scripts/seed.sh          # with a confirmation prompt
./agent_space/scripts/seed.sh -yes     # unattended
```

`seed.sh` wraps `cmd/seed`, which applies the script over the ordinary database connection. It
needs no container runtime and no local cluster, so the same command seeds CockroachDB Cloud — the
older `docker exec … cockroach sql` route only ever worked against the local stack. Statements are
executed one at a time: sent as one batch they land in a single implicit transaction, and
CockroachDB will not drop and recreate a table inside one.

It drops and recreates only `services`, `deploys` and `request_latency`. The vector store's tables
are left alone.

That creates `services`, `deploys` and `request_latency`, with a latency series that degrades
exactly when commit `a91f3c2` ships — so the agent can *derive* the culprit rather than restate what
the vector store already told it. Measured difference on the same question:

| | tool calls | failed | answer grounded in |
|---|---|---|---|
| empty cluster | 8 | 6 | vector store only |
| seeded cluster | 5 | 0 | `deploys` table + vector store |

## Tests

```sh
./agent_space/scripts/test.sh          # gofmt, build, vet, unit tests
./agent_space/scripts/test.sh -r -c    # add the race detector and coverage
./agent_space/scripts/test.sh -a       # also run the live API smoke test
```

`test.sh` is the one to run before committing. It keeps going after a failing stage, so one broken
thing does not hide the rest, and exits non-zero if any stage failed. `cd agent_space && go test
./...` still works if you only want the unit tests.

`-a` additionally runs `test_api.sh` against a running server. That is opt-in rather than automatic
because it **writes one row** into whatever database the API points at, which may be a live cluster.
The row is tagged `SMOKETEST-<timestamp>`; `scripts/nuke.sh` clears it.

The MCP client and the agent loop are both tested against an in-process MCP server
(`utils/mcp/mcptest`) built with the same SDK — real protocol round trips, no cloud credentials, no
network. The agent tests drive the loop with a scripted model, so control flow (history
construction, tool dispatch, argument-error recovery, iteration cap) is verified without spending
tokens. `utils` covers endpoint/key/dimension resolution, including the zero-dimensions case that
self-hosted embedding servers need.

## Deploying to AWS

`scripts/deploy.sh` builds the image, pushes it to ECR, and creates or updates the App Runner
service. It needs the Docker CLI and the AWS CLI, and an IAM principal with ECR push rights
(`AmazonEC2ContainerRegistryPowerUser`), `AWSAppRunnerFullAccess`, and an App Runner ECR access
role.

```sh
AWS_REGION=us-east-1 ./agent_space/scripts/deploy.sh
```

## Demo

`agent_space/scripts/demo.sh` seeds realistic incidents, shows the live MCP tool list, and runs an
end-to-end decision with its trace:

```sh
./agent_space/scripts/demo.sh                       # against localhost:8080
API_URL=https://….awsapprunner.com ./agent_space/scripts/demo.sh
```

## Benchmarking

`agent_space/scripts/bench.sh` times each endpoint and, for `/agent`, splits wall time into MCP
time and model time — the `duration_ms` in the trace makes that decomposition exact, so a slow run
can be attributed rather than guessed at.

```sh
./agent_space/scripts/bench.sh                              # 3 runs each of /retrieve, /ask, /agent
./agent_space/scripts/bench.sh -n 10 -e retrieve            # 10 runs of one endpoint
./agent_space/scripts/bench.sh -l "glm-5.2" -o glm.jsonl    # label and keep the raw records
```

It skips a warmup run by default so model load time is not charged to run 1, drops `/agent` when
MCP is unavailable rather than timing a 503, and needs `jq`. The per-request timeout defaults to
900s: `curl` disconnecting cancels the request context server-side and the handler returns 500, so
a run that fails at exactly `-m` seconds is the client giving up, not the server failing.

## License

MIT — see [LICENSE](LICENSE).
