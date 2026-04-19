import { definePluginEntry, type OpenClawPluginApi } from "./api.js";
import { BridgeClient, ClawvisorClient } from "./src/clawvisor-client.js";
import { MessageBridge } from "./src/message-bridge.js";
import { createScavenger, type ScavengeTrigger } from "./src/outbound-scavenger.js";
import { pairPlugin } from "./src/pair.js";
import {
  defaultSecretsPath,
  readSecrets,
  writeSecrets,
  type PluginSecrets,
} from "./src/secrets.js";
import type { ClawvisorPluginConfig } from "./src/types.js";
import { registerClawvisorTools } from "./src/tools.js";

// Duck-type the OpenClaw SDK method used for lifecycle-safe async init.
// Not all SDK versions advertise it on the public type; if it's missing
// we fall back to a fire-and-forget init, with the same hook + tool
// registration done synchronously so the register-window never closes
// on us.
type ServiceApi = OpenClawPluginApi & {
  registerService?: (svc: {
    id: string;
    start: () => Promise<void> | void;
    stop?: () => Promise<void> | void;
  }) => void;
};

/**
 * OpenClaw closes the `register` window as soon as the function returns —
 * hooks (`message_received`, `message_sent`, `before_tool_call`, …) and
 * tools must be attached **synchronously** during register, before any
 * await. Any await inside the async body means the hook/tool api.on /
 * api.registerTool calls happen after the window has closed and are
 * silently dropped.
 *
 * Shape:
 *   - register(api):
 *       - parse config, bail early on disabled / missing url
 *       - declare mutable state (clients, bridge, lastConversation…)
 *       - attach all hooks + tools against that state (no awaits)
 *       - registerService("clawvisor-init") — async boot fills the state in
 *
 *   Hooks early-return if the state hasn't been populated yet (e.g. a
 *   message arrives before pairing finishes). This trades a small window
 *   of dropped events for never getting into the "hooks aren't wired"
 *   failure mode that was causing silent no-forwarding in v1.
 */
