-- 005_auth_model: Replace policies with restrictions + task lifetime support.

CREATE TABLE IF NOT EXISTS restrictions (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    service    TEXT NOT NULL,
    action     TEXT NOT NULL DEFAULT '*',
    reason     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_restrictions_user ON restrictions(user_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_restrictions_unique ON restrictions(user_id, service, action);

ALTER TABLE tasks ADD COLUMN lifetime TEXT NOT NULL DEFAULT 'session';

DROP TABLE IF EXISTS policies;
