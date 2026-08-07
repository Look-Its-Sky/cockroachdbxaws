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

Any OpenAI-compatible server works. **`OPENAI_BASE_URL` is the switch — set it and the local host
wins:**

```sh
OPENAI_BASE_URL=http://localhost:11434/v1     # Ollama
LLM_MODEL=qwen2.5-coder:32b
LLM_EMBEDDING_MODEL=nomic-embed-text
VECTOR_DIMENSIONS=768
```

Precedence is per-provider rather than per-variable, because the usual way to switch is to leave
the old settings in `.env`. With `OPENAI_BASE_URL` set, the `LLM_*` / `OPENAI_*` variables take
precedence and the `OPENROUTER_*` ones are ignored — otherwise a leftover
`OPENROUTER_MODEL=z-ai/glm-5.2` would be sent to Ollama, which answers with a confusing 404.

The API key is optional here, since most local servers ignore it. `OPENAI_API_KEY` and
`LLM_API_KEY` are used if set; **`OPENROUTER_API_KEY` deliberately is not** — it is a live billable
credential, and forwarding it to an arbitrary process on localhost would leak it silently.

The boot log states which provider actually won, so a base URL that is not being picked up is
visible immediately:

```
LLM: self-hosted at http://localhost:11434/v1 | chat: qwen2.5-coder:32b |
     embeddings: nomic-embed-text (request dimensions omitted, column 768)
```

Two sharp edges:

- **Model names are required.** Without `LLM_MODEL` the literal string `local-model` is sent.
  Servers that serve one loaded model (llama.cpp, LM Studio) ignore the field; Ollama and vLLM
  route by name and will reject it. A warning is logged either way.
- **The `dimensions` field is omitted by default when self-hosted**, because most local embedding
  servers reject it outright. The vector column still has to be sized, so set `VECTOR_DIMENSIONS`
  to the model's native width (`nomic-embed-text` is 768, `mxbai-embed-large` is 1024). If your
  server does accept the field, set `LLM_EMBEDDING_DIMENSIONS` and it sizes the column too.

From inside a container, reach a server on your host at `http://host.docker.internal:11434/v1`;
the compose file sets `extra_hosts` so that resolves on Linux too.

Confirm the model you pick actually emits tool calls before building on it — `cmd/toolcheck`
below measures exactly that, and many small local models score badly.

## Resetting the vector store

```sh
cd agent_space
go run ./cmd/nuke                 # empty the tables, keep the schema
go run ./cmd/nuke -mode=drop      # remove them; required after changing the embedding width
go run ./cmd/nuke -mode=drop -yes # unattended
```

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
cd agent_space
go run ./cmd/toolcheck              # 20 iterations against OPENROUTER_MODEL
go run ./cmd/toolcheck -n 40 -v     # more samples, print every iteration
go run ./cmd/toolcheck -model qwen/qwen3-coder-next
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

The agent diagnoses; it does not remediate. Of the 15 tools the MCP server advertises, it is offered
only the 10 that cannot modify the cluster:

```
discovered 15 tools, offering 10 to the agent
withholding 5 write tools: create_database, create_table, delete_rows, insert_rows, update_rows
```

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

## Tests

```sh
cd agent_space
go test ./...
```

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
