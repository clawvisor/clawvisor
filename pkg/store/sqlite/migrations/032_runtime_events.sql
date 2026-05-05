CREATE TABLE runtime_events (
    id                   TEXT PRIMARY KEY,
    timestamp            TEXT NOT NULL,
    session_id           TEXT NOT NULL REFERENCES runtime_sessions(id) ON DELETE CASCADE,
    user_id              TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id             TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    provider             TEXT NOT NULL DEFAULT '',
    event_type           TEXT NOT NULL,
    action_kind          TEXT NOT NULL DEFAULT '',
    approval_id          TEXT,
    task_id              TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    matched_task_id      TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    lease_id             TEXT,
    tool_use_id          TEXT,
    request_fingerprint  TEXT,
    resolution_transport TEXT,
    decision             TEXT,
    outcome              TEXT,
    reason               TEXT,
    metadata_json        TEXT NOT NULL DEFAULT '{}'
);

CREATE INDEX idx_runtime_events_user_timestamp
    ON runtime_events(user_id, timestamp DESC);
CREATE INDEX idx_runtime_events_session_timestamp
    ON runtime_events(session_id, timestamp DESC);
