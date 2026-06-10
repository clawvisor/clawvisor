-- Persist the in-flight scope-expansion envelope (replaces the legacy
-- singular pending_action shape). Holds expected_tools / expected_egress
-- / required_credentials JSON arrays plus the reason the agent gave.
-- Cleared by UpdateTaskEnvelopeFrom on approve / ResolveTaskPendingExpansion on deny.
ALTER TABLE tasks ADD COLUMN pending_expansion_json TEXT;

-- Sweep legacy pending_scope_expansion rows: standing tasks and session
-- tasks still within their expiry roll back to 'active'; tasks whose
-- expiry has passed become 'expired' so we don't briefly re-arm scope
-- that should remain frozen. The legacy pending_action / pending_reason
-- columns are blanked in the same statement so a rolling-deploy old
-- binary doesn't see phantom singular-shape state. See the matching
-- postgres migration for the full rationale.
UPDATE tasks
SET status = 'active',
    pending_action = NULL,
    pending_reason = ''
WHERE status = 'pending_scope_expansion'
  AND (lifetime = 'standing' OR expires_at IS NULL OR expires_at > datetime('now'));

UPDATE tasks
SET status = 'expired',
    pending_action = NULL,
    pending_reason = ''
WHERE status = 'pending_scope_expansion';

-- Columns intentionally left in place for rolling-deploy compatibility;
-- a follow-up migration drops them after all instances are on the new
-- code. See the matching postgres migration's rationale.