export default definePluginEntry({
  id: "clawvisor",
  name: "Clawvisor",
  description:
    "Connect OpenClaw agents to a Clawvisor authorization gateway for credential vaulting, task-scoped auth, and auto-approval.",

  register(apiRaw: OpenClawPluginApi) {
    const api = apiRaw as ServiceApi;
    const config = (api.pluginConfig ?? {}) as ClawvisorPluginConfig;

    if (config.enabled === false) {
      api.logger.info("clawvisor plugin disabled by config");
      return;
    }
    if (!config.url) {
      api.logger.warn("clawvisor: missing `url` in plugin config — plugin not registered");
      return;
    }

    // Mutable state populated by the async boot; hooks + tools close over it.
    const clients = new Map<string, ClawvisorClient>();
    let defaultClient: ClawvisorClient | null = null;
    let bridge: MessageBridge | null = null;
    let bridgeToken: string | undefined;
    // Set once asyncInit builds the scavenger (which needs `bridge`).
    // The `message_received` hook calls this after forwarding the user
    // message, so each user turn also captures the previous assistant turn.
    let scavenge: ScavengeTrigger | null = null;
    // Safe to maintain unconditionally — written only from within the
    // synchronously-registered before_tool_call / after_tool_call hooks.
    const toolCallAgentMap = new Map<string, string>();
    const lastConversationForAgent = new Map<string, string>();

    // ─── synchronous hook registration ─────────────────────────────────
    // All four `api.on` calls happen before any await so OpenClaw sees
    // them inside the register window.

    api.on("before_tool_call", (_event, ctx) => {
      if (ctx.toolCallId && ctx.agentId) {
        toolCallAgentMap.set(ctx.toolCallId, ctx.agentId);
      }
    });
    api.on("after_tool_call", (_event, ctx) => {
      if (ctx.toolCallId) {
        toolCallAgentMap.delete(ctx.toolCallId);
      }
    });

    api.on("message_received", async (event, ctx) => {
      const convo = ctx.conversationId ?? ctx.channelId;
      if (convo && ctx.agentId) {
        lastConversationForAgent.set(ctx.agentId, convo);
      }
      if (!bridge) return; // not paired yet — drop rather than crash
      // Scavenge the previous assistant reply from the session JSONL first,
      // so it gets a lower seq than the incoming user message. This keeps
      // the buffer in conversational order (asst#k-1 → user#k → asst#k).
      // OpenClaw doesn't fire message_sent for non-channel plugins, so each
      // user turn is where we capture the prior agent turn. Server dedupes
      // via stable event_id so multiple scans are idempotent.
      if (scavenge) {
        try {
          await scavenge({
            agentId: ctx.agentId,
            conversationId: ctx.conversationId,
            channelId: ctx.channelId,
          });
        } catch (err) {
          api.logger.warn(`clawvisor: scavenge failed: ${(err as Error).message}`);
        }
      }
      bridge.forward({
        content: event.content,
        from: event.from,
        channelId: ctx.channelId,
        conversationId: ctx.conversationId,
        timestamp: event.timestamp,
        role: "user",
        channel: ctx.channel,
        threadId: ctx.threadId,
        messageId: event.messageId,
      });
    });

    api.on("message_sent", (event, ctx) => {
      const convo = ctx.conversationId ?? ctx.channelId;
      if (convo && ctx.agentId) {
        lastConversationForAgent.set(ctx.agentId, convo);
      }
      if (!event.success) return;
      if (!bridge) return; // not paired yet
      bridge.forward({
        content: event.content,
        from: "agent",
        channelId: ctx.channelId,
        conversationId: ctx.conversationId,
        role: "assistant",
        channel: ctx.channel,
        threadId: ctx.threadId,
        messageId: event.messageId,
      });
    });

    // ─── synchronous tool registration ─────────────────────────────────
    // Resolvers close over the mutable state. If a tool is called before
    // the async boot finishes populating clients, the resolver throws a
    // clear error (caller gets a tool-failure, not a silent no-op).

    function clientForToolCall(toolCallId: string): ClawvisorClient {
      if (!defaultClient) {
        throw new Error(
          "clawvisor: plugin is still pairing — try again in a moment (no agent tokens yet)",
        );
      }
      const agentId = toolCallAgentMap.get(toolCallId);
      if (agentId) {
        const c = clients.get(agentId);
        if (c) return c;
      }
      return defaultClient;
    }

    function conversationForToolCall(toolCallId: string): string | undefined {
      const agentId = toolCallAgentMap.get(toolCallId);
      if (!agentId) return undefined;
      return lastConversationForAgent.get(agentId);
    }

    const attestationResolver = () => bridgeToken;
    registerClawvisorTools(api, clientForToolCall, conversationForToolCall, attestationResolver);

    // ─── async boot ────────────────────────────────────────────────────
    // We kick asyncInit via `Promise.resolve().then(...)` so it runs
    // immediately after `register` returns. `registerService` exists on
    // the SDK but empirically its `start` callback fires 10–20 seconds
    // after register — long enough that message hooks wired in register
    // can fire with a null bridge and drop events. Using a microtask
    // gets asyncInit running within ms. The `initialized` guard means
    // if `registerService.start` ALSO calls asyncInit later, it's a
    // cheap no-op.

    let initialized = false;
    const asyncInit = async () => {
      if (initialized) return;
      initialized = true;
      const secretsPath = defaultSecretsPath();
      let secrets: PluginSecrets;
      try {
        secrets = await readSecrets(secretsPath);
      } catch (err) {
        api.logger.error(
          `clawvisor: failed to read secrets file at ${secretsPath}: ${(err as Error).message}. Refusing to pair — fix file perms or remove the file to re-pair.`,
        );
        return;
      }

      // Incomplete-pair detection: bridgeToken present but agentTokens
      // empty (pair fired before `agents` was in config). Treat as
      // unconfigured so a fresh pairCode can redeem cleanly.
      const tokensComplete =
        !!secrets.bridgeToken &&
        secrets.agentTokens !== undefined &&
        Object.keys(secrets.agentTokens).length > 0;

      if (!tokensComplete) {
        if (
          secrets.bridgeToken &&
          (!secrets.agentTokens || Object.keys(secrets.agentTokens).length === 0)
        ) {
          api.logger.warn(
            'clawvisor: previous pair left no agent tokens (likely because `agents` was empty in config at pair time). Mint a new pair code, ensure `agents: ["main"]` is set, and reload — the existing bridgeToken will be replaced.',
          );
        }
        if (!config.pairCode) {
          api.logger.warn(
            "clawvisor: not yet configured. Paste /skill/setup-openclaw from the dashboard into your agent to deposit a pair code.",
          );
          return;
        }
        try {
          const outcome = await pairPlugin(
            {
              url: config.url!,
              pairCode: config.pairCode,
              agentIds: config.agents ?? [],
              installFingerprint: secrets.installFingerprint,
            },
            { logger: api.logger },
          );
          secrets = {
            bridgeToken: outcome.bridgeToken,
            agentTokens: outcome.agents,
            installFingerprint: outcome.installFingerprint,
          };
          try {
            await writeSecrets(secretsPath, secrets);
            api.logger.info(`clawvisor: secrets persisted to ${secretsPath} (0600)`);
          } catch (err) {
            api.logger.error(
              `clawvisor: paired successfully but could not write secrets file at ${secretsPath}: ${(err as Error).message}. Not registering — fix the path/perms and retry.`,
            );
            return;
          }
        } catch (err) {
          api.logger.error(`clawvisor: pair failed — ${(err as Error).message}`);
          return;
        }
      }

      const agentTokens = secrets.agentTokens ?? {};
      const agentIds = Object.keys(agentTokens);
      if (agentIds.length === 0) {
        api.logger.warn(
          "clawvisor: pair succeeded but no agent tokens were minted — check the agents array in plugin config",
        );
        return;
      }

      for (const [agentId, token] of Object.entries(agentTokens)) {
        clients.set(agentId, new ClawvisorClient(config.url!, token));
      }
      defaultClient = clients.get("main") ?? clients.get(agentIds[0]!)!;

      const healthy = await defaultClient.isHealthy();
      if (!healthy) {
        api.logger.error(`clawvisor: cannot reach ${config.url}/health — hooks will stay in drop mode`);
        // Don't null out state — leave tools usable; the user might want
        // to call clawvisor_fetch_catalog and see the underlying error.
      }

      // Flip on message forwarding — hooks reference these via closure
      // and will start actually forwarding immediately.
      bridgeToken = secrets.bridgeToken;
      bridge = new MessageBridge({
        client: new BridgeClient(config.url!, secrets.bridgeToken!),
        groupChatId: config.groupChatId ?? "openclaw-default",
        logger: api.logger,
      });
      // Build the outbound scavenger. Default agent id = first configured
      // agent (usually "main") for the case when ctx.agentId isn't
      // populated on the message_received hook.
      scavenge = createScavenger({
        bridge,
        logger: api.logger,
        defaultAgentId: agentIds[0] ?? "main",
      });

      api.logger.info(
        `clawvisor plugin ready: ${clients.size} agent(s) connected to ${config.url}`,
      );
    };

    // Fire-and-forget on a microtask — always, regardless of whether
    // the SDK exposes registerService. See the comment on asyncInit for
    // why: registerService.start runs too late to cover messages that
    // arrive in the first 10+ seconds after register. If the SDK does
    // also call asyncInit via registerService, the `initialized` guard
    // makes it a no-op.
    void Promise.resolve().then(asyncInit).catch((err) => {
      api.logger.error(`clawvisor: async init failed: ${(err as Error).message}`);
    });
    if (typeof api.registerService === "function") {
      api.registerService({
        id: "clawvisor-init",
        start: asyncInit,
      });
    }
  },
});
