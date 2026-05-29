-- Add an optional child dedup key for canonical audit rows. Existing rows keep
-- dedup_key NULL, so the request-level unique index has the same key space as
-- the old canonical index and needs no backfill. Rows that are child
-- observations inside one request set dedup_key and use a separate child
-- unique index while preserving the original request_id for grouping/debugging.

ALTER TABLE audit_log ADD COLUMN dedup_key TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_audit_canonical_request_dedup
    ON audit_log(user_id, request_id, COALESCE(task_id, ''))
    WHERE deduped_of IS NULL AND dedup_key IS NULL;

DROP INDEX IF EXISTS idx_audit_canonical_dedup;

CREATE UNIQUE INDEX IF NOT EXISTS idx_audit_canonical_child_dedup
    ON audit_log(user_id, request_id, COALESCE(task_id, ''), dedup_key)
    WHERE deduped_of IS NULL AND dedup_key IS NOT NULL;
