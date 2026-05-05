-- SQLite cannot drop inline UNIQUE constraints, so we recreate the table
-- to change UNIQUE(request_id) → UNIQUE(request_id, user_id).

CREATE TABLE audit_log_new (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id        TEXT REFERENCES agents(id) ON DELETE SET NULL,
    request_id      TEXT NOT NULL,
    task_id         TEXT,
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
    error_msg       TEXT,
    verification    TEXT,
    UNIQUE(request_id, user_id)
);

INSERT INTO audit_log_new SELECT
    id, user_id, agent_id, request_id, task_id, timestamp, service, action,
    params_safe, decision, outcome, policy_id, rule_id,
    safety_flagged, safety_reason, reason, data_origin, context_src,
    duration_ms, filters_applied, error_msg, verification
FROM audit_log;

DROP TABLE audit_log;
ALTER TABLE audit_log_new RENAME TO audit_log;

CREATE INDEX idx_audit_user_time ON audit_log(user_id, timestamp DESC);
CREATE INDEX idx_audit_outcome   ON audit_log(user_id, outcome);
CREATE INDEX idx_audit_service   ON audit_log(user_id, service);
