CREATE TABLE IF NOT EXISTS notification_messages (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    target_type TEXT NOT NULL,
    target_id   TEXT NOT NULL,
    channel     TEXT NOT NULL,
    message_id  TEXT NOT NULL,
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_notif_msg_target ON notification_messages(target_type, target_id, channel);
