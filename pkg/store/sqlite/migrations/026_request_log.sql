CREATE TABLE gateway_request_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    audit_id    TEXT NOT NULL,
    request_id  TEXT NOT NULL,
    agent_id    TEXT NOT NULL,
    user_id     TEXT NOT NULL,
    service     TEXT NOT NULL,
    action      TEXT NOT NULL,
    task_id     TEXT NOT NULL DEFAULT '',
    reason      TEXT NOT NULL DEFAULT '',
    decision    TEXT NOT NULL DEFAULT '',
    outcome     TEXT NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_request_log_user_time ON gateway_request_log(user_id, created_at DESC);
CREATE INDEX idx_request_log_audit_id  ON gateway_request_log(audit_id);
