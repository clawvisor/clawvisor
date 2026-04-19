import { test } from "node:test";
import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import type { MessageBridge } from "../src/message-bridge.js";
import { createScavenger } from "../src/outbound-scavenger.js";

// Record-only spy that matches the subset of MessageBridge the scavenger
// actually calls. We use `as unknown as MessageBridge` to satisfy the
// type signature without dragging in the real BridgeClient.
function spyBridge() {
  const calls: Array<Record<string, unknown>> = [];
  const bridge = {
    forward(ev: Record<string, unknown>) {
      calls.push(ev);
    },
  };
  return { bridge: bridge as unknown as MessageBridge, calls };
}

const silentLogger = { info: () => {}, warn: () => {}, error: () => {} };

// Helper — assistant entries need a parseable timestamp for the watermark
// filter to treat them as "new". Synthesize one relative to now (seconds).
function nowSec() {
  return Math.floor(Date.now() / 1000);
}

async function seedSession(
  baseDir: string,
  agentId: string,
  entries: Array<Record<string, unknown>>,
) {
  const sessionsDir = join(baseDir, agentId, "sessions");
  await fs.mkdir(sessionsDir, { recursive: true });
  const file = join(sessionsDir, "session-1.jsonl");
  await fs.writeFile(file, entries.map((e) => JSON.stringify(e)).join("\n") + "\n");
  return file;
}

test("scavenger forwards asst with timestamps inside the backfill window", async () => {
  const root = await fs.mkdtemp(join(tmpdir(), "scav-"));
  const originalEnv = process.env.CLAWVISOR_SESSIONS_DIR;
  process.env.CLAWVISOR_SESSIONS_DIR = root;
  try {
    const recentTs = nowSec() - 60;
    await seedSession(root, "main", [
      { id: "u1", message: { role: "user", content: "hi" } },
      { id: "a1", timestamp: recentTs, message: { role: "assistant", content: "hello there" } },
      { id: "a1b", timestamp: recentTs + 1, message: { role: "assistant", content: [{ type: "text", text: "how can I help" }] } },
      { id: "u2", message: { role: "user", content: "list my emails" } },
    ]);
    const { bridge, calls } = spyBridge();
    const scavenge = createScavenger({ bridge, logger: silentLogger, defaultAgentId: "main" });
    await scavenge({ agentId: "main", conversationId: "webchat" });

    assert.equal(calls.length, 2, "both recent asst nodes should be forwarded");
    assert.match(String(calls[0]!.content), /hello there/);
    assert.match(String(calls[1]!.content), /how can I help/);
    assert.match(String(calls[0]!.messageId), /^oc_asst:a1:/);
    assert.match(String(calls[1]!.messageId), /^oc_asst:a1b:/);
  } finally {
    process.env.CLAWVISOR_SESSIONS_DIR = originalEnv;
    await fs.rm(root, { recursive: true, force: true });
  }
});

test("scavenger skips asst older than the 15-minute backfill window", async () => {
  const root = await fs.mkdtemp(join(tmpdir(), "scav-"));
  const originalEnv = process.env.CLAWVISOR_SESSIONS_DIR;
  process.env.CLAWVISOR_SESSIONS_DIR = root;
  try {
    const staleTs = nowSec() - 60 * 60; // 1 hour ago
    await seedSession(root, "main", [
      { id: "u1", message: { role: "user", content: "old" } },
      { id: "a1", timestamp: staleTs, message: { role: "assistant", content: "stale reply" } },
      { id: "u2", message: { role: "user", content: "again" } },
    ]);
    const { bridge, calls } = spyBridge();
    const scavenge = createScavenger({ bridge, logger: silentLogger, defaultAgentId: "main" });
    await scavenge({ agentId: "main" });
    assert.equal(calls.length, 0, "asst outside the backfill window must not be forwarded");
  } finally {
    process.env.CLAWVISOR_SESSIONS_DIR = originalEnv;
    await fs.rm(root, { recursive: true, force: true });
  }
});

test("scavenger is idempotent — repeat calls with unchanged JSONL do not re-forward", async () => {
  const root = await fs.mkdtemp(join(tmpdir(), "scav-"));
  const originalEnv = process.env.CLAWVISOR_SESSIONS_DIR;
  process.env.CLAWVISOR_SESSIONS_DIR = root;
  try {
    const recentTs = nowSec() - 60;
    await seedSession(root, "main", [
      { id: "u1", message: { role: "user", content: "hi" } },
      { id: "a1", timestamp: recentTs, message: { role: "assistant", content: "hello" } },
      { id: "u2", message: { role: "user", content: "again" } },
    ]);
    const { bridge, calls } = spyBridge();
    const scavenge = createScavenger({ bridge, logger: silentLogger, defaultAgentId: "main" });
    await scavenge({ agentId: "main" });
    await scavenge({ agentId: "main" });
    await scavenge({ agentId: "main" });
    assert.equal(calls.length, 1, "repeat calls must not re-forward the same asst");
  } finally {
    process.env.CLAWVISOR_SESSIONS_DIR = originalEnv;
    await fs.rm(root, { recursive: true, force: true });
  }
});

