CREATE TABLE IF NOT EXISTS approval_escalations (
    id                 TEXT PRIMARY KEY,
    approval_record_id TEXT NOT NULL,
    user_id            TEXT NOT NULL,
    target_type        TEXT NOT NULL,
    target_id          TEXT NOT NULL,
    approval_request   TEXT NOT NULL,
    escalation_chain   TEXT NOT NULL,
    current_step       INTEGER NOT NULL DEFAULT 0,
    next_escalate_at   TEXT NOT NULL,
    status             TEXT NOT NULL DEFAULT 'active',
    created_at         TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at         TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (approval_record_id) REFERENCES approval_records(id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_approval_escalations_due
    ON approval_escalations(status, next_escalate_at);

CREATE UNIQUE INDEX IF NOT EXISTS idx_approval_escalations_target
    ON approval_escalations(target_type, target_id);
