-- Tracking user activity in conversations to support inactivity-based notification routing.
CREATE TABLE IF NOT EXISTS conversations (
  id                    TEXT PRIMARY KEY,
  last_user_message_at  TIMESTAMPTZ NOT NULL
);
