-- Append-only audit log of every task lifecycle event. One row per
-- transition: create-pending, create-approved, create-denied, expand-
-- pending, expand-approved, expand-denied, expand-expired, revoked,
-- completed. Each row captures the agent's verbatim original tool_use
-- (tool_use_id, tool_name, tool_input_json) when the event originated
-- from an agent call — this is what the proxy needs to reconstruct
-- the model's missing assistant turn after a substituted-prompt
-- approval, and what an auditor reads to see exactly what the agent
-- asked for.
--
-- Why a dedicated table instead of extending approval_records: the
-- audit story spans events that don't have an approval_record (e.g.
-- sweep-driven expiry, revoke from a non-approval surface) and a row
-- per *transition* is more natural than overloading the single
-- approval row. Keeps approval_records focused on the canonical
-- approval object and lets this table grow at its own cadence.
CREATE TABLE task_lifecycle_events (
    id                  TEXT PRIMARY KEY,
    task_id             TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    user_id             TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id            TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    -- event_type values: task_create_pending, task_create_approved,
    -- task_create_denied, task_expand_pending, task_expand_approved,
    -- task_expand_denied, task_expand_expired, task_revoked,
    -- task_completed, task_expired. New event_types may be added
    -- without a migration; readers MUST treat unknown event_type as
    -- "other lifecycle event" rather than failing.
    event_type          TEXT NOT NULL,
    occurred_at         TIMESTAMPTZ NOT NULL,
    -- approval_id is set whenever the event corresponds to an
    -- approval_record (create_pending/approved/denied,
    -- expand_pending/approved/denied). NULL for sweep / revoke /
    -- complete events that don't go through an approval surface.
    approval_id         TEXT,
    -- approval_surface mirrors approval_records.surface
    -- ("inline_chat", "dashboard", "telegram", ""). Empty for non-
    -- approval events.
    approval_surface    TEXT NOT NULL DEFAULT '',
    -- conversation_id / request_id / tool_use_id correlate the event
    -- back to the LLM turn that triggered it. All three NULL for
    -- non-agent-driven events (sweep, manual revoke from dashboard).
    conversation_id     TEXT,
    request_id          TEXT,
    tool_use_id         TEXT,
    -- tool_name is the original tool_use name (e.g. "Bash"). Empty
    -- for non-agent events.
    tool_name           TEXT NOT NULL DEFAULT '',
    -- tool_input_json is the agent's verbatim tool input — the body
    -- of the curl POST to /control/tasks or /control/tasks/{id}/
    -- expand. The body editor reconstructs the missing assistant
    -- turn from this on the next request; auditors read it to see
    -- exactly what the agent asked for. NULL for non-agent events.
    tool_input_json     JSONB,
    -- payload_json is the event-specific delta: for task_create_*
    -- the full envelope, for task_expand_* the delta (additional
    -- tools / egress / credentials / reason), for resolution events
    -- the merged result. Append-only; never updated.
    payload_json        JSONB NOT NULL DEFAULT '{}',
    notes               TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_task_lifecycle_events_task_occurred
    ON task_lifecycle_events(task_id, occurred_at);
CREATE INDEX idx_task_lifecycle_events_approval
    ON task_lifecycle_events(approval_id) WHERE approval_id IS NOT NULL;
CREATE INDEX idx_task_lifecycle_events_conversation
    ON task_lifecycle_events(conversation_id) WHERE conversation_id IS NOT NULL;
