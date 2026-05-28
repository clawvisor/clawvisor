-- Per-agent setting for conversation-based auto-approval of inline task
-- creation. Lives on agent_runtime_settings alongside the other lite-
-- proxy runtime knobs (lite_proxy_secret_detection_disabled, etc.)
-- because it's a "how does the runtime treat this agent's tool calls"
-- toggle, read on every llm-proxy request via agent.RuntimeSettings.
--
-- Values: "off" | "low" | "medium" (UI cap) | "high" | "critical".
-- The auto-approve gate compares risk levels via riskRank() and works
-- at any level; product/UI surfaces today restrict the user-settable
-- value to off/low/medium. Server-side validation in pkg/store enforces
-- the same cap so a direct API client cannot write a higher threshold.
-- Defaults to "off" so existing agents see no behavior change.
ALTER TABLE agent_runtime_settings
ADD COLUMN IF NOT EXISTS conversation_auto_approve_threshold VARCHAR(20) NOT NULL DEFAULT 'off';
