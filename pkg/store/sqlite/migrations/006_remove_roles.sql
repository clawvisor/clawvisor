-- 006_remove_roles.sql: Remove agent_roles table and role_id from agents.
-- SQLite can't ALTER TABLE DROP COLUMN, so recreate agents without role_id.

CREATE TABLE agents_new (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
INSERT INTO agents_new SELECT id, user_id, name, token_hash, created_at FROM agents;
DROP TABLE agents;
ALTER TABLE agents_new RENAME TO agents;
CREATE INDEX IF NOT EXISTS idx_agents_user ON agents(user_id);

DROP TABLE IF EXISTS agent_roles;
