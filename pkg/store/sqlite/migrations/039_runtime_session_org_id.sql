-- Phase 0.4: per-runtime-session org_id, populated from agent.org_id at
-- session creation. Empty string = "no org".
ALTER TABLE runtime_sessions ADD COLUMN org_id TEXT NOT NULL DEFAULT '';

UPDATE runtime_sessions
   SET org_id = COALESCE((SELECT org_id FROM agents WHERE agents.id = runtime_sessions.agent_id), '')
 WHERE org_id = '';

CREATE INDEX IF NOT EXISTS idx_runtime_sessions_org_agent ON runtime_sessions (org_id, agent_id);
