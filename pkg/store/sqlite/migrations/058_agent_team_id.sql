-- ADD COLUMN only. SQLite lacks ADD COLUMN IF NOT EXISTS; the runner
-- tolerates "duplicate column name" so this is safe on re-runs, but
-- under that tolerance path the rest of the migration is skipped — so
-- the index lives in 059 as a separate migration that can apply
-- independently.
ALTER TABLE agents ADD COLUMN team_id TEXT;
