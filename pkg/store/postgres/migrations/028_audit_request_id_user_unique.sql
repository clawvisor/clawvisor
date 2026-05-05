-- The original UNIQUE constraint on request_id alone causes silent drops
-- when different users happen to reuse the same request_id (e.g. smoke tests).
-- Scope the uniqueness to (request_id, user_id) so each user's request_ids
-- are independent.
ALTER TABLE audit_log DROP CONSTRAINT IF EXISTS audit_log_request_id_key;
ALTER TABLE audit_log ADD CONSTRAINT audit_log_request_id_user_id_key UNIQUE (request_id, user_id);
