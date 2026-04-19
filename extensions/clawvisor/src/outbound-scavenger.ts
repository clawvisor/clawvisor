import { promises as fs } from "node:fs";
import { homedir } from "node:os";
import { createHash } from "node:crypto";
import { join, resolve as resolvePath } from "node:path";

import type { MessageBridge } from "./message-bridge.js";

/**
 * OpenClaw's non-channel plugin hook runner does not route outbound
 * messages to our plugin — `api.on("message_sent", …)` is silent across
 * all channels we've tested (webchat, Telegram, OpenClaw 2026.4.x). The
 * reply content IS on disk though, in the per-agent session JSONL, so
 * we scavenge it from there on each inbound event.
 *
 * Trigger: `api.on("message_received", …)` calls `scavenge()` before
 * forwarding the user message. We read the active session JSONL and
 * forward every assistant node that appears AFTER the last one we've
 * already forwarded (the "watermark"). Each asst gets its own forward
 * so auto-approval can distinguish turns; stable per-node event_ids
 * let the server dedupe retries.
 *
 * A watermark-based scan (rather than "asst between last 2 user nodes")
 * sidesteps the structural lag where the current user isn't written to
 * JSONL yet when the hook fires — we don't need to know which user is
 * "current", we just need to know which assistants are new.
 *
 * Trailing assistant replies (the reply to the final user turn, with no
 * subsequent user to trigger a scan) are intentionally not captured —
 * the auto-approval use case only needs context up through ingestion of
 * the current user message.
 */
export interface ScavengerDeps {
  bridge: MessageBridge;
  logger: {
    info(msg: string, ...args: unknown[]): void;
    warn(msg: string, ...args: unknown[]): void;
    error(msg: string, ...args: unknown[]): void;
  };
  /** Fallback agent id when the hook ctx doesn't carry one. */
  defaultAgentId: string;
}

export type ScavengeTrigger = (ctx: {
  agentId?: string;
  conversationId?: string;
  channelId?: string;
}) => Promise<void>;

export function createScavenger(deps: ScavengerDeps): ScavengeTrigger {
  // In-memory cache of event_ids we've already forwarded within this
  // process. Server-side `(bridge_id, event_id)` dedup is the
  // correctness primitive; this is a round-trip optimization.
  const seen = new Set<string>();
  const SEEN_MAX = 500;

  // Timestamp watermark per agent (Unix seconds). On each scavenge we
  // forward asst nodes whose timestamp is strictly greater. On first
  // call (or after a plugin hot-reload) we default to (now − 15 min)
  // which matches the server buffer's retention window — anything older
  // is gone anyway, and anything within the window that we previously
  // forwarded gets deduped server-side via the stable event_id.
  const BACKFILL_WINDOW_SEC = 15 * 60;
  const watermarkByAgent = new Map<string, number>();

  return async (ctx) => {
    const agentId = ctx.agentId ?? deps.defaultAgentId;
    if (!agentId) return;

    try {
      const sessionFile = await findActiveSessionFile(agentId);
      if (!sessionFile) {
        return;
      }

      const lines = await readJsonlLines(sessionFile);
      const asstNodes = lines.filter(
        (e) => e?.message?.role === "assistant" && typeof e.id === "string",
      ) as Array<Record<string, unknown> & { id: string }>;

      const nowSec = Math.floor(Date.now() / 1000);
      let watermarkTs = watermarkByAgent.get(agentId);
      if (watermarkTs === undefined) {
        watermarkTs = nowSec - BACKFILL_WINDOW_SEC;
      }

      const newAsst = asstNodes.filter((n) => {
        const ts = parseAssistantTimestamp(n);
        // Skip entries without a parseable timestamp — without a ts we
        // can't tell if they're new, and forwarding unconditionally
        // would re-send on every call.
        return typeof ts === "number" && ts > watermarkTs!;
      });
      if (newAsst.length === 0) {
        watermarkByAgent.set(agentId, watermarkTs);
        return;
      }

      let maxTs = watermarkTs;
      for (const asstNode of newAsst) {
        const text = extractText(
          (asstNode as { message?: { content?: unknown } }).message?.content,
        );
        if (!text) continue;

        // event_id: "oc_asst:<asst_node_id>:<text_hash_16>". Node id is
        // stable within a session; the text hash guards against the
        // unlikely case of id reuse across session files.
        const textHash = createHash("sha256").update(text).digest("hex").slice(0, 16);
        const eventId = `oc_asst:${asstNode.id}:${textHash}`;

        if (seen.has(eventId)) continue;
        if (seen.size >= SEEN_MAX) seen.clear();
        seen.add(eventId);

        const ts = parseAssistantTimestamp(asstNode) ?? nowSec;
        if (ts > maxTs) maxTs = ts;

        deps.bridge.forward({
          content: text,
          from: "agent",
          conversationId: ctx.conversationId,
          channelId: ctx.channelId,
          timestamp: ts,
          role: "assistant",
          channel: undefined,
          threadId: ctx.conversationId ?? ctx.channelId,
          messageId: eventId,
        });
      }

      watermarkByAgent.set(agentId, maxTs);
    } catch (err) {
      deps.logger.error(
        `clawvisor outbound-scavenger: ${(err as Error).message}`,
      );
    }
  };
}

