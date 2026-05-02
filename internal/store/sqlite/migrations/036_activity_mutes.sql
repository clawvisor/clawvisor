CREATE TABLE activity_mutes (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL,
  host TEXT NOT NULL,
  path_prefix TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX idx_activity_mutes_user_host_path
  ON activity_mutes(user_id, host, path_prefix);

CREATE INDEX idx_activity_mutes_user_id
  ON activity_mutes(user_id);
