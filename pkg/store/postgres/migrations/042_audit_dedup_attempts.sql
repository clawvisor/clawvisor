-- See pkg/store/sqlite/migrations/041_audit_dedup_attempts.sql for the full
-- rationale. Summary: drop the (request_id, user_id) UNIQUE constraint, add a
-- deduped_of column, and replace the constraint with a partial unique index
-- scoped to canonical (deduped_of IS NULL) rows. Per-scope uniqueness is
-- enforced in SQL; cross-scope precedence (pre-task beats task-scoped) is
-- enforced by FindDedupCandidate ordering at read time.

ALTER TABLE audit_log DROP CONSTRAINT IF EXISTS audit_log_request_id_user_id_key;
ALTER TABLE audit_log ADD COLUMN deduped_of TEXT;

CREATE UNIQUE INDEX idx_audit_canonical_dedup
    ON audit_log(user_id, request_id, COALESCE(task_id, ''))
    WHERE deduped_of IS NULL;
