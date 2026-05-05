CREATE TABLE runtime_placeholders (
    placeholder   TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id      TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    service_id    TEXT NOT NULL,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    last_used_at  TEXT
);

CREATE INDEX idx_runtime_placeholders_agent
    ON runtime_placeholders(agent_id, created_at DESC);
