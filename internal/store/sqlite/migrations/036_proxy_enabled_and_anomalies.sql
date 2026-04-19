-- Per-bridge proxy-enablement flag. When true, the Clawvisor Proxy is the
-- authoritative transcript source for this bridge; the plugin's scavenger
-- stays in the codebase but is gated off server-side via this flag.
--
-- See docs/design-proxy-stage1.md §6 and §3.2.
ALTER TABLE bridge_tokens ADD COLUMN proxy_enabled INTEGER NOT NULL DEFAULT 0;

-- Transcript cross-check anomalies. Populated by a background job that
-- compares proxy-sourced and plugin-sourced transcripts for the same
-- conversation. Disagreement = potential tampering or plugin/proxy bug.
--
-- See docs/design-proxy-stage1.md §5.4.
CREATE TABLE transcript_anomalies (
    id                 TEXT PRIMARY KEY,
    bridge_id          TEXT NOT NULL REFERENCES bridge_tokens(id) ON DELETE CASCADE,
    conversation_id    TEXT NOT NULL DEFAULT '',
    kind               TEXT NOT NULL,                -- 'plugin_only' | 'proxy_only' | 'content_mismatch'
    detail             TEXT NOT NULL DEFAULT '',     -- JSON; specific turns + fields that disagree
    detected_at        TEXT NOT NULL DEFAULT (datetime('now')),
    resolved_at        TEXT,
    resolved_by        TEXT
);

CREATE INDEX idx_transcript_anomalies_bridge_detected ON transcript_anomalies(bridge_id, detected_at DESC);
