ALTER TABLE agents ADD COLUMN team_id TEXT;
CREATE INDEX IF NOT EXISTS idx_agents_team ON agents(team_id) WHERE team_id IS NOT NULL;
