-- 007_multi_account: Add alias column to service_meta for multi-account support.

ALTER TABLE service_meta ADD COLUMN IF NOT EXISTS alias TEXT NOT NULL DEFAULT 'default';
ALTER TABLE service_meta DROP CONSTRAINT IF EXISTS service_meta_user_id_service_id_key;
ALTER TABLE service_meta ADD CONSTRAINT service_meta_user_id_service_id_alias_key UNIQUE(user_id, service_id, alias);
