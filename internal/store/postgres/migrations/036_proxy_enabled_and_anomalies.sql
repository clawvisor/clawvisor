-- Per-bridge proxy-enablement flag + transcript anomaly table.
-- See docs/design-proxy-stage1.md §6, §3.2, §5.4.
ALTER TABLE bridge_tokens ADD COLUMN proxy_enabled BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE transcript_anomalies (
    id                 TEXT PRIMARY KEY,
    bridge_id          TEXT NOT NULL REFERENCES bridge_tokens(id) ON DELETE CASCADE,
    conversation_id    TEXT NOT NULL DEFAULT '',
    kind               TEXT NOT NULL,
    detail             JSONB,
    detected_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at        TIMESTAMPTZ,
    resolved_by        TEXT
);

CREATE INDEX idx_transcript_anomalies_bridge_detected ON transcript_anomalies(bridge_id, detected_at DESC);
