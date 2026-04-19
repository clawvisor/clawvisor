import { test } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";

import { BridgeClient, ClawvisorClient, requestPair } from "../src/clawvisor-client.js";
import { MessageBridge } from "../src/message-bridge.js";
import { registerClawvisorTools } from "../src/tools.js";

interface CapturedRequest {
  method: string;
  path: string;
  auth?: string;
  bridgeAttestation?: string;
  idempotencyKey?: string;
  body: unknown;
}

async function startMockServer(
  onRequest: (req: CapturedRequest) => { status: number; body?: unknown } | undefined,
): Promise<{ url: string; close: () => Promise<void>; requests: CapturedRequest[] }> {
  const requests: CapturedRequest[] = [];

  const server = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      const raw = Buffer.concat(chunks).toString("utf8");
      let body: unknown = undefined;
      if (raw) {
        try {
          body = JSON.parse(raw);
        } catch {
          body = raw;
        }
      }
      const captured: CapturedRequest = {
        method: req.method ?? "",
        path: req.url ?? "",
        auth: req.headers["authorization"] as string | undefined,
        bridgeAttestation: req.headers["x-clawvisor-bridge-attestation"] as string | undefined,
        idempotencyKey: req.headers["idempotency-key"] as string | undefined,
        body,
      };
      requests.push(captured);

      const handled = onRequest(captured);
      const status = handled?.status ?? 200;
      res.writeHead(status, { "Content-Type": "application/json" });
      res.end(JSON.stringify(handled?.body ?? { ok: true }));
    });
  });

  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${addr.port}`,
    requests,
    close: () =>
      new Promise<void>((resolve, reject) =>
        server.close((err) => (err ? reject(err) : resolve())),
      ),
  };
}

// End-to-end contract: MessageBridge authenticates with a *bridge* token
// (cvisbr_...) — never an agent token — and clawvisor_create_task uses an
// agent token. Same conversationId flows through both sides so auto-approval
// can consult the forwarded buffer.
test("bridge client forwards messages; agent client creates tasks; tokens stay distinct", async () => {
  const server = await startMockServer((req) => {
    if (req.path === "/api/tasks") {
      return { status: 201, body: { task_id: "task-xyz", status: "active" } };
    }
    return undefined;
  });

  try {
    const bridgeClient = new BridgeClient(server.url, "cvisbr_bridge_secret");
    const agentClient = new ClawvisorClient(server.url, "cvis_agent_secret");

    const bridge = new MessageBridge({
      client: bridgeClient,
      groupChatId: "openclaw-default",
      logger: { warn: () => {} },
    });

    const conversationId = "slack:C0123/thread:ABC";

    bridge.forward({
      content: "please summarize my inbox",
      from: "user:alice",
      conversationId,
      timestamp: 1_700_000_000,
    });
    bridge.forward({
      content: "on it",
      from: "agent",
      conversationId,
      timestamp: 1_700_000_001,
    });

    await new Promise((resolve) => setTimeout(resolve, 50));

    await agentClient.createTask(
      {
        purpose: "summarize inbox",
        authorized_actions: [{ service: "google.gmail", action: "list_messages" }],
        group_chat_id: conversationId,
      },
      { bridgeAttestation: "cvisbr_bridge_secret" },
    );

    const ingests = server.requests.filter((r) => r.path === "/api/buffer/ingest");
    assert.equal(ingests.length, 2, "should have forwarded both messages");
    for (const r of ingests) {
      assert.equal(r.method, "POST");
      assert.equal(
        r.auth,
        "Bearer cvisbr_bridge_secret",
        "ingest must authenticate with the bridge token, never with an agent token",
      );
      const body = r.body as { group_chat_id?: string };
      assert.equal(body.group_chat_id, conversationId);
    }

    const tasks = server.requests.filter((r) => r.path === "/api/tasks");
    assert.equal(tasks.length, 1);
    assert.equal(
      tasks[0].auth,
      "Bearer cvis_agent_secret",
      "task creation must authenticate with the agent token, never the bridge token",
    );
    const taskBody = tasks[0].body as { group_chat_id?: string; purpose?: string };
    assert.equal(taskBody.group_chat_id, conversationId);
    assert.equal(taskBody.purpose, "summarize inbox");
    assert.equal(
      tasks[0].bridgeAttestation,
      "Bearer cvisbr_bridge_secret",
      "task creation must carry the bridge attestation header — server rejects agent-supplied group_chat_id without it",
    );
  } finally {
    await server.close();
  }
});

test("MessageBridge drops empty-content events", async () => {
  const server = await startMockServer(() => undefined);
  try {
    const client = new BridgeClient(server.url, "cvisbr_token");
    const bridge = new MessageBridge({
      client,
      groupChatId: "fallback",
      logger: { warn: () => {} },
    });

    bridge.forward({ content: "", conversationId: "c1" });
    await new Promise((resolve) => setTimeout(resolve, 30));
    assert.equal(
      server.requests.filter((r) => r.path === "/api/buffer/ingest").length,
      0,
      "empty-content forward should not hit the wire",
    );
  } finally {
    await server.close();
  }
});

test("MessageBridge falls back conversationId → channelId → groupChatId", async () => {
  const server = await startMockServer(() => undefined);
  try {
    const client = new BridgeClient(server.url, "cvisbr_token");
    const bridge = new MessageBridge({
      client,
      groupChatId: "fallback-gid",
      logger: { warn: () => {} },
    });

    bridge.forward({ content: "a", channelId: "chan-1" });
    bridge.forward({ content: "b" });
    bridge.forward({ content: "c", conversationId: "conv-1", channelId: "chan-1" });

    await new Promise((resolve) => setTimeout(resolve, 50));
    const ingests = server.requests.filter((r) => r.path === "/api/buffer/ingest");
    assert.equal(ingests.length, 3);
    const keys = ingests.map((r) => (r.body as { group_chat_id: string }).group_chat_id);
    assert.deepEqual(keys, ["chan-1", "fallback-gid", "conv-1"]);
  } finally {
    await server.close();
  }
});

// Regression: if the plugin's conversation resolver returns undefined for
// a given toolCallId (e.g. because the calling agent has no bound
// conversation yet), clawvisor_create_task must NOT inject a group_chat_id
// and must NOT send the bridge attestation header. Otherwise agent B
// could silently inherit agent A's last observed conversation and have
// the server attest for a chat agent B was never in.
test("clawvisor_create_task omits group_chat_id + attestation when no agent-scoped conversation", async () => {
  type RegisteredTool = {
    execute: (toolCallId: string, params: Record<string, unknown>) => Promise<unknown>;
  };
  const tools = new Map<string, RegisteredTool>();
  const fakeApi = {
    registerTool(def: unknown, _opts: unknown) {
      const d = def as { name: string; execute: RegisteredTool["execute"] };
      tools.set(d.name, { execute: d.execute });
    },
  } as unknown as Parameters<typeof registerClawvisorTools>[0];

  const server = await startMockServer((req) => {
    if (req.path === "/api/tasks") {
      return { status: 201, body: { task_id: "task-1", status: "pending_approval" } };
    }
    return undefined;
  });

  try {
    const agentClient = new ClawvisorClient(server.url, "cvis_agent_b");
    registerClawvisorTools(
      fakeApi,
      () => agentClient,
      () => undefined, // resolver returns nothing: agent B has no bound conversation
      () => "cvisbr_bridge_secret",
    );

    const tool = tools.get("clawvisor_create_task");
    assert.ok(tool, "clawvisor_create_task should have registered");
    await tool!.execute("tool-call-B", {
      purpose: "delete something",
      authorized_actions: [{ service: "google.drive", action: "delete_file" }],
      // Agent tries to sneak in a group_chat_id via unschematized params.
      group_chat_id: "conv-from-agent-A",
    });

    const tasks = server.requests.filter((r) => r.path === "/api/tasks");
    assert.equal(tasks.length, 1);
    const body = tasks[0].body as Record<string, unknown>;
    assert.equal(
      body.group_chat_id,
      undefined,
      "no agent-scoped conversation → group_chat_id must not be injected, and any agent-supplied value must be stripped",
    );
    assert.equal(
      tasks[0].bridgeAttestation,
      undefined,
      "attestation header must not be sent when no group_chat_id is attested",
    );
  } finally {
    await server.close();
  }
});

test("requestPair posts fingerprint+hostname+agents and returns minted tokens", async () => {
  const server = await startMockServer((req) => {
    if (req.path === "/api/plugin/pair?wait=true") {
      return {
        status: 200,
        body: {
          pair_id: "pair-abc",
          status: "approved",
          bridge_token: "cvisbr_freshly_minted",
          agents: {
            main: "cvis_main_token",
            researcher: "cvis_researcher_token",
          },
        },
      };
    }
    return undefined;
  });
  try {
    const resp = await requestPair(server.url, {
      pair_code: "pair_code_abc123",
      install_fingerprint: "fp_laptop_xyz",
      hostname: "alice-laptop",
      idempotency_key: "idem-test-1",
      agent_ids: ["main", "researcher"],
    });
    assert.equal(resp.status, "approved");
    assert.equal(resp.bridge_token, "cvisbr_freshly_minted");
    assert.deepEqual(resp.agents, {
      main: "cvis_main_token",
      researcher: "cvis_researcher_token",
    });

    const pairCall = server.requests.find((r) => r.path === "/api/plugin/pair?wait=true");
    assert.ok(pairCall, "plugin should have hit /api/plugin/pair?wait=true");
    assert.equal(pairCall.method, "POST");
    assert.equal(
      pairCall.auth,
      undefined,
      "pair request is unauthenticated — capability comes from the one-time pair_code, not a bearer",
    );
    assert.equal(
      pairCall.idempotencyKey,
      "idem-test-1",
      "Idempotency-Key header must flow through so long-poll retries collapse onto one pending pair",
    );
    const body = pairCall.body as {
      pair_code: string;
      install_fingerprint: string;
      hostname: string;
      agent_ids: string[];
      idempotency_key: string;
    };
    assert.equal(body.pair_code, "pair_code_abc123");
    assert.equal(body.install_fingerprint, "fp_laptop_xyz");
    assert.equal(body.hostname, "alice-laptop");
    assert.deepEqual(body.agent_ids, ["main", "researcher"]);
    assert.equal(body.idempotency_key, "idem-test-1");
  } finally {
    await server.close();
  }
});
