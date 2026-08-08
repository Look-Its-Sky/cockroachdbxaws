-- Demo application schema for the SRE agent.
--
--   docker exec -i cockroachdbxaws-cockroach-1 \
--     cockroach sql --insecure -d defaultdb < agent_space/scripts/seed-cluster.sql
--
-- Without this the cluster holds only langchain_pg_embedding and
-- langchain_pg_collection, so the agent has nothing meaningful to query: it
-- invents plausible table names, every call errors, and it falls back to
-- answering from the vector store alone. That still produces the right answer,
-- which is exactly what makes it a trap — the live-cluster half of the system
-- looks like it is working when it is contributing nothing.
--
-- The data is deliberately consistent with the seeded incidents: commit
-- a91f3c2 is the one that regressed checkout latency, and the numbers here
-- corroborate INC-412 rather than merely restating it.

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

INSERT INTO services (name, owner_team, tier) VALUES
    ('checkout', 'payments',  1),
    ('cart',     'growth',    1),
    ('search',   'discovery', 2);

-- a91f3c2 is the culprit: the latency rows below straddle its deploy time.
INSERT INTO deploys (service, commit_sha, author, summary, deployed_at, rolled_back) VALUES
    ('checkout', '4d20b71', 'rhea',  'bump payment SDK to 4.2.1',                  '2026-08-05 09:14:00+00', false),
    ('checkout', 'a91f3c2', 'tomas', 'add synchronous fraud-check to request path','2026-08-06 14:02:00+00', false),
    ('cart',     '77bd10e', 'imani', 'cache cart totals per session',              '2026-08-04 11:47:00+00', true),
    ('search',   'b0c9e55', 'rhea',  'reindex product catalogue nightly',          '2026-08-06 03:30:00+00', false);

-- Healthy before 14:02, badly degraded after. p99 crosses 8s, matching INC-412.
INSERT INTO request_latency (service, endpoint, observed_at, p50_ms, p99_ms, error_rate) VALUES
    ('checkout', '/api/checkout', '2026-08-06 13:00:00+00',  42,   310, 0.0011),
    ('checkout', '/api/checkout', '2026-08-06 13:30:00+00',  45,   328, 0.0009),
    ('checkout', '/api/checkout', '2026-08-06 14:00:00+00',  44,   319, 0.0012),
    ('checkout', '/api/checkout', '2026-08-06 14:30:00+00', 512,  6480, 0.0180),
    ('checkout', '/api/checkout', '2026-08-06 15:00:00+00', 690,  8120, 0.0270),
    ('checkout', '/api/checkout', '2026-08-06 15:30:00+00', 705,  8340, 0.0294),
    ('cart',     '/api/cart',     '2026-08-06 15:00:00+00',  38,   210, 0.0008),
    ('search',   '/api/search',   '2026-08-06 15:00:00+00',  96,   540, 0.0021);
