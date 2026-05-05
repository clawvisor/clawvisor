-- Telegram groups table: stores per-group settings independently from notification_configs.
-- Supports multiple groups per user, each with independent auto-approval settings.
CREATE TABLE IF NOT EXISTS telegram_groups (
    id                     TEXT PRIMARY KEY,
    user_id                TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    group_chat_id          TEXT NOT NULL,
    title                  TEXT NOT NULL DEFAULT '',
    auto_approval_enabled  INTEGER NOT NULL DEFAULT 0,
    auto_approval_notify   INTEGER NOT NULL DEFAULT 1,
    created_at             TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at             TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(user_id, group_chat_id)
);
CREATE INDEX IF NOT EXISTS idx_telegram_groups_user ON telegram_groups(user_id);

-- Migrate existing single-group data from notification_configs into telegram_groups.
INSERT OR IGNORE INTO telegram_groups (id, user_id, group_chat_id, title, auto_approval_enabled, auto_approval_notify, created_at, updated_at)
SELECT
    lower(hex(randomblob(16))),
    nc.user_id,
    json_extract(nc.config, '$.group_chat_id'),
    '',
    COALESCE(json_extract(nc.config, '$.auto_approval_enabled'), 0),
    COALESCE(json_extract(nc.config, '$.auto_approval_notify'), 1),
    datetime('now'),
    datetime('now')
FROM notification_configs nc
WHERE nc.channel = 'telegram'
  AND json_extract(nc.config, '$.group_chat_id') IS NOT NULL
  AND json_extract(nc.config, '$.group_chat_id') != '';
