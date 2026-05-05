-- 007_multi_account: Add alias column to service_meta for multi-account support.
-- SQLite requires table recreation to add column with unique constraint.

CREATE TABLE service_meta_new (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    service_id TEXT NOT NULL,
    alias TEXT NOT NULL DEFAULT 'default',
    activated_at TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, service_id, alias)
);
INSERT INTO service_meta_new (id, user_id, service_id, alias, activated_at, updated_at)
    SELECT id, user_id, service_id, 'default', activated_at, updated_at FROM service_meta;
DROP TABLE service_meta;
ALTER TABLE service_meta_new RENAME TO service_meta;
CREATE INDEX IF NOT EXISTS idx_service_meta_user ON service_meta(user_id);
