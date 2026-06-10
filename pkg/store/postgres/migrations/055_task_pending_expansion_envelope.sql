-- Persist the in-flight scope-expansion envelope (replaces the legacy
-- singular pending_action shape). Holds expected_tools / expected_egress
-- / required_credentials JSON arrays plus the reason the agent gave.
-- Cleared by UpdateTaskEnvelopeFrom on approve / ResolveTaskPendingExpansion on deny.
ALTER TABLE tasks ADD COLUMN pending_expansion_json JSONB;

-- Sweep legacy pending_scope_expansion rows so the new envelope-shape
-- expand flow starts from a clean state. Standing tasks and session
-- tasks still within their expiry roll back to 'active'; tasks whose
-- expiry has passed become 'expired' so we don't briefly re-arm scope
-- that should remain frozen until the cleanup sweeper would have
-- caught them.
--
-- The legacy pending_action / pending_reason columns are blanked in
-- the SAME statement so an old-code instance polling for "pending
-- action on an active task" doesn't see a phantom from the pre-deploy
-- state. Without this NULL'ing, the dual-write window during a
-- rolling deploy lets stale singular-shape data drive old-binary
-- decisions even though the new code has cleared the row's
-- pending_scope_expansion status.
--
-- In-flight expansion requests at deploy time ARE lost — any agent
-- long-polling on /api/tasks/{id}/expand will time out without a
-- verdict. This is an unavoidable cost of the shape change; agents
-- will retry naturally on the next request. Operators should announce
-- a brief expand-flow blackout around the deploy.
UPDATE tasks
SET status = 'active',
    pending_action = NULL,
    pending_reason = ''
WHERE status = 'pending_scope_expansion'
  AND (lifetime = 'standing' OR expires_at IS NULL OR expires_at > NOW());

UPDATE tasks
SET status = 'expired',
    pending_action = NULL,
    pending_reason = ''
WHERE status = 'pending_scope_expansion';

-- Legacy pending_action / pending_reason columns are no longer read by
-- the new store. We intentionally do NOT drop them here: during a
-- rolling deploy an old instance still SELECTing those columns would
-- see "column does not exist" until it was rotated out. A follow-up
-- migration (e.g. 056_drop_legacy_pending_action.sql, after all
-- instances are on the new code) handles the drop.
--
-- Cross-version split-brain (operator-facing): even with the columns
-- left in place, old-code and new-code instances cannot coherently
-- share scope-expansion state for the duration of the rolling deploy.
-- New code writes ONLY pending_expansion_json; old code reads ONLY
-- pending_action. Old-code Approve/Deny against a new-code-created
-- expansion will fail to find the pending state, and old-code's
-- UpdateTaskActions clears legacy columns without touching
-- pending_expansion_json (the new code's read paths gate on
-- status='pending_scope_expansion' so they recover cleanly, but the
-- shared task row will look inconsistent until the deploy finishes).
--
-- Operational guidance: drain old instances of approve/deny traffic
-- before flipping the migration, or accept that any in-flight
-- expansion landing on an old instance during the deploy window will
-- block until the next new-instance request retries.
