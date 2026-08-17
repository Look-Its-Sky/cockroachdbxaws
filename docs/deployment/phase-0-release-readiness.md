# Phase 0 release readiness

**Recorded:** 2026-08-15

**Release candidate:** `974fa9b136f161e82ea4c797251fd10eda15471f`

**Branch at verification:** `integration`

**Deployment region:** Verified against the private operator configuration

**Terraform workspace:** `account-<aws-account-id>`

## Public repository handling

This is the sanitized release record. Live account and resource IDs, principal
ARNs, IP addresses, hostnames, queue URLs, operator CIDRs, tenant names, and
Terraform state inventory are intentionally omitted. They remain in ignored
operator files or AWS and must be resolved again through authenticated,
read-only commands immediately before a plan or deployment.

## Outcome

The merged release passes every non-live application and deployment validation
gate required before an AWS change. The existing static-analysis deployment is
present and reachable through its intended management boundaries. A read-only
Terraform refresh plan against the correct account workspace reports:

```text
Plan: 0 to add, 1 to change, 0 to destroy.
```

The sole planned change is the analysis instance's stored user-data value for
`SLA_REPOSITORY_REF`, from the former branch name to the pinned merged commit.
Because the instance is configured with `user_data_replace_on_change = false`,
applying that plan does not deploy the new application revision or replace the
host. The operator must still update `SLA_REPOSITORY_REF` on the host through
Session Manager and run `sudo deploy-static-log-analysis` as documented.

No AWS resource was created, changed, restarted, or deleted during this Phase 0
review.

## Local deployment inputs

The ignored operator file
`services/static-log-analysis/infra/aws/terraform.tfvars` is configured with:

| Input | Verified value/state |
|---|---|
| Region | Explicitly configured and validated against the provider |
| Tenant | Configured in the ignored operator file |
| Repository ref | Pinned release SHA above |
| Optional demo | Enabled |
| Demo size | Explicitly configured in the ignored operator file |
| Demo ingress | One operator `/32` |
| Dashboard | Enabled |
| Dashboard hostname | Configured privately; DNS resolution verified |
| Dashboard ingress | Configured privately; scope requires final operator review |
| Explicit application sources | Empty; the enabled demo supplies the payment log group |

The AWS CLI's default region differs from the configured deployment region.
All operator commands must therefore use `--region <deployment-region>`, set
`AWS_REGION` explicitly, or execute through Terraform's configured provider.
Never use the CLI default implicitly for deployment inspection.

The active AWS principal was verified successfully with STS. Its account number
matches the selected Terraform workspace. The principal ARN is intentionally
not copied into this committed document; verify it immediately before every
plan or apply:

```bash
aws sts get-caller-identity
```

## Existing AWS inventory

Read-only AWS inspection confirmed:

- the analysis EC2 instance is running;
- the demo EC2 instance is running;
- both instances report `Online` through Systems Manager;
- the assignment and dead-letter queues exist in the configured region;
- the configured demo log group exists with bounded retention and contains log
  data;
- the dashboard hostname resolves to the analysis host's stable public address;
  and
- the account-specific Terraform state contains the EC2, EIP, IAM, security
  group, SQS, log group, and association resources expected by the module.

This is an inventory result, not proof that every container on either host is
healthy. Host-level service and application smoke checks remain operator work
because they require an SSM session and may lead directly into a deployment or
restart.

## Terraform state finding

The Terraform `default` workspace has an empty local state. The deployed
resources are tracked in the account-specific workspace derived from the
active AWS identity.

This was a serious apply hazard: following the old deployment command sequence
from `default` could have proposed duplicate resources. The deployment guide now
requires selection and verification of the account-specific workspace before a
plan or apply, and the local workspace selector has been set accordingly.

Local state and variable files were restricted to mode `0600`. They remain
local, ignored files. This is adequate only for the current single-operator
handoff. Before a second operator or CI can apply Terraform, migrate the state
to an encrypted remote backend with locking and test access to it from the
deployment principal.

## Validation record

### Static log analysis

