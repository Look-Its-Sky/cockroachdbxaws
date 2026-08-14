-- Wires the planted regression on the `demo-bug` branch into the demo, so a
-- queue-driven run reaches triage with a fault that is genuinely present.
--
--   go run ./cmd/seed -file scripts/demo-bug.sql
--   ./scripts/enqueue.sh -v 2
--
-- APPLY THIS ONLY AFTER `demo-bug` IS PUSHED. Until then triage clones the
-- branch, fails to find it, and correctly reports commit_missing.
--
-- The bug is scripts/demo-bug.patch, commit 51e85e6, one line removed from
-- money.Sum: the units carry. Every order whose line-item nanos sum past a
-- whole unit is undercharged by exactly 1.00. money_test.go catches it in two
-- cases, which is the point — checkout is the only mapped service where a
-- candidate can be verified rather than merely compiled.
--
-- Safe to re-run: the deploy is deleted before it is inserted, the context is
-- an UPSERT, and the branch flip is idempotent.
--
-- To undo everything:
--   DELETE FROM deploys WHERE commit_sha = '51e85e6';
--   DELETE FROM incident_context WHERE incident_id = '3f8a1c94d2b7e6053a4c81fd9e27b6cae05d413f8a92c7be14d0f6a35c8e29b7' AND context_version = 2;
--   go run ./cmd/seed -file scripts/repos.sql   -- puts checkout back on main
--   git push origin --delete demo-bug

-- The deploy the investigation has to implicate. Seven characters, matching
-- every other row in this table and what the agent will copy into its verdict.
DELETE FROM deploys WHERE service = 'checkout' AND commit_sha = '51e85e6';

INSERT INTO deploys (service, commit_sha, author, summary, deployed_at, rolled_back)
VALUES ('checkout', '51e85e6', 'SRE Agent Demo',
        'perf(checkout): drop redundant int64 conversion in money.Sum',
        '2026-08-12 09:15:00+00', false);

-- A second context version on the existing checkout incident rather than a new
-- incident id, so scripts/enqueue.sh drives it with -v 2 and nothing else has
-- to learn a new identifier.
--
-- Describes the symptom only. Naming the function would hand the investigation
-- its answer, and the whole point is that it finds the commit itself.
UPSERT INTO incident_context
    (incident_id, context_version, service_id, environment, severity, summary, log_excerpt, detected_at)
VALUES
    ('3f8a1c94d2b7e6053a4c81fd9e27b6cae05d413f8a92c7be14d0f6a35c8e29b7', 2,
     'checkout', 'production', 'error',
     'Finance reconciliation flagged 412 orders since 09:20 UTC where the amount charged is exactly 1.00 below the sum of the line items. The shortfall is always one whole currency unit, never a fraction, and only affects orders whose line-item cents add past a whole unit. Orders that settle under a whole unit of cents are charged correctly. Revenue impact is accruing and the checkout deploy window this morning is the only change in the blast radius.',
     'WARN  checkout.order  total mismatch order=8f21c4 expected=42.30 charged=41.30 delta=-1.00
WARN  checkout.order  total mismatch order=91ab07 expected=18.05 charged=17.05 delta=-1.00
INFO  checkout.money  Sum(units=4 nanos=1100000000) -> units=4 nanos=100000000
ERROR finance.reconcile  412 orders under-collected since 09:20Z, total -412.00',
     '2026-08-12 09:47:00+00');

-- Point checkout at the branch carrying the bug. main stays clean.
UPDATE service_repositories SET default_branch = 'demo-bug', updated_at = now()
WHERE service_id = 'checkout';
