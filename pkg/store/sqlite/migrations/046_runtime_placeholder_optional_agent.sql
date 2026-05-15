ALTER TABLE runtime_placeholders RENAME TO runtime_placeholders_old;

CREATE TABLE runtime_placeholders (
    placeholder          TEXT PRIMARY KEY,
    user_id              TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id             TEXT REFERENCES agents(id) ON DELETE CASCADE,
    service_id           TEXT NOT NULL,
    created_at           TEXT NOT NULL DEFAULT (datetime('now')),
    last_used_at         TEXT,
    vault_item_id        TEXT NOT NULL DEFAULT '',
    credential_grant_id  TEXT NOT NULL DEFAULT '',
    task_id              TEXT NOT NULL DEFAULT '',
    expires_at           TEXT,
    revoked_at           TEXT,
    use_count            INTEGER NOT NULL DEFAULT 0
);

INSERT INTO runtime_placeholders (
    placeholder, user_id, agent_id, service_id, created_at, last_used_at,
    vault_item_id, credential_grant_id, task_id, expires_at, revoked_at, use_count
)
SELECT
    placeholder, user_id, agent_id, service_id, created_at, last_used_at,
    vault_item_id, credential_grant_id, task_id, expires_at, revoked_at, use_count
FROM runtime_placeholders_old;

DROP TABLE runtime_placeholders_old;

CREATE INDEX idx_runtime_placeholders_agent
    ON runtime_placeholders(agent_id, created_at DESC);

CREATE INDEX idx_runtime_placeholders_task
    ON runtime_placeholders(task_id, created_at DESC);
