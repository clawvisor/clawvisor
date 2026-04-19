-- Transcript events ingested by the Clawvisor Proxy (Stage 1+).
-- Proxy captures user <-> assistant <-> tool turns at the wire level
-- and POSTs structured TurnEvents here for auto-approval + audit.
--
-- See docs/design-proxy-stage1.md §5.1 and docs/proxy-api.md §8.1.
CREATE TABLE transcript_events (
    event_id         TEXT PRIMARY KEY,
    bridge_id        TEXT NOT NULL REFERENCES bridge_tokens(id) ON DELETE CASCADE,
    source           TEXT NOT NULL,                  -- 'proxy' | 'plugin'
    source_version   TEXT NOT NULL DEFAULT '',
    stream           TEXT NOT NULL,                  -- 'llm' | 'channel' | 'action'
    agent_token_id   TEXT NOT NULL DEFAULT '',       -- cvis_... token (may be '' when unattributed)
    conversation_id  TEXT NOT NULL DEFAULT '',       -- provider-native; 'telegram:12345' etc. (may be '' for stateless LLM)
    provider         TEXT NOT NULL DEFAULT '',       -- 'anthropic' | 'telegram' | 'openai' | ...
    direction        TEXT NOT NULL DEFAULT '',       -- 'inbound' | 'outbound'
    role             TEXT NOT NULL DEFAULT '',       -- 'user' | 'assistant' | 'tool' | 'system'
    text             TEXT NOT NULL DEFAULT '',
    tool_calls       TEXT NOT NULL DEFAULT '',       -- JSON array; '' when absent
    tool_results     TEXT NOT NULL DEFAULT '',       -- JSON array; '' when absent
    raw_ref          TEXT NOT NULL DEFAULT '',       -- JSON; pointer into proxy's traffic log
    signature        TEXT NOT NULL DEFAULT '',       -- JSON: {alg, key_id, sig}
    sig_status       TEXT NOT NULL DEFAULT 'unsigned', -- 'valid' | 'invalid' | 'unsigned' (audit-only at Stage 1)
    ts               TEXT NOT NULL,                  -- event timestamp (capture time)
    ingested_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_transcript_events_bridge_ts ON transcript_events(bridge_id, ts DESC);
CREATE INDEX idx_transcript_events_convo_ts ON transcript_events(conversation_id, ts DESC);
CREATE INDEX idx_transcript_events_ingested ON transcript_events(ingested_at);
