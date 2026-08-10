-- Demo data for the SRE agent: the services, their deploy history, and the
-- telemetry an incident is visible in.
--
--   go run ./cmd/seed
--
-- Every commit below is REAL. The SHAs, dates, authors and subjects are taken
-- from Look-Its-Sky/opentelemetry-demo-auto-sre-test, the repository that
-- scripts/repos.sql maps these services to. That matters: the agent names a
-- commit, and the remediation stage then clones that repository and runs
-- `git show <sha>` against it. A fabricated SHA would resolve to nothing.
--
-- This script DROPs and recreates what it defines. It deliberately does NOT
-- touch `investigations` or `service_repositories`, which are long-lived and
-- created by the server at boot.

DROP TABLE IF EXISTS incident_context;
DROP TABLE IF EXISTS request_latency;
DROP TABLE IF EXISTS deploys;
DROP TABLE IF EXISTS services;

CREATE TABLE services (
    name        STRING PRIMARY KEY,
    owner_team  STRING NOT NULL,
    tier        INT NOT NULL              -- 1 is customer-facing
);

CREATE TABLE deploys (
    id          INT PRIMARY KEY DEFAULT unique_rowid(),
    service     STRING NOT NULL REFERENCES services(name),
    commit_sha  STRING NOT NULL,
    author      STRING NOT NULL,
    summary     STRING NOT NULL,
    deployed_at TIMESTAMPTZ NOT NULL,
    rolled_back BOOL NOT NULL DEFAULT false
);

CREATE TABLE request_latency (
    id          INT PRIMARY KEY DEFAULT unique_rowid(),
    service     STRING NOT NULL REFERENCES services(name),
    endpoint    STRING NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    p50_ms      INT NOT NULL,
    p99_ms      INT NOT NULL,
    error_rate  DECIMAL(5,4) NOT NULL
);

-- Written by the static-log-analysis producer. An SQS assignment carries only
-- identifiers, so the worker reads the prose back from here, keyed by
-- (incident_id, context_version) — the producer revises context in place, and a
-- message names the version it was raised against.
CREATE TABLE incident_context (
    incident_id     STRING NOT NULL,
    context_version INT NOT NULL,
    service_id      STRING NOT NULL,      -- deliberately no FK: the producer
                                          -- names services we may not know
    environment     STRING NOT NULL,
    severity        STRING NOT NULL,
    summary         STRING NOT NULL,
    log_excerpt     STRING NOT NULL,
    detected_at     TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (incident_id, context_version)
);

INSERT INTO services (name, owner_team, tier) VALUES
    ('checkout', 'payments',  1),
    ('cart',     'growth',    1),
    ('payment',  'payments',  1),
    ('search',   'discovery', 2);

-- Real commits, in the order they landed. The two culprits:
--
--   0c6f0ae  checkout, 2026-07-20 07:55Z — plumbs a KAFKA_TOPIC variable
--            through checkout, accounting and fraud-detection. A topic that
--            stops matching is how order publication fails silently.
--   207d6ef  payment,  2026-07-23 20:24Z — changes the nanos divisor used for
--            the demo.payment.amount attribute in src/payment/index.js.
--
-- The dependabot bumps around them are not filler: they are the noise the agent
-- has to rule out, and it names the wrong commit if it just picks the newest.
INSERT INTO deploys (service, commit_sha, author, summary, deployed_at, rolled_back) VALUES
    ('checkout', '0c6f0ae', 'Ryan Faircloth',   'Add KAFKA_TOPIC environment variable (#3665)',                                   '2026-07-20 07:55:57+00', false),
    ('checkout', 'a74eec8', 'dependabot[bot]',  'build(deps): bump distroless/static-debian12 (#3727)',                           '2026-07-21 08:57:04+00', false),
    ('checkout', '92df8a2', 'Bhuvan Somisetty', 'fix(checkout): correct nanos divisor for shipping and order amount (#3744)',     '2026-07-22 12:07:49+00', false),
    ('checkout', '462edda', 'dependabot[bot]',  'chore(deps): bump the src-checkout group across 1 directory (#3793)',            '2026-08-04 08:13:41+00', false),
    ('checkout', '6c4b663', 'Piotr Kielkowicz', 'chore(checkout): ignore inapplicable OpenPGP advisory (#3821)',                  '2026-08-10 11:54:11+00', false),
    ('payment',  '1f2e2cf', 'Cijo Thomas',      'fix(payment): Set transaction counter unit (#3716)',                             '2026-07-20 06:13:49+00', false),
    ('payment',  '207d6ef', 'Bhuvan Somisetty', 'fix(payment): correct nanos handling for demo.payment.amount (#3750)',           '2026-07-23 20:24:46+00', false),
    ('payment',  '608ea54', 'dependabot[bot]',  'Bump the src-payment group with 1 update (#3774)',                               '2026-07-28 08:12:54+00', false),
    ('payment',  'a6af09e', 'dependabot[bot]',  'chore(deps): bump @openfeature/server-sdk (#3792)',                              '2026-08-04 08:13:15+00', false),
    ('cart',     '0db33b4', 'Bhuvan Somisetty', 'Make cartFailure rate configurable instead of a fixed toggle (#3625)',           '2026-07-06 15:03:29+00', false),
    ('cart',     'ed6f130', 'Piotr Kielkowicz', 'chore(accounting,cart): Bump .NET packages (#3806)',                             '2026-08-04 10:15:20+00', false),
    ('cart',     '1a9412e', 'Cijo Thomas',      'feat(cart): report status to OpAMP server (#3656)',                              '2026-08-04 13:27:50+00', false);

