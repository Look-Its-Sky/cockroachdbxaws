# Local model hosting for agent integration

**Recorded:** 2026-08-16  
**Status:** GPU-backed local verdict path passed; remediation remains disabled

This public-safe runbook hosts both model roles locally and sends no model
traffic or incident context to AWS or a third-party inference provider. It is
for Linux development and deployment evidence, not an AWS architecture.

## Tested model set

| Role | Ollama model | Resident size | Contract |
|---|---|---:|---|
| investigation chat | `qwen2.5-coder:14b-instruct` | about 9.5 GB | OpenAI-compatible chat and tool calls |
| vector embeddings | `nomic-embed-text` | about 323 MB | 768-dimensional OpenAI-compatible embeddings |

The passing run used an RTX 5070 Ti with 16 GB VRAM. Ollama reported both
models as 100% GPU-resident. A smaller chat model should be selected on a GPU
that cannot retain the 14B model and its context cache.

## One-time model preparation

Install Ollama using its official Linux instructions, then keep its listener on
loopback. From the repository root:

```bash
mkdir -p .local/ollama-models
OLLAMA_HOST=127.0.0.1:11434 \
OLLAMA_MODELS="$PWD/.local/ollama-models" \
OLLAMA_KEEP_ALIVE=24h \
OLLAMA_MAX_LOADED_MODELS=2 ollama serve
```

In another terminal:

```bash
OLLAMA_HOST=http://127.0.0.1:11434 ollama pull qwen2.5-coder:14b-instruct
OLLAMA_HOST=http://127.0.0.1:11434 ollama pull nomic-embed-text
```

`.local/` and `.env.agent-local` are ignored. Do not commit model blobs,
tokens, model request logs, incident payloads, or generated answers.

Verify the two API contracts before starting the worker:

```bash
curl -fsS http://127.0.0.1:11434/v1/embeddings \
  -H 'Content-Type: application/json' \
  -d '{"model":"nomic-embed-text","input":"integration probe"}' \
  | jq '.data[0].embedding | length'

curl -fsS http://127.0.0.1:11434/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen2.5-coder:14b-instruct","messages":[{"role":"user","content":"Reply only: ready"}],"temperature":0}' \
  | jq -r '.choices[0].message.content'
```

The expected values are `768` and `ready`. These probes also warm the models;
that keeps a cold model load outside the inference client's 30-second request
deadline. The explicit keep-alive prevents an idle worker from unloading both
models and paying that cold-start cost on its next assignment.

## Start the local integration

Start the static-analysis deployment as described in
`docs/agent-sqs-deployment.md`, then prepare the ignored provider file:

```bash
cp .env.agent-local.example .env.agent-local
```

Create the separate agent database and start the read-only MCP dependency. MCP
is published on loopback only:

```bash
docker compose --env-file .env.agent-local \
  -f compose.local-integration.yaml up -d agent-db-init agent-mcp
```

On Linux hosts where the firewall blocks bridge containers from reaching a
host listener, use the tested host-network deployment:

```bash
docker compose --env-file .env.agent-local \
  -f compose.local-models.yaml up -d --build
```

This overlay reaches Ollama, CockroachDB, LocalStack, and MCP only through
`127.0.0.1`. It forces `REMEDIATION_ENABLED=false`, offers only read tools, and
does not mount `/var/run/docker.sock`. The API remains on
`http://127.0.0.1:18081`.

Where Docker-to-host gateway traffic is known to work, the portable bridge
deployment remains available:

```bash
docker compose --env-file .env.agent-local \
  -f compose.local-integration.yaml up -d agent
```

Do not run both agent services simultaneously; they consume the same queue.

## Verification

```bash
curl -fsS http://127.0.0.1:18081/ping
docker compose -f compose.local-models.yaml logs --no-color agent
OLLAMA_HOST=http://127.0.0.1:11434 ollama ps
```

The successful evidence run admitted an exact stored assignment from local
SQS, fetched its immutable safe context through the distinct read-only analysis
connection, made local embedding and chat calls, and persisted a terminal
`ROLLBACK` verdict in one iteration. The assignment queue returned to zero.
The stored error length was zero.

A later progress-contract smoke sent a fresh synthetic `shipping/production`
cohort through the same path. The durable API exposed five content-free events
(context start/completion, model start/completion, verdict completion), and the
same five events were present after restarting the agent container. No prompt,
reasoning, answer, tool payload, or incident context was printed as evidence.

That proves local investigation, not automatic repair. The evidence assignment
produced `ROLLBACK`, which is intentionally not a code-change candidate, and
this deployment has no repository mapping or remediation authority. A future
fix test needs a reviewed disposable repository fixture, a deterministic
`HOTFIX` incident, a sandbox with Docker access, and an explicit assertion that
no remote branch or pull request can be created.

## Stop

```bash
docker compose -f compose.local-models.yaml down
docker compose -f compose.local-integration.yaml stop agent-mcp
```

Stop the foreground Ollama process with `Ctrl-C`. The static-analysis stack and
its preserved volumes are independent and are not removed by these commands.
