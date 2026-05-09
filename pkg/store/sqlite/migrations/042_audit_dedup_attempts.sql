-- Replace the inline UNIQUE(request_id, user_id) constraint with a partial
-- unique index scoped to canonical rows only, and add a deduped_of column
-- so retry attempts can be recorded as their own audit_log rows pointing
-- at the canonical entry.
--
-- Why: the existing UNIQUE blocked legitimate retries from landing in the
-- audit log entirely. The handler's dedup branch returned early without
-- inserting, so an operator scanning audit history could not see that an
-- agent had retried. It also caused a latent bug: cross-task reuse of a
-- request_id was *intended* to land a second canonical row (see
-- TestGateway_Dedup_SameRequestID_DifferentTask_NotDeduplicated), but the
-- second LogAudit silently violated UNIQUE while the handler still
-- returned a fresh audit_id to the agent.
--
-- New shape:
--   * deduped_of NULL → canonical row (one per dedup scope)
--   * deduped_of NOT NULL → retry attempt; references the canonical's id
--   * Per-scope uniqueness (user_id, request_id, COALESCE(task_id,'')) is
--     enforced in SQL; cross-scope precedence (pre-task beats task-scoped)
--     is enforced by FindDedupCandidate ordering at read time. A
--     task-scoped canonical that lands after a pre-task canonical for the
--     same request_id is allowed but never re-wins dedup.
--
-- SQLite cannot drop the inline UNIQUE constraint; we recreate the table
-- the same way migration 028 did, copying every current column.
--
-- Blocking time: this is a full-table rewrite (see migrations/README.md).
-- INSERT INTO audit_log_new SELECT … FROM audit_log copies every existing
-- audit row plus its chain_facts companion, holding an exclusive write lock
-- for the duration. Expect roughly the same window as 028 — fast on fresh
-- installs, several seconds on installs with months of audit history.
--
-- Foreign-key safety: chain_facts.audit_id references audit_log(id) ON DELETE
-- CASCADE. PRAGMA defer_foreign_keys = ON only defers the *constraint check*
-- to COMMIT — it does not suppress cascade actions. DROP TABLE audit_log fires
-- an implicit DELETE FROM audit_log, which cascades to chain_facts and empties
-- it. PRAGMA foreign_keys = OFF would suppress cascades but cannot be toggled
-- inside a transaction (and the migration runner wraps each migration in one).
-- Workaround: snapshot chain_facts to a TEMP TABLE before the rebuild and
-- restore it afterwards. The new audit_log keeps the same ids, so the FK
-- references resolve at COMMIT.

PRAGMA defer_foreign_keys = ON;

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

-- chain_facts was emptied by the cascade during DROP TABLE audit_log; restore
-- it now that the new audit_log has the same row ids the references point at.
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
