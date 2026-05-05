-- Phase 0.4: per-runtime-session org_id, populated from agent.org_id at
-- session creation. Empty string = "no org" (same convention as
-- agents.org_id). Backfill existing rows from agents in a single statement;
-- this is safe even on a large table because the column is added with a
-- default first, then backfilled with a single UPDATE.

ALTER TABLE runtime_sessions ADD COLUMN org_id TEXT NOT NULL DEFAULT '';

UPDATE runtime_sessions s
   SET org_id = COALESCE(a.org_id, '')
  FROM agents a
 WHERE s.agent_id = a.id
   AND s.org_id = '';

CREATE INDEX IF NOT EXISTS idx_runtime_sessions_org_agent ON runtime_sessions (org_id, agent_id);
