-- Telegram groups table: stores per-group settings independently from notification_configs.
-- Supports multiple groups per user, each with independent auto-approval settings.
CREATE TABLE IF NOT EXISTS telegram_groups (
    id                     TEXT PRIMARY KEY,
    user_id                TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    group_chat_id          TEXT NOT NULL,
    title                  TEXT NOT NULL DEFAULT '',
    auto_approval_enabled  BOOLEAN NOT NULL DEFAULT FALSE,
    auto_approval_notify   BOOLEAN NOT NULL DEFAULT TRUE,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(user_id, group_chat_id)
);
CREATE INDEX IF NOT EXISTS idx_telegram_groups_user ON telegram_groups(user_id);

-- Migrate existing single-group data from notification_configs into telegram_groups.
INSERT INTO telegram_groups (id, user_id, group_chat_id, title, auto_approval_enabled, auto_approval_notify, created_at, updated_at)
SELECT
    gen_random_uuid()::text,
    nc.user_id,
    nc.config->>'group_chat_id',
    '',
    COALESCE((nc.config->>'auto_approval_enabled')::boolean, FALSE),
    COALESCE((nc.config->>'auto_approval_notify')::boolean, TRUE),
    NOW(),
    NOW()
FROM notification_configs nc
WHERE nc.channel = 'telegram'
  AND nc.config->>'group_chat_id' IS NOT NULL
  AND nc.config->>'group_chat_id' != ''
ON CONFLICT DO NOTHING;