| Gate | Result |
|---|---|
| `gofmt -l .` | Pass; no unformatted Go files |
| `go vet ./...` | Pass |
| `go vet -tags=integration ./...` | Pass |
| `go test ./...` | Pass |
| `REQUIRE_DOCKER=1 go test -p 1 -race -tags=integration ./...` | Pass |
| `./scripts/check-generated-proto.sh` | Pass |
| `./scripts/check-production-deployment.sh` | Pass after formatting the ignored `terraform.tfvars` |
| Pinned Terraform `init` and `validate` | Pass |
| Production Compose render | Pass as part of the deployment gate |

The Docker-backed race/integration suite passed with real CockroachDB and
LocalStack containers, serialized as required by the repository gate:

```bash
cd services/static-log-analysis
REQUIRE_DOCKER=1 go test -p 1 -race -tags=integration ./...
```

The run took several minutes; persistence was the longest package. Reserve
exclusive local Docker capacity when repeating it.

### Dashboard

| Gate | Result |
|---|---|
| `npm ci` | Pass; 155 packages audited, no reported vulnerabilities |
| `npm test` | Pass; 3 files and 6 tests |
| `npm run typecheck` | Pass |
| `npm run build` | Pass; production Next.js build completed |

The install emitted npm's notice that install scripts for `better-sqlite3` and
`esbuild` are not yet covered by an `allowScripts` policy. The current build
succeeded, but dependency install-script policy should be made explicit during
release hardening.

### Agent environment

No agent source was changed.

| Gate | Result |
|---|---|
| `gofmt` | Pass |
| `go build ./...` | Pass |
| `go vet ./...` | Pass |
| `go test ./...` | Pass |

An agent API was already listening on `localhost:8080`. The optional live API
smoke was deliberately skipped because it writes to the configured database and
the request did not authorize modifying that environment.

## Changes made during Phase 0

- Added the combined platform deployment plan and linked it from the root
  README.
- Pinned the ignored local Terraform `repository_ref` to the merged release
  SHA.
- Formatted the ignored local Terraform variables file.
- Selected the existing account-specific Terraform workspace locally.
- Restricted local Terraform variables and state files to mode `0600` and
  corrected container-created state-file ownership.
- Added the account-workspace safety step to the deployment guide.

Only documentation changes are tracked by Git. The Terraform variable, state,
workspace selector, dependency, build, and cache artifacts remain ignored local
operator state.

## Remaining Phase 0 decisions

These must be answered before the next apply or service deployment:

- [ ] Confirm the active AWS principal is the intended deployment principal.
- [ ] Confirm the CockroachDB cluster and `static_log_analysis` database are in
  the same physical region as the configured AWS deployment.
- [ ] Confirm the analysis host's stable egress `/32` remains allowlisted in
  CockroachDB Cloud.
- [ ] Decide whether broad public dashboard ingress is intentional for the demo
  or should be narrowed to operator CIDRs.
- [ ] Confirm DNS control and certificate renewal for the dashboard hostname.
- [ ] Name the environment owner and teardown date.
- [ ] Record the expected daily/monthly AWS and CockroachDB cost ceiling.
- [ ] Migrate Terraform state to an encrypted, locked remote backend before a
  shared or CI-driven apply.
- [x] Run the static-analysis Docker integration/race gate.

## Next operator sequence

Do not run `terraform apply` from `default`. From the infrastructure directory:

```bash
cd services/static-log-analysis/infra/aws
export AWS_REGION=<deployment-region>
aws sts get-caller-identity
terraform init
deployment_workspace="account-$(aws sts get-caller-identity --query Account --output text)"
terraform workspace select "$deployment_workspace"
terraform workspace show
terraform fmt -check
terraform validate
terraform plan -out=static-log-analysis.tfplan
```

Review that the plan still contains no add or destroy action. Applying the
current plan records the new desired bootstrap user data but does not run it.
To deploy the merged release after the plan review, use Session Manager and
follow the existing in-place update procedure:

```bash
sudoedit /etc/static-log-analysis.env
# Set SLA_REPOSITORY_REF to the release SHA recorded at the top of this file.
sudo deploy-static-log-analysis
sudo systemctl status static-log-analysis --no-pager
```

Then run the health, metrics, Compose, SQS, and dashboard smoke checks in
`docs/static-log-analysis/deployment.md`. Stop before agent integration: the
database schema and incident-context blockers in the platform plan remain
unresolved.
