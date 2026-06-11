-- See postgres/migrations/056_task_lifecycle_events.sql for the
-- design rationale. SQLite shape: JSONB columns become TEXT (SQLite
-- has no native JSONB), TIMESTAMPTZ becomes TEXT (ISO-8601 UTC), and
-- NOW() defaults are folded into application-side defaults.
CREATE TABLE task_lifecycle_events (
    id                  TEXT PRIMARY KEY,
    task_id             TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    user_id             TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id            TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    event_type          TEXT NOT NULL,
    occurred_at         TEXT NOT NULL,
    approval_id         TEXT,
    approval_surface    TEXT NOT NULL DEFAULT '',
    conversation_id     TEXT,
    request_id          TEXT,
    tool_use_id         TEXT,
    tool_name           TEXT NOT NULL DEFAULT '',
    tool_input_json     TEXT,
    payload_json        TEXT NOT NULL DEFAULT '{}',
    notes               TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL
);

CREATE INDEX idx_task_lifecycle_events_task_occurred
    ON task_lifecycle_events(task_id, occurred_at);
CREATE INDEX idx_task_lifecycle_events_approval
    ON task_lifecycle_events(approval_id) WHERE approval_id IS NOT NULL;
CREATE INDEX idx_task_lifecycle_events_conversation
    ON task_lifecycle_events(conversation_id) WHERE conversation_id IS NOT NULL;
