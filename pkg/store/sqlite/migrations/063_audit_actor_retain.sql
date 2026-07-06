-- 04 flat-team: audit/cost survive user deletion (PRD §15.4, Q4).
--
-- audit_log.user_id was ON DELETE CASCADE, so deleting a departed user
-- wiped exactly the audit trail you need after they leave. Flip it to
-- ON DELETE SET NULL and add a server-derived, NOT-NULL actor_email so the
-- record stays attributable via email-at-the-time even after user_id nulls.
--
-- SQLite cannot alter an FK in place, so the audit_log table is recreated
-- (the 028_* pattern). The wrinkle 028 didn't face: 052 later added
-- llm_request_cost with `audit_id REFERENCES audit_log(id) ON DELETE
-- CASCADE`. With foreign_keys=ON, DROP TABLE audit_log fires an implicit
-- delete that cascades into llm_request_cost and would wipe all cost rows.
-- PRAGMA foreign_keys is a no-op inside the migration transaction, so we
-- protect the cost rows by round-tripping them through a TEMP table across
-- the audit_log swap.

-- 1. Cost table only gains a column (no FK change) — simple ALTER, plus a
--    server-derived backfill (email-at-the-time; sentinel for rows whose
--    user was already deleted).
ALTER TABLE llm_request_cost ADD COLUMN actor_email TEXT NOT NULL DEFAULT '';
UPDATE llm_request_cost SET actor_email =
    COALESCE((SELECT email FROM users WHERE users.id = llm_request_cost.user_id), '(deleted-user)');

-- 2. Back up cost rows before the audit_log DROP cascades them away.
CREATE TEMP TABLE _cost_actor_bak AS SELECT * FROM llm_request_cost;

-- 3. Recreate audit_log with user_id nullable + SET NULL + actor_email.
CREATE TABLE audit_log_new (
    id                          TEXT PRIMARY KEY,
    user_id                     TEXT REFERENCES users(id) ON DELETE SET NULL,
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
    deduped_of                  TEXT,
    dedup_key                   TEXT,
    actor_email                 TEXT NOT NULL DEFAULT ''
);

INSERT INTO audit_log_new (
    id, user_id, agent_id, request_id, task_id, session_id, approval_id, lease_id,
    tool_use_id, matched_task_id, lease_task_id, timestamp, service, action,
    params_safe, decision, outcome, policy_id, rule_id, resolution_confidence,
    intent_verdict, used_active_task_context, used_lease_bias, used_conv_judge_resolution,
    would_block, would_review, would_prompt_inline, safety_flagged, safety_reason,
    reason, data_origin, context_src, duration_ms, filters_applied, verification,
    error_msg, deduped_of, dedup_key, actor_email
)
SELECT
    id, user_id, agent_id, request_id, task_id, session_id, approval_id, lease_id,
    tool_use_id, matched_task_id, lease_task_id, timestamp, service, action,
    params_safe, decision, outcome, policy_id, rule_id, resolution_confidence,
    intent_verdict, used_active_task_context, used_lease_bias, used_conv_judge_resolution,
    would_block, would_review, would_prompt_inline, safety_flagged, safety_reason,
    reason, data_origin, context_src, duration_ms, filters_applied, verification,
    error_msg, deduped_of, dedup_key,
    COALESCE((SELECT email FROM users WHERE users.id = audit_log.user_id), '(deleted-user)')
FROM audit_log;

DROP TABLE audit_log;
ALTER TABLE audit_log_new RENAME TO audit_log;

-- 4. Restore cost rows (audit ids preserved, so the audit_id FK re-satisfies).
INSERT INTO llm_request_cost SELECT * FROM _cost_actor_bak;
DROP TABLE _cost_actor_bak;

-- 5. Rebuild audit_log indexes (identical to pre-recreate definitions).
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
CREATE UNIQUE INDEX idx_audit_canonical_request_dedup
    ON audit_log(user_id, request_id, COALESCE(task_id, ''))
    WHERE deduped_of IS NULL AND dedup_key IS NULL;
CREATE UNIQUE INDEX idx_audit_canonical_child_dedup
    ON audit_log(user_id, request_id, COALESCE(task_id, ''), dedup_key)
    WHERE deduped_of IS NULL AND dedup_key IS NOT NULL;
