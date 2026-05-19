-- Symmetric dedup scoping: align audit_log, pending_approvals, and
-- approval_records on the same (user_id, request_id, COALESCE(task_id, ''))
-- key. Replaces the "audit-only" dedup scope from PR #361 with one that
-- covers all three tables, so a cross-task reuse of a request_id either
-- succeeds everywhere (different task = different scope = independent rows
-- in every table) or hits a same-scope conflict everywhere (which the
-- handler recovers via the existing dedup-attempt path).
--
-- New shape across the three tables:
--   * audit_log: deduped_of NULL → canonical; deduped_of NOT NULL → retry
--     attempt referencing the canonical. UNIQUE(user_id, request_id,
--     COALESCE(task_id,'')) WHERE deduped_of IS NULL.
--   * pending_approvals: new task_id column; UNIQUE(user_id, request_id,
--     COALESCE(task_id,'')).
--   * approval_records: existing task_id column; the partial unique on
--     request_id is replaced with UNIQUE(user_id, request_id,
--     COALESCE(task_id,'')) WHERE request_id IS NOT NULL AND request_id != ''.
--
-- Why one migration instead of three: the three tables form one logical
-- dedup scope. Splitting the schema change across migrations would create
-- transient states where audit_log permits cross-task canonicals but
-- pending_approvals still UNIQUE-blocks them, which is exactly the wart
-- this redesign exists to remove. Applying them together is the only
-- coherent intermediate state.
--
-- SQLite rebuild dance: both audit_log and pending_approvals carry an
-- inline UNIQUE constraint that SQLite cannot drop in place, so each
-- table is recreated. audit_log is the heavyweight one (months of audit
-- history); pending_approvals is small (only rows still awaiting a human
-- decision) so its rebuild is effectively free.
--
-- Blocking time: the audit_log rewrite is the same shape as migration 028
-- and PR #361's predecessor migration — fast on fresh installs, several
-- seconds on installs with months of audit history. The pending_approvals
-- rebuild is bounded by the number of in-flight approvals (typically 0–10).
--
-- Foreign-key safety on audit_log: chain_facts.audit_id references
-- audit_log(id) ON DELETE CASCADE. PRAGMA defer_foreign_keys = ON only
-- defers the *constraint check* to COMMIT — it does not suppress cascade
-- actions. DROP TABLE audit_log fires an implicit DELETE FROM audit_log,
-- which cascades to chain_facts and empties it. PRAGMA foreign_keys = OFF
-- would suppress cascades but cannot be toggled inside a transaction (and
-- the migration runner wraps each migration in one). Workaround: snapshot
-- chain_facts to a TEMP TABLE before the rebuild and restore it afterwards.
-- pending_approvals has no inbound FKs (verified at migration authoring
-- time) so no companion backup is needed for it.

PRAGMA defer_foreign_keys = ON;

-- ─────────────────────────────────────────────────────────────────────────────
-- 1. audit_log: add deduped_of, replace UNIQUE(request_id, user_id) with the
--    partial canonical-dedup index. Same shape as PR #361's migration 042.
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TEMP TABLE chain_facts_backup AS SELECT * FROM chain_facts;

CREATE TABLE audit_log_new (
    id                          TEXT PRIMARY KEY,
    user_id                     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id                    TEXT REFERENCES agents(id) ON DELETE SET NULL,
    request_id                  TEXT NOT NULL,
    task_id                     TEXT,
    session_id                  TEXT,
    approval_id                 TEXT,
    lease_id                    TEXT,
    tool_use_id                 TEXT,
    matched_task_id             TEXT,
    lease_task_id               TEXT,
    timestamp                   TEXT NOT NULL DEFAULT (datetime('now')),
    service                     TEXT NOT NULL,
    action                      TEXT NOT NULL,
    params_safe                 TEXT NOT NULL DEFAULT '{}',
    decision                    TEXT NOT NULL,
    outcome                     TEXT NOT NULL,
    policy_id                   TEXT,
    rule_id                     TEXT,
    resolution_confidence       TEXT,
    intent_verdict              TEXT,
    used_active_task_context    INTEGER NOT NULL DEFAULT 0,
    used_lease_bias             INTEGER NOT NULL DEFAULT 0,
    used_conv_judge_resolution  INTEGER NOT NULL DEFAULT 0,
    would_block                 INTEGER NOT NULL DEFAULT 0,
    would_review                INTEGER NOT NULL DEFAULT 0,
    would_prompt_inline         INTEGER NOT NULL DEFAULT 0,
    safety_flagged              INTEGER NOT NULL DEFAULT 0,
    safety_reason               TEXT,
    reason                      TEXT,
    data_origin                 TEXT,
    context_src                 TEXT,
    duration_ms                 INTEGER NOT NULL DEFAULT 0,
    filters_applied             TEXT,
    verification                TEXT,
    error_msg                   TEXT,
    deduped_of                  TEXT
);

