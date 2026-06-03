-- SQLite ALTER TABLE doesn't support IF NOT EXISTS on columns, but the migration
-- runner is idempotent by version number and tolerates "duplicate column name" errors.
ALTER TABLE tasks ADD COLUMN max_cost_micros INTEGER CHECK (max_cost_micros >= 0);
ALTER TABLE tasks ADD COLUMN max_tokens INTEGER CHECK (max_tokens >= 0);

ALTER TABLE agent_runtime_settings ADD COLUMN max_cost_micros INTEGER CHECK (max_cost_micros >= 0);
ALTER TABLE agent_runtime_settings ADD COLUMN max_tokens INTEGER CHECK (max_tokens >= 0);
