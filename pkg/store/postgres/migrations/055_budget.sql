ALTER TABLE tasks ADD COLUMN IF NOT EXISTS max_cost_micros BIGINT CHECK (max_cost_micros >= 0);
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS max_tokens BIGINT CHECK (max_tokens >= 0);

ALTER TABLE agent_runtime_settings ADD COLUMN IF NOT EXISTS max_cost_micros BIGINT CHECK (max_cost_micros >= 0);
ALTER TABLE agent_runtime_settings ADD COLUMN IF NOT EXISTS max_tokens BIGINT CHECK (max_tokens >= 0);