test("scavenger picks up new asst written after the watermark advanced", async () => {
  const root = await fs.mkdtemp(join(tmpdir(), "scav-"));
  const originalEnv = process.env.CLAWVISOR_SESSIONS_DIR;
  process.env.CLAWVISOR_SESSIONS_DIR = root;
  try {
    const t1 = nowSec() - 120;
    const t2 = nowSec() - 60;
    const file = await seedSession(root, "main", [
      { id: "u1", message: { role: "user", content: "hi" } },
      { id: "a1", timestamp: t1, message: { role: "assistant", content: "hello" } },
      { id: "u2", message: { role: "user", content: "next" } },
    ]);
    const { bridge, calls } = spyBridge();
    const scavenge = createScavenger({ bridge, logger: silentLogger, defaultAgentId: "main" });
    await scavenge({ agentId: "main" });

    await fs.writeFile(
      file,
      [
        { id: "u1", message: { role: "user", content: "hi" } },
        { id: "a1", timestamp: t1, message: { role: "assistant", content: "hello" } },
        { id: "u2", message: { role: "user", content: "next" } },
        { id: "a2", timestamp: t2, message: { role: "assistant", content: "sure thing" } },
        { id: "u3", message: { role: "user", content: "ok" } },
      ]
        .map((e) => JSON.stringify(e))
        .join("\n") + "\n",
    );
    await scavenge({ agentId: "main" });

    assert.equal(calls.length, 2);
    assert.match(String(calls[0]!.content), /hello/);
    assert.match(String(calls[1]!.content), /sure thing/);
  } finally {
    process.env.CLAWVISOR_SESSIONS_DIR = originalEnv;
    await fs.rm(root, { recursive: true, force: true });
  }
});

test("scavenger captures mid-conversation asst even if the current user isn't in JSONL yet", async () => {
  // Reproduces the real-world case where `message_received` fires before
  // OpenClaw writes the user node for the in-flight message — the reply
  // to the PREVIOUS user is still captured.
  const root = await fs.mkdtemp(join(tmpdir(), "scav-"));
  const originalEnv = process.env.CLAWVISOR_SESSIONS_DIR;
  process.env.CLAWVISOR_SESSIONS_DIR = root;
  try {
    const tReply = nowSec() - 30;
    await seedSession(root, "main", [
      { id: "u1", message: { role: "user", content: "hi" } },
      { id: "a1", timestamp: nowSec() - 120, message: { role: "assistant", content: "hey" } },
      { id: "u2", message: { role: "user", content: "what's up" } },
      { id: "a2", timestamp: tReply, message: { role: "assistant", content: "not much" } },
      // NOTE: "u3" is not in JSONL yet — the plugin's message_received
      // hook fires before OpenClaw writes the current user node.
    ]);
    const { bridge, calls } = spyBridge();
    const scavenge = createScavenger({ bridge, logger: silentLogger, defaultAgentId: "main" });
    await scavenge({ agentId: "main" });

    // Both recent asst are in-window; "not much" in particular proves the
    // scavenger doesn't need the current user to be in JSONL to fire.
    assert.ok(
      calls.some((c) => /not much/.test(String(c.content))),
      "must capture a2 even though u3 isn't in JSONL yet",
    );
  } finally {
    process.env.CLAWVISOR_SESSIONS_DIR = originalEnv;
    await fs.rm(root, { recursive: true, force: true });
  }
});

test("scavenger handles missing sessions directory gracefully", async () => {
  const root = await fs.mkdtemp(join(tmpdir(), "scav-"));
  const originalEnv = process.env.CLAWVISOR_SESSIONS_DIR;
  process.env.CLAWVISOR_SESSIONS_DIR = root;
  try {
    const { bridge, calls } = spyBridge();
    const scavenge = createScavenger({ bridge, logger: silentLogger, defaultAgentId: "never-paired" });
    await scavenge({ agentId: "never-paired" });
    assert.equal(calls.length, 0);
  } finally {
    process.env.CLAWVISOR_SESSIONS_DIR = originalEnv;
    await fs.rm(root, { recursive: true, force: true });
  }
});
