-- Optional team assignment for an agent. NULL = no team (existing
-- rows are unaffected). The cloud-side spend-cap enforcer reads
-- this to decide whether a team cap applies before the org cap.
-- We don't FK to teams(id) because that table lives in cloud, not
-- in clawvisor — cloud validates team membership at write time.
ALTER TABLE agents ADD COLUMN IF NOT EXISTS team_id TEXT;
CREATE INDEX IF NOT EXISTS idx_agents_team ON agents(team_id) WHERE team_id IS NOT NULL;
