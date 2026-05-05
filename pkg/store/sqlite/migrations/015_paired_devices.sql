CREATE TABLE paired_devices (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_name     TEXT NOT NULL,
    device_token    TEXT NOT NULL,
    device_hmac_key TEXT NOT NULL,
    paired_at       TEXT NOT NULL DEFAULT (datetime('now')),
    last_seen_at    TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_paired_devices_user_id ON paired_devices(user_id);
