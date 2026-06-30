-- Index for per-org cost dashboards. The cloud governance control
-- panel queries llm_request_cost JOIN agents ON agents.id = c.agent_id
-- WHERE agents.org_id = ? AND c.timestamp BETWEEN ? AND ?, then
-- aggregates SUM(cost_micros) and groups by model / task_id / agent_id /
-- user_id. With only the existing (user_id, task_id) and
-- (user_id, timestamp) indexes the planner falls back to a full scan
-- of llm_request_cost when filtering by agent's org — adding the
-- (agent_id, timestamp) covering tuple lets it seek directly.
--
-- Partial index: NULL agent_id means "direct user proxy-lite call"
-- which the governance dashboards explicitly exclude ("Agent-attributed
-- only" surfaced in UI). Skipping those rows keeps the index small.
CREATE INDEX IF NOT EXISTS idx_llm_cost_agent_time
  ON llm_request_cost(agent_id, timestamp)
  WHERE agent_id IS NOT NULL;
