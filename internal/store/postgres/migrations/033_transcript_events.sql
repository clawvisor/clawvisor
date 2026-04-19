-- Transcript events ingested by the Clawvisor Proxy (Stage 1+).
-- See docs/design-proxy-stage1.md §5.1 and docs/proxy-api.md §8.1.
CREATE TABLE transcript_events (
    event_id         TEXT PRIMARY KEY,
    bridge_id        TEXT NOT NULL REFERENCES bridge_tokens(id) ON DELETE CASCADE,
    source           TEXT NOT NULL,
    source_version   TEXT NOT NULL DEFAULT '',
    stream           TEXT NOT NULL,
    agent_token_id   TEXT NOT NULL DEFAULT '',
    conversation_id  TEXT NOT NULL DEFAULT '',
    provider         TEXT NOT NULL DEFAULT '',
    direction        TEXT NOT NULL DEFAULT '',
    role             TEXT NOT NULL DEFAULT '',
    text             TEXT NOT NULL DEFAULT '',
    tool_calls       JSONB,
    tool_results     JSONB,
    raw_ref          JSONB,
    signature        JSONB,
    sig_status       TEXT NOT NULL DEFAULT 'unsigned',
    ts               TIMESTAMPTZ NOT NULL,
    ingested_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_transcript_events_bridge_ts ON transcript_events(bridge_id, ts DESC);
CREATE INDEX idx_transcript_events_convo_ts ON transcript_events(conversation_id, ts DESC);
CREATE INDEX idx_transcript_events_ingested ON transcript_events(ingested_at);
