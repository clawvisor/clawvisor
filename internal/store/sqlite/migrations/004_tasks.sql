-- Task-scoped authorization
CREATE TABLE tasks (
    id                  TEXT PRIMARY KEY,
    user_id             TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id            TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    purpose             TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'pending_approval',
    authorized_actions  TEXT NOT NULL DEFAULT '[]',
    callback_url        TEXT,
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    approved_at         TEXT,
    expires_at          TEXT,
    expires_in_seconds  INTEGER NOT NULL DEFAULT 1800,
    request_count       INTEGER NOT NULL DEFAULT 0,
    pending_action      TEXT,
    pending_reason      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_tasks_user_status ON tasks(user_id, status);
CREATE INDEX idx_tasks_agent       ON tasks(agent_id);

ALTER TABLE audit_log ADD COLUMN task_id TEXT;
