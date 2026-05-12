-- Per-user cache of tools discovered from MCP-backed services. Populated at
-- service activation time (when the user first supplies a credential) so the
-- catalog and gateway can answer "what actions does this user have available?"
-- without spawning the MCP server subprocess on every call.
--
-- Read on registry cache miss for an MCP service; the in-memory cache then
-- absorbs subsequent reads for the rest of the process lifetime.
CREATE TABLE IF NOT EXISTS mcp_tool_caches (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    service_id TEXT NOT NULL,
    alias      TEXT NOT NULL DEFAULT 'default',
    tools      TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, service_id, alias)
);
