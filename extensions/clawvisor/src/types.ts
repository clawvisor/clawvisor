/**
 * Plugin config loaded from openclaw.plugin.json configSchema. This is the
 * config the user (or the agent during setup) touches — it intentionally
 * contains NO secret material. Tokens live in a plugin-owned 0600 file
 * (see `./secrets.ts`) so they never end up in plaintext YAML, logs, or
 * version control.
 */
export interface ClawvisorPluginConfig {
  enabled?: boolean;
  /** Clawvisor instance URL (e.g. "https://clawvisor.com"). */
  url?: string;
  /**
   * Short-lived one-time pair code deposited by the agent during
   * /skill/setup-openclaw. The plugin redeems it on first run to initiate
   * pairing; server consumes it atomically. Absent after successful
   * pairing — the bridge token takes over.
   */
  pairCode?: string;
  /** Fallback group chat ID when no conversation/channel context is available. */
  groupChatId?: string;
  /** OpenClaw agent IDs to mint agent tokens for on pair. */
  agents?: string[];
}

/**
 * Body of POST /api/buffer/ingest (bridge-token-authenticated). Includes
 * integrity fields (event_id + seq) and structured provenance (role,
 * channel, thread_id, message_id) that the server renders into the
 * approval LLM prompt as discrete JSON fields.
 */
export interface BufferIngestRequest {
  group_chat_id: string;
  text: string;
  sender_id: string;
  sender_name: string;
  timestamp: number;
  /** Client-generated idempotency key; exact (bridge, event_id) replay is a no-op. */
  event_id: string;
  /** Per-bridge monotonic sequence; server rejects regressions and dup-seq. */
  seq: number;
  /** "user" | "assistant" | "tool" | "system". Server validates enum. */
  role?: "user" | "assistant" | "tool" | "system";
  channel?: string;
  thread_id?: string;
  message_id?: string;
}

/** Response from POST /api/plugin/pair?wait=true on approve. */
export interface PairApprovedResponse {
  pair_id: string;
  status: "approved";
  bridge_token?: string;
  agents?: Record<string, string>;
}
