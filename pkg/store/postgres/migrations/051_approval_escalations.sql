CREATE TABLE IF NOT EXISTS approval_escalations (
    id                 TEXT PRIMARY KEY,
    approval_record_id TEXT NOT NULL REFERENCES approval_records(id) ON DELETE CASCADE,
    user_id            TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    target_type        TEXT NOT NULL,
    target_id          TEXT NOT NULL,
    approval_request   JSONB NOT NULL,
    escalation_chain   JSONB NOT NULL,
    current_step       INTEGER NOT NULL DEFAULT 0,
    next_escalate_at   TIMESTAMPTZ NOT NULL,
    status             TEXT NOT NULL DEFAULT 'active',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_approval_escalations_due
    ON approval_escalations(status, next_escalate_at);

CREATE UNIQUE INDEX IF NOT EXISTS idx_approval_escalations_target
    ON approval_escalations(target_type, target_id);
