CREATE TABLE audit_log (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id        TEXT REFERENCES agents(id) ON DELETE SET NULL,
    request_id      TEXT NOT NULL UNIQUE,
    timestamp       TEXT NOT NULL DEFAULT (datetime('now')),
    service         TEXT NOT NULL,
    action          TEXT NOT NULL,
    params_safe     TEXT NOT NULL DEFAULT '{}',
    decision        TEXT NOT NULL,
    outcome         TEXT NOT NULL,
    policy_id       TEXT,
    rule_id         TEXT,
    safety_flagged  INTEGER NOT NULL DEFAULT 0,
    safety_reason   TEXT,
    reason          TEXT,
    data_origin     TEXT,
    context_src     TEXT,
    duration_ms     INTEGER NOT NULL DEFAULT 0,
    filters_applied TEXT,
    error_msg       TEXT
);

CREATE INDEX idx_audit_user_time ON audit_log(user_id, timestamp DESC);
CREATE INDEX idx_audit_outcome   ON audit_log(user_id, outcome);
CREATE INDEX idx_audit_service   ON audit_log(user_id, service);

CREATE TABLE pending_approvals (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    request_id      TEXT NOT NULL UNIQUE,
    audit_id        TEXT NOT NULL,
    request_blob    TEXT NOT NULL,
    callback_url    TEXT,
    telegram_msg_id TEXT,
    expires_at      TEXT NOT NULL,
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
