-- Persist MCP sessions so they survive daemon restarts.
CREATE TABLE mcp_sessions (
    id         TEXT PRIMARY KEY,
    expires_at TIMESTAMPTZ NOT NULL
);
