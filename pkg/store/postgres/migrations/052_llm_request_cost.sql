-- Per-LLM-request cost and token usage. One row per upstream LLM
-- call. See the SQLite copy of this migration for the rationale
-- (separate table to avoid mostly-NULL columns on audit_log;
-- task_id denormalised for fast SUM rollups; cost_micros nullable
-- for unknown-model rows so aggregates surface them).
CREATE TABLE IF NOT EXISTS llm_request_cost (
  audit_id            TEXT PRIMARY KEY,
  user_id             TEXT NOT NULL,
  agent_id            TEXT,
  task_id             TEXT,
  request_id          TEXT NOT NULL,
  timestamp           TIMESTAMPTZ NOT NULL,
  provider            TEXT NOT NULL,
  model               TEXT NOT NULL,
  input_tokens        INTEGER NOT NULL DEFAULT 0,
  output_tokens       INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
  cache_write_tokens  INTEGER NOT NULL DEFAULT 0,
  cost_micros         BIGINT
);

-- Primary read path: GetTaskCost filters on (user_id, task_id).
-- A user_id-leading composite lets the planner do an index-only
-- seek without a heap filter on user_id, which matters once a
-- single task accumulates many cost rows.
CREATE INDEX IF NOT EXISTS idx_llm_cost_user_task
  ON llm_request_cost(user_id, task_id)
  WHERE task_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_llm_cost_user_time
  ON llm_request_cost(user_id, timestamp);