function extractText(content: unknown): string {
  if (!content) return "";
  if (typeof content === "string") return content;
  if (Array.isArray(content)) {
    return content
      .filter((c: unknown) => (c as { type?: string })?.type === "text")
      .map((c: unknown) => (c as { text?: string })?.text ?? "")
      .join("\n")
      .trim();
  }
  return "";
}

async function readJsonlLines(filePath: string): Promise<Array<Record<string, unknown>>> {
  try {
    const raw = await fs.readFile(filePath, "utf8");
    return raw
      .split("\n")
      .filter(Boolean)
      .map((l) => {
        try {
          return JSON.parse(l);
        } catch {
          return null;
        }
      })
      .filter((v): v is Record<string, unknown> => v !== null);
  } catch {
    return [];
  }
}

async function findActiveSessionFile(agentId: string): Promise<string | null> {
  // Honor CLAWVISOR_SESSIONS_DIR for tests and unusual deployments;
  // default path mirrors what OpenClaw writes at runtime.
  const override = process.env.CLAWVISOR_SESSIONS_DIR;
  const dir = override
    ? resolvePath(override, agentId, "sessions")
    : resolvePath(homedir(), ".openclaw", "agents", agentId, "sessions");

  let entries: string[];
  try {
    entries = await fs.readdir(dir);
  } catch {
    return null;
  }
  const jsonls = entries.filter((f) => f.endsWith(".jsonl"));
  if (jsonls.length === 0) return null;

  // Pick the most recently modified — if the gateway has one session
  // open per channel concurrently, this picks whichever the current
  // user message just landed in. Not foolproof (two sessions could
  // race), but correct for the common single-active-session case.
  const stats = await Promise.all(
    jsonls.map(async (f) => {
      const full = join(dir, f);
      try {
        const s = await fs.stat(full);
        return { file: full, mtime: s.mtimeMs };
      } catch {
        return null;
      }
    }),
  );
  const valid = stats.filter((s): s is { file: string; mtime: number } => s !== null);
  if (valid.length === 0) return null;
  valid.sort((a, b) => b.mtime - a.mtime);
  return valid[0]!.file;
}

function parseAssistantTimestamp(entry: unknown): number | undefined {
  if (!entry || typeof entry !== "object") return undefined;
  const e = entry as { timestamp?: unknown; message?: { timestamp?: unknown } };
  const raw = e.timestamp ?? e.message?.timestamp;
  if (typeof raw === "number" && raw > 0) {
    // Heuristic: if it's in milliseconds (>= 10^12), convert to seconds
    return raw >= 1e12 ? Math.floor(raw / 1000) : raw;
  }
  if (typeof raw === "string") {
    const parsed = Date.parse(raw);
    if (!isNaN(parsed)) return Math.floor(parsed / 1000);
  }
  return undefined;
}