-- checkout: healthy until 0c6f0ae lands at 07:55 on the 20th, then order
-- placement starts failing. Latency barely moves — the orders that fail, fail
-- fast — so a decision made on p99 alone would miss this entirely.
INSERT INTO request_latency (service, endpoint, observed_at, p50_ms, p99_ms, error_rate) VALUES
    ('checkout', '/api/checkout', '2026-07-20 06:30:00+00',  44,   318, 0.0012),
    ('checkout', '/api/checkout', '2026-07-20 07:00:00+00',  46,   325, 0.0010),
    ('checkout', '/api/checkout', '2026-07-20 07:30:00+00',  45,   311, 0.0013),
    ('checkout', '/api/checkout', '2026-07-20 08:00:00+00',  51,   402, 0.1870),
    ('checkout', '/api/checkout', '2026-07-20 08:30:00+00',  49,   388, 0.3140),
    ('checkout', '/api/checkout', '2026-07-20 09:00:00+00',  52,   410, 0.3660),
    -- and the neighbours, unaffected, so "everything is broken" is ruled out
    ('cart',     '/api/cart',     '2026-07-20 08:30:00+00',  38,   210, 0.0008),
    ('search',   '/api/search',   '2026-07-20 08:30:00+00',  96,   540, 0.0021);

-- payment: 207d6ef lands at 20:24 on the 23rd. The amount attribute goes wrong
-- rather than the service going down, so errors climb where downstream
-- validation rejects the malformed value.
INSERT INTO request_latency (service, endpoint, observed_at, p50_ms, p99_ms, error_rate) VALUES
    ('payment',  '/api/charge',   '2026-07-23 19:30:00+00',  61,   402, 0.0014),
    ('payment',  '/api/charge',   '2026-07-23 20:00:00+00',  58,   395, 0.0012),
    ('payment',  '/api/charge',   '2026-07-23 20:30:00+00',  63,   414, 0.0910),
    ('payment',  '/api/charge',   '2026-07-23 21:00:00+00',  60,   408, 0.1240),
    ('payment',  '/api/charge',   '2026-07-23 21:30:00+00',  62,   399, 0.1310);

-- The two incidents the demo enqueues. checkout is the one to show: it is Go,
-- it builds in seconds, and it is the only service here with a test suite.
INSERT INTO incident_context
    (incident_id, context_version, service_id, environment, severity, summary, log_excerpt, detected_at) VALUES
    ('3f8a1c94d2b7e6053a4c81fd9e27b6cae05d413f8a92c7be14d0f6a35c8e29b7', 1,
     'checkout', 'production', 'error',
     'Order placement on /api/checkout began failing 5 minutes after the 07:55 deploy. The error rate went from 0.13% to 36.6% in 90 minutes while p50 and p99 latency stayed flat, so requests are being rejected rather than timing out. Orders are not reaching the downstream accounting and fraud-detection consumers.',
     '2026-07-20T08:00:41Z ERROR checkout.PlaceOrder failed to publish order: kafka: topic not present in metadata after 60000ms' || chr(10) ||
     '2026-07-20T08:00:41Z WARN  checkout.kafka producer could not resolve topic from configuration' || chr(10) ||
     '2026-07-20T08:02:15Z ERROR accounting consumer received 0 messages in the last 300s (expected > 0)',
     '2026-07-20 08:00:41+00'),

    -- keeps the incident id the original sample SQS message carried, so
    -- scripts/enqueue.sh -s payment still resolves
    ('6424cd115aa6f4be24f943bd4c10e776dbf4459c267fda69520dcd548831b5ed', 1,
     'payment', 'production', 'error',
     'Charge requests on /api/charge began failing shortly after the 20:24 deploy. The error rate rose from 0.12% to 13.1% with latency unchanged. Failures correlate with the reported charge amount: downstream validation is rejecting amounts whose minor units are wrong by three orders of magnitude.',
     '2026-07-23T20:31:12Z ERROR payment.charge rejected by validator: amount 12.00 does not match expected minor units (1200)' || chr(10) ||
     '2026-07-23T20:31:12Z WARN  payment.charge demo.payment.amount attribute recorded as 12.00 for a 12.99 order' || chr(10) ||
     '2026-07-23T20:33:41Z ERROR payment.charge validator rejected 47 of 312 charges in the last 60s',
     '2026-07-23 20:31:12+00');
