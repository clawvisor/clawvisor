import { randomUUID } from "node:crypto";

import type { BridgeClient } from "./clawvisor-client.js";

export interface MessageBridgeOptions {
  client: BridgeClient;
  /** Fallback group chat ID when no conversation/channel context is available. */
  groupChatId: string;
  logger: {
    warn(msg: string, ...args: unknown[]): void;
  };
}

export interface ForwardEvent {
  content: string;
  from?: string;
  channelId?: string;
  conversationId?: string;
  timestamp?: number;
  role?: "user" | "assistant" | "tool" | "system";
  /** Platform's opaque per-message id, if available. */
  messageId?: string;
  /** Platform tag: "slack", "discord", "telegram", etc. */
  channel?: string;
  /** Thread / session id within the channel. */
  threadId?: string;
}

/**
 * Forwards both user-inbound and agent-outbound messages to Clawvisor's
 * buffer so auto-approval has full conversation context. Authenticated with
 * the plugin's bridge token — never an agent token — so that a compromised
 * agent cannot plant approval messages. Fire-and-forget: errors are logged
 * but never block the message pipeline.
 *
 * Adds per-event integrity fields the server enforces:
 *   - event_id: random UUID unless the platform provides a stable messageId
 *     (in which case we use it, so cross-process retries dedupe cleanly).
 *   - seq: per-bridge monotonically increasing counter. Server rejects
 *     regressions and dup-seq-with-different-event. Counter is in-memory
 *     and resets across plugin restarts — the server also resets (well,
 *     persists but skip checks on first post-restart event, see note
 *     below). For v1 we accept that a plugin restart may cause the first
 *     few post-restart events to be rejected if the clock runs backwards,
 *     and use the wall-clock-derived counter to minimize that risk.
 */
export class MessageBridge {
  private client: BridgeClient;
  private groupChatId: string;
  private logger: MessageBridgeOptions["logger"];
  // Start seq from current millis so cross-restart values are still
  // monotonic as long as the clock doesn't jump back. Plain incrementing
  // counter would reset to 1 on restart and immediately be rejected by
  // the server's AdvanceBridgeLastSeq check.
  private nextSeq: number;

  constructor(opts: MessageBridgeOptions) {
    this.client = opts.client;
    this.groupChatId = opts.groupChatId;
    this.logger = opts.logger;
    this.nextSeq = Date.now();
  }

  forward(event: ForwardEvent): void {
    if (!event.content) return;

    const groupChatId = event.conversationId ?? event.channelId ?? this.groupChatId;
    const from = event.from ?? "unknown";
    const seq = this.nextSeq++;
    // Prefer platform message id (stable across our retries) and fall back
    // to a fresh UUID. Either way the server dedupes on (bridge, event_id).
    const eventId = event.messageId ?? randomUUID();

    // Server expects Unix seconds. OpenClaw's inbound channel events (e.g.
    // telegram, webchat) hand us milliseconds; normalize so they don't land
    // way in the future and get silently dropped as `future_timestamp`.
    let timestamp: number;
    if (typeof event.timestamp === "number" && event.timestamp > 0) {
      timestamp = event.timestamp >= 1e12 ? Math.floor(event.timestamp / 1000) : event.timestamp;
    } else {
      timestamp = Math.floor(Date.now() / 1000);
    }

    this.client
      .ingestMessage({
        group_chat_id: groupChatId,
        text: event.content,
        sender_id: from,
        sender_name: from,
        timestamp,
        event_id: eventId,
        seq,
        role: event.role,
        channel: event.channel,
        thread_id: event.threadId,
        message_id: event.messageId,
      })
      .catch((err) => {
        this.logger.warn(`clawvisor buffer ingest failed: ${(err as Error).message}`);
      });
  }
}
