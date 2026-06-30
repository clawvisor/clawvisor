-- See sqlite/057_llm_request_cost_agent_time_index.sql for rationale.
-- Postgres version: same shape with TIMESTAMPTZ ordering.
CREATE INDEX IF NOT EXISTS idx_llm_cost_agent_time
  ON llm_request_cost(agent_id, timestamp)
  WHERE agent_id IS NOT NULL;
