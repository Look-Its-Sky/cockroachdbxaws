# CockroachDB × AWS Incident Investigation Platform

The platform's first component is the deterministic Static Log Analysis Service:
it turns logs from deployed services into deduplicated, enriched incidents, and
requests an agent investigation when deterministic rules say one is warranted.

```text
docs/        design documents and normative implementation contracts
services/    deployable components, one Go module each
```

- [Static Log Analysis Service documentation](docs/static-log-analysis/README.md)
- [Static Log Analysis Service code](services/static-log-analysis/)
- [Production EKS chart](services/static-log-analysis/chart/static-log-analysis/README.md)
- [Regional AWS infrastructure module](services/static-log-analysis/infra/aws/README.md)
- [Working in this repository](AGENTS.md) — current progress, gates, and conventions

The service is under construction. [AGENTS.md](AGENTS.md) records which
milestones and behaviours exist in code; the rest of the documentation describes
the intended system and its contracts.

## Running the tests

```text
cd services/static-log-analysis
go test ./...                                          # fast gate
./scripts/check-production-deployment.sh               # Helm and Terraform gate
REQUIRE_DOCKER=1 go test -p 1 -race -tags=integration ./...  # storage and concurrency gate
```
