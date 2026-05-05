CREATE TABLE IF NOT EXISTS activity_mutes (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  host TEXT NOT NULL,
  path_prefix TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_activity_mutes_user_host_path
  ON activity_mutes(user_id, host, path_prefix);

CREATE INDEX IF NOT EXISTS idx_activity_mutes_user_id
  ON activity_mutes(user_id);
