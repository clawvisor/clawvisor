-- Per-LLM-request cost and token usage. One row per upstream LLM
-- call. Lives in its own table (not on audit_log) because the vast
-- majority of audit rows are tool_use / approval / resolver swaps
-- with no usage to record — widening audit_log would be mostly NULLs.
--
-- TaskID is denormalised onto the row (rather than joining via
-- audit_id → audit_log.task_id) so the primary read pattern —
-- SUM(cost_micros) WHERE task_id = ? — is a single-table index seek.
--
-- cost_micros is int64 micro-USD (1e-6 USD per unit) to avoid float
-- drift on SUM(); NULL when the model isn't in the pricing table so
-- aggregates can surface "unknown-model spend" rather than silently
-- under-bill. Tokens are recorded regardless so cost is re-derivable
-- if a pricing-table bug slips through.
--
-- audit_id FKs into audit_log(id) ON DELETE CASCADE so cost rows can't
-- become orphaned (e.g. when an audit row is GC'd later, or when a
-- code path tries to insert a cost row whose audit_id never landed —
-- the dedup-conflict path in LogEndpointCall resolves the surviving
-- canonical audit row's id before insert specifically to honor this).
-- The SQLite store enables this with PRAGMA foreign_keys = ON in
-- sqlite.New (migration 001 also sets it for new DBs).
CREATE TABLE IF NOT EXISTS llm_request_cost (
  audit_id            TEXT PRIMARY KEY REFERENCES audit_log(id) ON DELETE CASCADE,
  user_id             TEXT NOT NULL,
  agent_id            TEXT,
  task_id             TEXT,
  request_id          TEXT NOT NULL,
  timestamp           DATETIME NOT NULL,
  provider            TEXT NOT NULL,
  model               TEXT NOT NULL,
  input_tokens        INTEGER NOT NULL DEFAULT 0,
  output_tokens       INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
  cache_write_tokens  INTEGER NOT NULL DEFAULT 0,
  cost_micros         INTEGER
);

-- Primary aggregation path: per-task cost rollups. The composite
-- (user_id, task_id) matches GetTaskCost's WHERE clause exactly so
-- the planner can do an index-only seek without filtering on
-- user_id in the heap.
CREATE INDEX IF NOT EXISTS idx_llm_cost_user_task
  ON llm_request_cost(user_id, task_id)
  WHERE task_id IS NOT NULL;

-- Secondary aggregation path: per-user time-range billing.
CREATE INDEX IF NOT EXISTS idx_llm_cost_user_time
  ON llm_request_cost(user_id, timestamp);
