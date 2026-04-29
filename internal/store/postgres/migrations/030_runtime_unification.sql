ALTER TABLE tasks ADD COLUMN expected_tools_json JSONB NOT NULL DEFAULT '[]';
ALTER TABLE tasks ADD COLUMN expected_egress_json JSONB NOT NULL DEFAULT '[]';
ALTER TABLE tasks ADD COLUMN intent_verification_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN expected_use TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN schema_version INTEGER NOT NULL DEFAULT 1;

ALTER TABLE pending_approvals ADD COLUMN approval_record_id TEXT;

ALTER TABLE audit_log
    ADD COLUMN session_id TEXT,
    ADD COLUMN approval_id TEXT,
    ADD COLUMN lease_id TEXT,
    ADD COLUMN tool_use_id TEXT,
    ADD COLUMN matched_task_id TEXT,
    ADD COLUMN lease_task_id TEXT,
    ADD COLUMN resolution_confidence TEXT,
    ADD COLUMN intent_verdict TEXT,
    ADD COLUMN used_active_task_context BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN used_lease_bias BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN used_conv_judge_resolution BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN would_block BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN would_review BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN would_prompt_inline BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE approval_records (
    id                   TEXT PRIMARY KEY,
    kind                 TEXT NOT NULL,
    user_id              TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id             TEXT REFERENCES agents(id) ON DELETE SET NULL,
    request_id           TEXT,
    task_id              TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    session_id           TEXT,
    status               TEXT NOT NULL,
    surface              TEXT NOT NULL DEFAULT '',
    summary_json         JSONB NOT NULL DEFAULT '{}',
    payload_json         JSONB NOT NULL DEFAULT '{}',
    resolution_transport TEXT NOT NULL DEFAULT '',
    expires_at           TIMESTAMPTZ,
    resolved_at          TIMESTAMPTZ,
    resolution           TEXT NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE runtime_sessions (
    id                        TEXT PRIMARY KEY,
    user_id                   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id                  TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    mode                      TEXT NOT NULL,
    proxy_bearer_secret_hash  TEXT NOT NULL UNIQUE,
    observation_mode          BOOLEAN NOT NULL DEFAULT FALSE,
    metadata_json             JSONB NOT NULL DEFAULT '{}',
    expires_at                TIMESTAMPTZ NOT NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at                TIMESTAMPTZ
);

CREATE TABLE one_off_approvals (
    id                  TEXT PRIMARY KEY,
    session_id          TEXT NOT NULL,
    request_fingerprint TEXT NOT NULL,
    approval_id         TEXT,
    approved_at         TIMESTAMPTZ NOT NULL,
    expires_at          TIMESTAMPTZ NOT NULL,
    used_at             TIMESTAMPTZ
);

CREATE TABLE tool_execution_leases (
    lease_id       TEXT PRIMARY KEY,
    session_id     TEXT NOT NULL,
    task_id        TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    tool_use_id    TEXT NOT NULL,
    tool_name      TEXT NOT NULL,
    status         TEXT NOT NULL,
    metadata_json  JSONB NOT NULL DEFAULT '{}',
    opened_at      TIMESTAMPTZ NOT NULL,
    expires_at     TIMESTAMPTZ NOT NULL,
    closed_at      TIMESTAMPTZ
);

CREATE TABLE task_invocations (
    id              TEXT PRIMARY KEY,
    task_id         TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    session_id      TEXT NOT NULL,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id        TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    request_id      TEXT NOT NULL DEFAULT '',
    invocation_type TEXT NOT NULL,
    status          TEXT NOT NULL,
    metadata_json   JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at    TIMESTAMPTZ
);

CREATE TABLE task_calls (
    id             TEXT PRIMARY KEY,
    task_id        TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    invocation_id  TEXT NOT NULL DEFAULT '',
    request_id     TEXT NOT NULL DEFAULT '',
    session_id     TEXT NOT NULL DEFAULT '',
    service        TEXT NOT NULL,
    action         TEXT NOT NULL,
    outcome        TEXT NOT NULL DEFAULT '',
    approval_id    TEXT,
    audit_id       TEXT,
    metadata_json  JSONB NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE active_task_sessions (
    id            TEXT PRIMARY KEY,
    task_id       TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    session_id    TEXT NOT NULL,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id      TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    status        TEXT NOT NULL,
    metadata_json JSONB NOT NULL DEFAULT '{}',
    started_at    TIMESTAMPTZ NOT NULL,
    last_seen_at  TIMESTAMPTZ NOT NULL,
    ended_at      TIMESTAMPTZ
);

CREATE UNIQUE INDEX idx_approval_records_request_id
    ON approval_records(request_id)
    WHERE request_id IS NOT NULL AND request_id != '';
CREATE INDEX idx_approval_records_user_status
    ON approval_records(user_id, status, created_at DESC);
CREATE INDEX idx_runtime_sessions_agent
    ON runtime_sessions(agent_id, created_at DESC);
CREATE INDEX idx_runtime_sessions_secret_hash
    ON runtime_sessions(proxy_bearer_secret_hash);
CREATE INDEX idx_one_off_approvals_session_fingerprint
    ON one_off_approvals(session_id, request_fingerprint, expires_at);
CREATE INDEX idx_tool_execution_leases_session_status
    ON tool_execution_leases(session_id, status, expires_at);
CREATE INDEX idx_task_invocations_task_session
    ON task_invocations(task_id, session_id, created_at DESC);
CREATE INDEX idx_task_calls_task_created
    ON task_calls(task_id, created_at DESC);
CREATE UNIQUE INDEX idx_active_task_sessions_task_session
    ON active_task_sessions(task_id, session_id);
