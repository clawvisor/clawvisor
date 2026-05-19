-- Per-task override for chain-context extraction mode. NULL or "" means the
-- system default applies (today: full Phase-2 LLM extraction). "builtins_only"
-- skips the async LLM extraction pass — only the synchronous builtin regex
-- patterns run. "full" forces today's behavior even after the system default
-- is flipped to builtins_only.
--
-- Enforced in the gateway's extraction dispatch; see resolveChainExtractionMode
-- in internal/api/handlers/gateway.go.
ALTER TABLE tasks ADD COLUMN chain_extraction_mode TEXT;