INSERT INTO audit_log_new SELECT
    id, user_id, agent_id, request_id, task_id, session_id, approval_id, lease_id,
    tool_use_id, matched_task_id, lease_task_id, timestamp, service, action,
    params_safe, decision, outcome, policy_id, rule_id, resolution_confidence,
    intent_verdict, used_active_task_context, used_lease_bias, used_conv_judge_resolution,
    would_block, would_review, would_prompt_inline,
    safety_flagged, safety_reason, reason, data_origin, context_src,
    duration_ms, filters_applied, verification, error_msg,
    NULL
FROM audit_log;

DROP TABLE audit_log;
ALTER TABLE audit_log_new RENAME TO audit_log;

INSERT INTO chain_facts SELECT * FROM chain_facts_backup;
DROP TABLE chain_facts_backup;

CREATE INDEX idx_audit_user_time ON audit_log(user_id, timestamp DESC);
CREATE INDEX idx_audit_outcome   ON audit_log(user_id, outcome);
CREATE INDEX idx_audit_service   ON audit_log(user_id, service);
CREATE INDEX idx_audit_runtime_host_path
    ON audit_log(
        user_id,
        service,
        COALESCE(json_extract(params_safe, '$.host'), ''),
        COALESCE(json_extract(params_safe, '$.path'), '')
    );

CREATE UNIQUE INDEX idx_audit_canonical_dedup
    ON audit_log(user_id, request_id, COALESCE(task_id, ''))
    WHERE deduped_of IS NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- 2. pending_approvals: add task_id, replace UNIQUE(request_id) with the
--    composite UNIQUE(user_id, request_id, COALESCE(task_id,'')).
--    Backfill task_id from audit_log via audit_id.
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE pending_approvals_new (
    id                  TEXT PRIMARY KEY,
    user_id             TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    request_id          TEXT NOT NULL,
    task_id             TEXT,
    audit_id            TEXT NOT NULL,
    request_blob        TEXT NOT NULL,
    callback_url        TEXT,
    telegram_msg_id     TEXT,
    expires_at          TEXT NOT NULL,
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    status              TEXT NOT NULL DEFAULT 'pending',
    approval_record_id  TEXT,
    executing_since     TEXT
);

INSERT INTO pending_approvals_new (
    id, user_id, request_id, task_id, audit_id, request_blob,
    callback_url, telegram_msg_id, expires_at, created_at,
    status, approval_record_id, executing_since
)
SELECT
    pa.id, pa.user_id, pa.request_id, al.task_id, pa.audit_id, pa.request_blob,
    pa.callback_url, pa.telegram_msg_id, pa.expires_at, pa.created_at,
    pa.status, pa.approval_record_id, pa.executing_since
FROM pending_approvals pa
LEFT JOIN audit_log al ON al.id = pa.audit_id;

DROP TABLE pending_approvals;
ALTER TABLE pending_approvals_new RENAME TO pending_approvals;

CREATE INDEX idx_pending_approvals_user_status
    ON pending_approvals(user_id, status);

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

-- ─────────────────────────────────────────────────────────────────────────────
-- 4. notification_messages: pre-migration approval rows were stored with
--    target_id = request_id. Post-migration the handler addresses the row
--    with target_id = request_id || '|' || task_id (see approvalNotifyTargetID
--    in internal/api/handlers/gateway.go). Backfill so existing in-flight
--    Telegram messages remain addressable.
--
--    The UPDATE only touches rows whose pending_approval now has a non-empty
--    task_id (back-filled in step 2). Rows for pre-task approvals stay under
--    their original request_id key, matching approvalNotifyTargetID's
--    pre-task branch.
-- ─────────────────────────────────────────────────────────────────────────────

UPDATE notification_messages
SET target_id = target_id || '|' || (
    SELECT pa.task_id FROM pending_approvals pa
    WHERE pa.request_id = notification_messages.target_id
      AND pa.task_id IS NOT NULL AND pa.task_id != ''
    LIMIT 1
)
WHERE target_type = 'approval'
  AND target_id IN (
      SELECT pa2.request_id FROM pending_approvals pa2
      WHERE pa2.task_id IS NOT NULL AND pa2.task_id != ''
  );
