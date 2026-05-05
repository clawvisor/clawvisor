-- Agent-to-group-chat pairings for Telegram auto-approval.
CREATE TABLE IF NOT EXISTS agent_group_pairings (
    id             TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id       TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    group_chat_id  TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(agent_id)
);
CREATE INDEX IF NOT EXISTS idx_agent_group_pairings_group ON agent_group_pairings(group_chat_id);
CREATE INDEX IF NOT EXISTS idx_agent_group_pairings_user  ON agent_group_pairings(user_id);

-- Approval metadata on tasks.
ALTER TABLE tasks ADD COLUMN approval_source    TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN approval_rationale TEXT NOT NULL DEFAULT '';
