-- Stage 3 M1: policy engine — per-bridge policy document, immutable
-- history, violation feed, bans, and judge-decision audit log.
-- See docs/design-proxy-stage3.md §3.

CREATE TABLE policies (
    id             TEXT PRIMARY KEY,
    bridge_id      TEXT NOT NULL UNIQUE REFERENCES bridge_tokens(id) ON DELETE CASCADE,
    version        INTEGER NOT NULL DEFAULT 1,
    yaml           TEXT NOT NULL,
    compiled_json  TEXT NOT NULL DEFAULT '',
    enabled        INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at     TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_policies_bridge ON policies(bridge_id);

CREATE TABLE policy_history (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    policy_id       TEXT NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
    bridge_id       TEXT NOT NULL,
    version         INTEGER NOT NULL,
    yaml            TEXT NOT NULL,
    author_user_id  TEXT NOT NULL DEFAULT '',
    comment         TEXT NOT NULL DEFAULT '',
    changed_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_policy_history_policy ON policy_history(policy_id, version DESC);

CREATE TABLE policy_violations (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                TEXT NOT NULL DEFAULT (datetime('now')),
    bridge_id         TEXT NOT NULL,
    agent_token_id    TEXT NOT NULL DEFAULT '',
    rule_name         TEXT NOT NULL,
    action            TEXT NOT NULL,                -- 'block' | 'flag'
    request_id        TEXT NOT NULL DEFAULT '',
    destination_host  TEXT NOT NULL DEFAULT '',
    destination_path  TEXT NOT NULL DEFAULT '',
    method            TEXT NOT NULL DEFAULT '',
    message           TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_policy_violations_bridge_ts ON policy_violations(bridge_id, ts DESC);
CREATE INDEX idx_policy_violations_agent_rule ON policy_violations(agent_token_id, rule_name, ts DESC);

CREATE TABLE agent_bans (
    id                TEXT PRIMARY KEY,
    bridge_id         TEXT NOT NULL,
    agent_token_id    TEXT NOT NULL,
    rule_name         TEXT NOT NULL DEFAULT '',
    banned_at         TEXT NOT NULL DEFAULT (datetime('now')),
    expires_at        TEXT NOT NULL,
    violation_count   INTEGER NOT NULL DEFAULT 0,
    lifted_at         TEXT,
    lifted_by         TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_agent_bans_bridge_active ON agent_bans(bridge_id, lifted_at, expires_at);
CREATE INDEX idx_agent_bans_agent ON agent_bans(agent_token_id, rule_name);

CREATE TABLE judge_decisions (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                TEXT NOT NULL DEFAULT (datetime('now')),
    bridge_id         TEXT NOT NULL,
    agent_token_id    TEXT NOT NULL DEFAULT '',
    rule_name         TEXT NOT NULL,
    cache_key         TEXT NOT NULL,
    decision          TEXT NOT NULL,                -- 'allow' | 'block' | 'flag_for_human_review'
    reason            TEXT NOT NULL DEFAULT '',
    model             TEXT NOT NULL DEFAULT '',
    latency_ms        INTEGER NOT NULL DEFAULT 0,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_judge_decisions_bridge_ts ON judge_decisions(bridge_id, ts DESC);
CREATE INDEX idx_judge_decisions_cache ON judge_decisions(cache_key, ts DESC);
