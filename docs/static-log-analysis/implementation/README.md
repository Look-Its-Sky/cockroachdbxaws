# Normative Implementation Contracts

These documents turn the approved architecture into testable implementation
behavior. The words MUST, MUST NOT, SHOULD, and MAY are normative.

- [Identity, trust, and admission](identity-and-admission.md)
- [Time windows and lifecycle boundaries](time-and-windows.md)
- [Journal contract](journal.md)
- [CockroachDB schema and transactions](database.md)
- [Versioned API and data schemas](schemas.md)
- [Runtime, roles, and OTLP ingress](runtime.md)
- [CloudWatch Logs source adapter](cloudwatch-source.md)
- [Milestone 5 vertical slice](vertical-slice.md)
- [TDD implementation plan](tdd-plan.md)
- [Test harness](test-harness.md)

## Precedence

1. Security and regional-boundary requirements.
2. These normative implementation contracts.
3. Architecture Decision Records.
4. Architectural overview documents.

Conflicts MUST be resolved in documentation and tests before implementation is
merged. Production measurements may change capacity values but not correctness
contracts without review.

## Compatibility rule

Persisted structures, queue messages, and agent contracts carry a major/minor
schema version. Readers MUST accept unknown fields within the same major version.
Writers MUST NOT change the meaning or type of an existing field. Breaking changes
require a new major version, dual-read migration, and rollback plan.
