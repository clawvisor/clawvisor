CREATE TABLE audit_log (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id        TEXT REFERENCES agents(id) ON DELETE SET NULL,
    request_id      TEXT NOT NULL UNIQUE,
    timestamp       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    service         TEXT NOT NULL,
    action          TEXT NOT NULL,
    params_safe     JSONB NOT NULL DEFAULT '{}',
    decision        TEXT NOT NULL,
    outcome         TEXT NOT NULL,
    policy_id       TEXT,
    rule_id         TEXT,
    safety_flagged  BOOLEAN NOT NULL DEFAULT FALSE,
    safety_reason   TEXT,
    reason          TEXT,
    data_origin     TEXT,
    context_src     TEXT,
    duration_ms     INTEGER NOT NULL DEFAULT 0,
    filters_applied JSONB,
    error_msg       TEXT
);

CREATE INDEX idx_audit_user_time ON audit_log(user_id, timestamp DESC);
CREATE INDEX idx_audit_outcome   ON audit_log(user_id, outcome);
CREATE INDEX idx_audit_service   ON audit_log(user_id, service);
CREATE INDEX idx_audit_origin    ON audit_log(user_id, data_origin);

CREATE TABLE pending_approvals (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    request_id      TEXT NOT NULL UNIQUE,
    audit_id        TEXT NOT NULL,
    request_blob    JSONB NOT NULL,
    callback_url    TEXT,
    telegram_msg_id TEXT,
    expires_at      TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
