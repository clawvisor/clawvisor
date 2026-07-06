-- 06a: instance-scoped (local) governance tables.
--
-- The OSS build implements the four governance callbacks
-- (CheckModelPolicy / CheckSpendCap / ScanContentPolicy / RecordViolation)
-- locally against these tables — one flat, instance-wide policy set (no
-- org/team dimension). The cloud build keeps its own org_* tables and
-- these are dormant there.
--
-- Shapes mirror cloud's 010_governance_policies.sql exactly EXCEPT there
-- is no org_id column anywhere: policies are instance-scoped, and the
-- violation log drops org_id (the "local" OrgID sentinel the pipeline
-- carries is never persisted). Model/task policies are append-only with
-- an `active` flag so SOC2-style point-in-time history works via
-- created_at (PUT inserts a new row and demotes the prior active row).

-- Singleton allow/deny list of canonical model identifiers. Models are
-- provider-qualified strings (e.g. "anthropic/claude-3-5-sonnet",
-- "openai/gpt-4o") matched exact-string against the gateway's resolved
-- canonical model. Bare names are rejected at PUT (see the handler).
CREATE TABLE IF NOT EXISTS instance_model_policy (
    id          TEXT PRIMARY KEY,
    mode        TEXT NOT NULL CHECK (mode IN ('allow','deny')),
    models      TEXT NOT NULL,          -- JSON array of canonical ids
    active      INTEGER NOT NULL DEFAULT 1,
    created_by  TEXT NOT NULL REFERENCES users(id),
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_instance_model_policy_active
    ON instance_model_policy(active) WHERE active = 1;

-- Per-window spend cap. window_kind ∈ daily | monthly, enforcement ∈
-- soft | hard. cap_micros is int64 micro-USD to match
-- llm_request_cost.cost_micros. One cap per window.
CREATE TABLE IF NOT EXISTS instance_spend_cap (
    id           TEXT PRIMARY KEY,
    window_kind  TEXT NOT NULL CHECK (window_kind IN ('daily','monthly')),
    cap_micros   INTEGER NOT NULL CHECK (cap_micros > 0),
    enforcement  TEXT NOT NULL DEFAULT 'soft' CHECK (enforcement IN ('soft','hard')),
    created_by   TEXT NOT NULL REFERENCES users(id),
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(window_kind)
);

-- Content-pattern policy. block rejects the request (with the admin-
-- authored block_message); flag allows it but records a violation row.
-- enabled supports temporary triage without deletion.
CREATE TABLE IF NOT EXISTS instance_content_policy (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    pattern       TEXT NOT NULL,
    pattern_kind  TEXT NOT NULL CHECK (pattern_kind IN ('regex','keyword')),
    action        TEXT NOT NULL CHECK (action IN ('block','flag')),
    block_message TEXT NOT NULL DEFAULT '',
    enabled       INTEGER NOT NULL DEFAULT 1,
    created_by    TEXT NOT NULL REFERENCES users(id),
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Singleton natural-language task-creation guidance. Fed into the intent
-- verifier's SYSTEM prompt (not the user message) org/instance-wide.
CREATE TABLE IF NOT EXISTS instance_task_policy (
    id          TEXT PRIMARY KEY,
    guidance    TEXT NOT NULL,
    active      INTEGER NOT NULL DEFAULT 1,
    created_by  TEXT NOT NULL REFERENCES users(id),
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_instance_task_policy_active
    ON instance_task_policy(active) WHERE active = 1;

-- Append-only log of blocked / flagged / warned requests. policy_id is a
-- soft link (no FK across the four policy tables). No org_id column: the
-- "local" sentinel is not persisted.
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
    created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_instance_violation_time
    ON instance_policy_violation(created_at);
