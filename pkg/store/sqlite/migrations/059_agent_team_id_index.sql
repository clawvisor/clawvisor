-- Companion to 058_agent_team_id.sql. Lives in a separate migration so
-- that if 058 hits the runner's "duplicate column name" tolerance path
-- (column already present from an earlier run / renumber), the index
-- creation still runs. CREATE INDEX IF NOT EXISTS is idempotent on its
-- own, so re-applying this migration is safe.
CREATE INDEX IF NOT EXISTS idx_agents_team ON agents(team_id) WHERE team_id IS NOT NULL;
