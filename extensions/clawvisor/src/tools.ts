import { Type } from "@sinclair/typebox";
import type { OpenClawPluginApi } from "../api.js";
import type { ClawvisorClient } from "./clawvisor-client.js";

export type ClientResolver = (toolCallId: string) => ClawvisorClient;

/**
 * Returns the conversation ID the agent is operating in *right now*, as the
 * plugin runtime sees it. Injected into clawvisor_create_task on the agent's
 * behalf — the agent never sets it, so a prompt-injection attack cannot
 * redirect approval to a different conversation.
 */
export type ConversationResolver = (toolCallId: string) => string | undefined;

/**
 * Returns the plugin's bridge token for attaching as an attestation header
 * on task creates. The server only trusts an injected group_chat_id when
 * the attestation is present and references a bridge that belongs to the
 * same user as the agent token. Held by the plugin runtime, never passed
 * through the agent's params.
 */
export type BridgeAttestationResolver = () => string | undefined;

function textResult(data: unknown) {
  return {
    content: [{ type: "text" as const, text: JSON.stringify(data, null, 2) }],
    details: data,
  };
}

/**
 * Registers the 8 Clawvisor agent tools. Each tool proxies to a corresponding
 * Clawvisor HTTP endpoint using the ClawvisorClient resolved from the active
 * agent for that invocation (see `resolveClient`).
 */
