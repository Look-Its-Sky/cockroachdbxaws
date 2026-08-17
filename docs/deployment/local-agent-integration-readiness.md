# Local agent integration readiness

**Recorded:** 2026-08-16

**Status:** Detector-to-SQS-to-context-to-local-model verdict path passed;
automatic remediation intentionally not exercised

This is a public-safe record. It contains no live cloud identifiers, secret
values, raw context payloads, or private repository contents.

## What was deployed

- CockroachDB 26.2 on the existing local static-analysis volume;
- static-analysis migration, combined OTLP processing, and outbox workers;
- LocalStack SQS and the assignment queue;
- the OTLP Collector;
- a separate `agent_space` logical database;
- an `agent_context_reader` role limited to the safe context tables;
- the local CockroachDB MCP server with write queries disabled; and
- a built `agent-space:local-integration` image bound by configuration to one
  region, tenant, classification, and queue;
- GPU-backed Ollama with `qwen2.5-coder:14b-instruct` and
  `nomic-embed-text`; and
- the Linux host-network agent overlay, bound entirely to loopback.

## Findings and fixes

1. The retained CockroachDB store was last used by version 25.3 and could not
   start directly on 26.2. A read-only archive was created in a temporary local
   directory, the store was migrated through 25.4, cluster version 25.4 was
   verified, and the repository-pinned 26.2 image then started successfully.
2. The retained analysis journal was approximately 618 MB and replay consumed
   one CPU while memory climbed beyond 7 GB without reaching readiness during
   the smoke window. It was preserved. Local Compose now permits selecting a
   fresh named journal volume for bounded smoke deployments.
3. Port 8080 was already owned by the OpenTelemetry demo. The local agent
   integration binds only `127.0.0.1:18081` by default.
4. The original agent image used mutable base tags. Build and runtime bases are
   now pinned by digest.
5. The agent context connection initially used the local root user. The
   integration bootstrap now creates a dedicated reader and grants only
   connectivity plus `SELECT` on `investigations`, `incident_families`, and
   `investigation_contexts`. A write attempt was denied as required.
6. Docker resolved `host.docker.internal` correctly, but the host firewall
   dropped bridge-to-host traffic. The loopback-only `compose.local-models.yaml`
   overlay now provides a tested Linux path without changing Docker or opening
   the model server to the LAN.
7. The first model attempt proved the inference client's 30-second deadline can
   expire during cold model load. The runbook warms both endpoints before the
   worker starts.

## Evidence obtained

- Static-analysis readiness: passed.
- Outbox readiness: passed.
- CockroachDB migration: passed.
- LocalStack queue health: passed.
- Collector startup: passed.
- Five allowed `payment/production` errors through the repository smoke script:
  accepted.
- Durable static-analysis context: present.
- Assignment queue depth after election: one available message.
- Live `AnalysisResolver` using the dedicated context-reader DSN: passed.
- Agent image build: passed.
- Local embedding API: passed at 768 dimensions.
- Local chat API: passed with the 14B coding model fully GPU-resident.
- Strict SQS admission, live safe-context read, and local inference: passed.
- Durable agent result: `done`, one iteration, inferred `ROLLBACK`, zero error
  bytes.
- Queue after acknowledgement: zero visible and zero in flight.
- Remediation: not run. It is forced off, no repository is mapped, and a
  rollback is not a code-change candidate.
- Read-only remediation API while execution is off: passed. Repository and
  remediation lists return bounded empty results, the rollback investigation
  has no execution record, and all remediation mutation routes remain gated.
- Dashboard semantics: the verdict renders as `Recommend rollback` and the
  execution state as `Not run — recommendation only`.
- Durable live progress: passed with a new synthetic `shipping/production`
  five-in-five assignment. The journal recorded context start/completion, local
  model start/completion, and terminal verdict in one iteration. All five safe
  categorical events survived an agent-container restart.

## Repeat the model-backed run

Follow `docs/deployment/local-model-hosting.md`. On the tested Linux host, the
final agent command is:

```bash
docker compose --env-file .env.agent-local \
  -f compose.local-models.yaml up -d --build
```

Generate a new assignment through the detector, poll the authenticated
investigation route, verify a durable terminal verdict, and confirm queue depth
returns to zero. Remediation remains intentionally disabled.
