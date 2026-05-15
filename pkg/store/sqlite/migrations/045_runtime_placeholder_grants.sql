ALTER TABLE runtime_placeholders ADD COLUMN vault_item_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runtime_placeholders ADD COLUMN credential_grant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runtime_placeholders ADD COLUMN task_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runtime_placeholders ADD COLUMN expires_at TEXT;
ALTER TABLE runtime_placeholders ADD COLUMN revoked_at TEXT;
ALTER TABLE runtime_placeholders ADD COLUMN use_count INTEGER NOT NULL DEFAULT 0;

CREATE INDEX idx_runtime_placeholders_task
    ON runtime_placeholders(task_id, created_at DESC);