export function registerClawvisorTools(
  api: OpenClawPluginApi,
  resolveClient: ClientResolver,
  resolveConversation: ConversationResolver,
  resolveBridgeAttestation: BridgeAttestationResolver,
): void {
  api.registerTool(
    {
      name: "clawvisor_fetch_catalog",
      label: "Clawvisor Catalog",
      description:
        "Fetch the Clawvisor service catalog. Returns available services, actions, and restrictions.",
      parameters: Type.Object({
        service: Type.Optional(
          Type.String({ description: "Optional service ID for detailed docs." }),
        ),
      }),
      async execute(toolCallId, params) {
        return textResult(await resolveClient(toolCallId).fetchCatalog(params));
      },
    },
    { name: "clawvisor_fetch_catalog" },
  );

  api.registerTool(
    {
      name: "clawvisor_create_task",
      label: "Clawvisor Create Task",
      description:
        "Create a new Clawvisor task for scoped authorization. Use wait=true to block until approved.",
      parameters: Type.Object({
        purpose: Type.String({ description: "What this task will do." }),
        authorized_actions: Type.Array(
          Type.Object({
            service: Type.String(),
            action: Type.String(),
            auto_execute: Type.Optional(Type.Boolean()),
            expected_use: Type.Optional(Type.String()),
          }),
        ),
        planned_calls: Type.Optional(
          Type.Array(
            Type.Object({
              service: Type.String(),
              action: Type.String(),
              reason: Type.String(),
              params: Type.Optional(Type.Record(Type.String(), Type.Unknown())),
            }),
          ),
        ),
        lifetime: Type.Optional(Type.String()),
        expires_in_seconds: Type.Optional(Type.Number()),
        wait: Type.Optional(Type.Boolean()),
        timeout: Type.Optional(Type.Number()),
      }),
      async execute(toolCallId, params) {
        // The plugin injects group_chat_id ONLY when it has a conversation
        // bound to the specific agent making this tool call. When no such
        // binding exists, we omit both group_chat_id and the attestation
        // header — the task falls back to manual approval rather than risk
        // attesting for a conversation the calling agent wasn't actually
        // in. The schema hides group_chat_id from the agent; we also strip
        // any sneaked-in value before rebuilding the body so an unsanitary
        // runtime can't forward it.
        const { group_chat_id: _ignored, ...rest } = params as Record<string, unknown>;
        const body: Record<string, unknown> = { ...rest };
        const convo = resolveConversation(toolCallId);
        const opts: { bridgeAttestation?: string } = {};
        if (convo) {
          body.group_chat_id = convo;
          // Only send the attestation when we're actually attesting for a
          // group_chat_id — the header has no meaning without one.
          const attestation = resolveBridgeAttestation();
          if (attestation) opts.bridgeAttestation = attestation;
        }
        return textResult(await resolveClient(toolCallId).createTask(body, opts));
      },
    },
    { name: "clawvisor_create_task" },
  );

  api.registerTool(
    {
      name: "clawvisor_get_task",
      label: "Clawvisor Get Task",
      description:
        "Get the current status and details of a Clawvisor task. Use wait=true to long-poll.",
      parameters: Type.Object({
        task_id: Type.String({ description: "The task ID to look up." }),
        wait: Type.Optional(Type.Boolean()),
        timeout: Type.Optional(Type.Number()),
      }),
      async execute(toolCallId, params) {
        const { task_id, wait, timeout } = params as {
          task_id: string;
          wait?: boolean;
          timeout?: number;
        };
        return textResult(await resolveClient(toolCallId).getTask(task_id, { wait, timeout }));
      },
    },
    { name: "clawvisor_get_task" },
  );

  api.registerTool(
    {
      name: "clawvisor_complete_task",
      label: "Clawvisor Complete Task",
      description: "Mark a Clawvisor task as completed.",
      parameters: Type.Object({
        task_id: Type.String({ description: "The task ID to complete." }),
      }),
      async execute(toolCallId, params) {
        const { task_id } = params as { task_id: string };
        return textResult(await resolveClient(toolCallId).completeTask(task_id));
      },
    },
    { name: "clawvisor_complete_task" },
  );

  api.registerTool(
    {
      name: "clawvisor_expand_task",
      label: "Clawvisor Expand Task",
      description: "Request adding a new action to an existing Clawvisor task scope.",
      parameters: Type.Object({
        task_id: Type.String(),
        service: Type.String(),
        action: Type.String(),
        reason: Type.String(),
        auto_execute: Type.Optional(Type.Boolean()),
        wait: Type.Optional(Type.Boolean()),
        timeout: Type.Optional(Type.Number()),
      }),
      async execute(toolCallId, params) {
        const { task_id, ...body } = params as { task_id: string } & Record<string, unknown>;
        return textResult(await resolveClient(toolCallId).expandTask(task_id, body));
      },
    },
    { name: "clawvisor_expand_task" },
  );

  api.registerTool(
    {
      name: "clawvisor_gateway_request",
      label: "Clawvisor Gateway Request",
      description:
        "Execute a service action through the Clawvisor gateway. Requires an active task.",
      parameters: Type.Object({
        service: Type.String({ description: "Service ID." }),
        action: Type.String({ description: "Action to perform." }),
        params: Type.Record(Type.String(), Type.Unknown(), {
          description: "Action-specific parameters.",
        }),
        reason: Type.String({ description: "Why this action is being performed." }),
        request_id: Type.String({ description: "Unique request ID." }),
        task_id: Type.String({ description: "Task ID authorizing this request." }),
        session_id: Type.Optional(Type.String()),
        context: Type.Optional(Type.Record(Type.String(), Type.Unknown())),
        wait: Type.Optional(Type.Boolean()),
        timeout: Type.Optional(Type.Number()),
      }),
      async execute(toolCallId, params) {
        return textResult(await resolveClient(toolCallId).gatewayRequest(params));
      },
    },
    { name: "clawvisor_gateway_request" },
  );

  api.registerTool(
    {
      name: "clawvisor_execute_request",
      label: "Clawvisor Execute Request",
      description: "Retry or long-poll a previously pending Clawvisor gateway request.",
      parameters: Type.Object({
        request_id: Type.String({
          description: "The request ID from the original gateway_request.",
        }),
        wait: Type.Optional(Type.Boolean()),
        timeout: Type.Optional(Type.Number()),
      }),
      async execute(toolCallId, params) {
        const { request_id, ...rest } = params as { request_id: string } & Record<string, unknown>;
        return textResult(await resolveClient(toolCallId).executeRequest(request_id, rest));
      },
    },
    { name: "clawvisor_execute_request" },
  );

  api.registerTool(
    {
      name: "clawvisor_report_bug",
      label: "Clawvisor Report Bug",
      description: "Report a bug or issue with a Clawvisor decision.",
      parameters: Type.Object({
        description: Type.String({ description: "Describe what happened." }),
        request_id: Type.Optional(Type.String()),
        task_id: Type.Optional(Type.String()),
        context: Type.Optional(Type.Record(Type.String(), Type.Unknown())),
      }),
      async execute(toolCallId, params) {
        return textResult(await resolveClient(toolCallId).reportBug(params));
      },
    },
    { name: "clawvisor_report_bug" },
  );
}
