-- Adds agent_attribution to transcript_events. See sqlite migration 041
-- for the trust-tier semantics.
ALTER TABLE transcript_events ADD COLUMN agent_attribution TEXT NOT NULL DEFAULT '';
