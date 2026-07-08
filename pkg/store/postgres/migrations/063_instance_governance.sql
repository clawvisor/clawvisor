-- 06a: instance-scoped (local) governance tables (Postgres mirror of
-- sqlite 064_instance_governance.sql). See that file's header for the
-- full rationale. TIMESTAMPTZ + BIGINT dialect; no org_id anywhere.

CREATE TABLE IF NOT EXISTS instance_model_policy (
    id          TEXT PRIMARY KEY,
    mode        TEXT NOT NULL CHECK (mode IN ('allow','deny')),
    models      TEXT NOT NULL,          -- JSON array of canonical ids
    active      INTEGER NOT NULL DEFAULT 1,
    created_by  TEXT NOT NULL REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_instance_model_policy_active
    ON instance_model_policy(active) WHERE active = 1;

CREATE TABLE IF NOT EXISTS instance_spend_cap (
    id           TEXT PRIMARY KEY,
    window_kind  TEXT NOT NULL CHECK (window_kind IN ('daily','monthly')),
    cap_micros   BIGINT NOT NULL CHECK (cap_micros > 0),
    enforcement  TEXT NOT NULL DEFAULT 'soft' CHECK (enforcement IN ('soft','hard')),
    created_by   TEXT NOT NULL REFERENCES users(id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(window_kind)
);

CREATE TABLE IF NOT EXISTS instance_content_policy (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    pattern       TEXT NOT NULL,
    pattern_kind  TEXT NOT NULL CHECK (pattern_kind IN ('regex','keyword')),
    action        TEXT NOT NULL CHECK (action IN ('block','flag')),
    block_message TEXT NOT NULL DEFAULT '',
    enabled       INTEGER NOT NULL DEFAULT 1,
    created_by    TEXT NOT NULL REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS instance_task_policy (
    id          TEXT PRIMARY KEY,
    guidance    TEXT NOT NULL,
    active      INTEGER NOT NULL DEFAULT 1,
    created_by  TEXT NOT NULL REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_instance_task_policy_active
    ON instance_task_policy(active) WHERE active = 1;

CREATE TABLE IF NOT EXISTS instance_policy_violation (
    id            TEXT PRIMARY KEY,
    user_id       TEXT,
    agent_id      TEXT,
    task_id       TEXT,
    policy_kind   TEXT NOT NULL CHECK (policy_kind IN
                    ('model_policy','spend_cap','content_policy','task_policy')),
    policy_id     TEXT,
    action_taken  TEXT NOT NULL CHECK (action_taken IN ('blocked','flagged','warned')),
    detail        TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_instance_violation_time
    ON instance_policy_violation(created_at);

-- SumInstanceCostMicros (the spend-cap read path) aggregates
-- SUM(cost_micros) over every llm_request_cost row in a time window,
-- instance-wide (no user_id / agent_id filter). Every existing index on
-- that table is (user_id,…) or (agent_id,…) leading, so this rollup falls
-- back to a full table scan. Add a timestamp-leading index so the window
-- filter seeks directly — same reasoning as migration 057's
-- (agent_id, timestamp) covering index for the cloud dashboards.
CREATE INDEX IF NOT EXISTS idx_llm_cost_ts_raw
    ON llm_request_cost(timestamp);
