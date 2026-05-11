-- See pkg/store/sqlite/migrations/042_symmetric_dedup_scope.sql for the full
-- rationale. Summary: align audit_log, pending_approvals, and approval_records
-- on the same (user_id, request_id, COALESCE(task_id, '')) dedup key.
--
-- Postgres doesn't need the inline-UNIQUE rebuild dance, so all three changes
-- are direct DDL: ALTER ... DROP CONSTRAINT, ALTER ... ADD COLUMN, ALTER ...
-- ADD ... USING / CREATE UNIQUE INDEX. The pending_approvals.task_id backfill
-- joins audit_log on audit_id.

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. audit_log: drop the (request_id, user_id) UNIQUE, add deduped_of, add the
--    canonical-dedup partial unique index. Same as PR #361's migration 042.
-- ─────────────────────────────────────────────────────────────────────────────

ALTER TABLE audit_log DROP CONSTRAINT IF EXISTS audit_log_request_id_user_id_key;
ALTER TABLE audit_log ADD COLUMN deduped_of TEXT;

CREATE UNIQUE INDEX idx_audit_canonical_dedup
    ON audit_log(user_id, request_id, COALESCE(task_id, ''))
    WHERE deduped_of IS NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. pending_approvals: drop UNIQUE(request_id), add task_id, backfill from
--    audit_log via audit_id, create the composite unique index.
-- ─────────────────────────────────────────────────────────────────────────────

ALTER TABLE pending_approvals DROP CONSTRAINT IF EXISTS pending_approvals_request_id_key;
ALTER TABLE pending_approvals ADD COLUMN task_id TEXT;

UPDATE pending_approvals pa
SET task_id = al.task_id
FROM audit_log al
WHERE al.id = pa.audit_id
  AND pa.task_id IS NULL;

CREATE UNIQUE INDEX idx_pending_approvals_dedup
    ON pending_approvals(user_id, request_id, COALESCE(task_id, ''));

-- ─────────────────────────────────────────────────────────────────────────────
-- 3. approval_records: replace the request_id-only partial unique index with
--    the composite (user_id, request_id, COALESCE(task_id,'')) version.
--    task_id column already exists (migration 030).
-- ─────────────────────────────────────────────────────────────────────────────

DROP INDEX idx_approval_records_request_id;

CREATE UNIQUE INDEX idx_approval_records_request_id
    ON approval_records(user_id, request_id, COALESCE(task_id, ''))
    WHERE request_id IS NOT NULL AND request_id != '';
