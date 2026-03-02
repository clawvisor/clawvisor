import { createHmac, createHash, createPublicKey, timingSafeEqual, generateKeyPairSync, sign, randomUUID } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";
import type { OpenClawPluginApi } from "openclaw/plugin-sdk";
import { readFileSync, writeFileSync, existsSync, mkdirSync } from "node:fs";
import { join, dirname } from "node:path";
import WebSocket from "ws";

const DEFAULT_PATH = "/clawvisor/callback";
const MAX_BODY_BYTES = 1024 * 512;
const DEFAULT_GATEWAY_WS_URL = "ws://127.0.0.1:18789";
const DEFAULT_SESSION_KEY = "agent:main:main";

// ── Types ──────────────────────────────────────────────────────────────

interface ClawvisorConfig {
  secret?: string;
  path?: string;
  gatewayWsUrl?: string;
}

interface ClawvisorRequestCallback {
  type: "request";
  request_id: string;
  status: "executed" | "denied" | "timeout" | "error";
  result?: { summary?: string; data?: unknown };
  error?: string;
  audit_id?: string;
}

interface ClawvisorTaskCallback {
  type: "task";
  task_id: string;
  status: "approved" | "denied" | "scope_expanded" | "scope_expansion_denied" | "expired";
}

type ClawvisorCallbackPayload = ClawvisorRequestCallback | ClawvisorTaskCallback;

interface DeviceIdentity {
  id: string;
  publicKey: string;
  privateKey: string;
}

interface PendingRequest {
  resolve: (value: unknown) => void;
  reject: (reason: unknown) => void;
}

// ── Crypto helpers ─────────────────────────────────────────────────────

function verifySignature(body: string, secret: string, header: string | undefined): boolean {
  if (!header) return false;
  const expected = "sha256=" + createHmac("sha256", secret).update(body).digest("hex");
  try {
    const a = Buffer.from(header.padEnd(expected.length));
    const b = Buffer.from(expected);
    if (a.length !== b.length) return false;
    return timingSafeEqual(a, b);
  } catch {
    return false;
  }
}

function base64UrlEncode(buf: Buffer): string {
  return buf.toString("base64").replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function signNonce(privateKey: string, payload: string): string {
  const sig = sign(null, Buffer.from(payload, "utf8"), privateKey);
  return base64UrlEncode(sig);
}

function publicKeyToBase64Url(publicKeyPem: string): string {
  const ED25519_SPKI_PREFIX = Buffer.from("302a300506032b6570032100", "hex");
  const spki = createPublicKey(publicKeyPem).export({ type: "spki", format: "der" });
  const raw = spki.length === ED25519_SPKI_PREFIX.length + 32 && spki.subarray(0, ED25519_SPKI_PREFIX.length).equals(ED25519_SPKI_PREFIX)
    ? spki.subarray(ED25519_SPKI_PREFIX.length)
    : spki;
  return base64UrlEncode(raw);
}

// ── Device identity (persisted) ────────────────────────────────────────

function getOrCreateDevice(dataDir: string): DeviceIdentity {
  const deviceFile = join(dataDir, "device.json");
  if (existsSync(deviceFile)) {
    return JSON.parse(readFileSync(deviceFile, "utf8"));
  }

  const { publicKey, privateKey } = generateKeyPairSync("ed25519", {
    publicKeyEncoding: { type: "spki", format: "pem" },
    privateKeyEncoding: { type: "pkcs8", format: "pem" },
  });

  // Device ID = SHA-256 of raw 32-byte Ed25519 public key (matching gateway's deriveDeviceIdFromPublicKey)
  const ED25519_SPKI_PREFIX = Buffer.from("302a300506032b6570032100", "hex");
  const spki = createPublicKey(publicKey).export({ type: "spki", format: "der" });
  const raw = spki.length === ED25519_SPKI_PREFIX.length + 32 && spki.subarray(0, ED25519_SPKI_PREFIX.length).equals(ED25519_SPKI_PREFIX)
    ? spki.subarray(ED25519_SPKI_PREFIX.length)
    : spki;
  const id = createHash("sha256").update(raw).digest("hex");
  const device: DeviceIdentity = { id, publicKey, privateKey };

  mkdirSync(dirname(deviceFile), { recursive: true });
  writeFileSync(deviceFile, JSON.stringify(device, null, 2), { mode: 0o600 });
  return device;
}

// ── Body reader ────────────────────────────────────────────────────────

async function readBody(req: IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    let body = "";
    let size = 0;
    req.on("data", (chunk: Buffer) => {
      size += chunk.length;
      if (size > MAX_BODY_BYTES) { reject(new Error("Payload too large")); return; }
      body += chunk.toString();
    });
    req.on("end", () => resolve(body));
    req.on("error", reject);
  });
}

// ── Wake text builder ──────────────────────────────────────────────────

const DELIVERY_INSTRUCTION =
  "\n\nDelivery: If your current channel is not webchat, use the `message` tool with `action=send` to deliver your response to the user.";

function buildWakeText(payload: ClawvisorCallbackPayload): string {
  const text = payload.type === "task"
    ? buildTaskWakeText(payload)
    : buildRequestWakeText(payload);
  return text + DELIVERY_INSTRUCTION;
}

function buildTaskWakeText(payload: ClawvisorTaskCallback): string {
  const { task_id, status } = payload;

  switch (status) {
    case "approved":
      return [
        `[Clawvisor] Task ${task_id} has been approved by the user.`,
        "Action required: Read the Clawvisor SKILL.md first, then continue with task execution — make the gateway requests you were waiting on.",
        "When done, respond in the conversation where the task was initiated (if applicable).",
      ].join("\n");
    case "denied":
      return [
        `[Clawvisor] Task ${task_id} was denied by the user.`,
        "Action required: Do NOT proceed with any gateway requests for this task.",
        "Respond in the conversation where the task was initiated (if applicable) to let the user know.",
      ].join("\n");
    case "scope_expanded":
      return [
        `[Clawvisor] Task ${task_id} scope expansion was approved.`,
        "Action required: The new action is now in scope. Continue with the task.",
      ].join("\n");
    case "scope_expansion_denied":
      return [
        `[Clawvisor] Task ${task_id} scope expansion was denied.`,
        "Action required: The requested action was not added to the task scope. You'll need per-request approval for that action, or inform the user.",
      ].join("\n");
    case "expired":
      return [
        `[Clawvisor] Task ${task_id} has expired.`,
        "Action required: If work is still needed, create a new task or expand the expired one. Inform the user if applicable.",
      ].join("\n");
    default:
      return `[Clawvisor] Task ${task_id} status: ${status}`;
  }
}

function buildRequestWakeText(payload: ClawvisorRequestCallback): string {
  const { request_id, status, result, error } = payload;

  let instructions: string;
  switch (status) {
    case "executed":
      instructions = [
        `[Clawvisor] Request ${request_id} has been executed successfully.`,
        "Action required: Read the Clawvisor SKILL.md if needed, then process the result below.",
        "Respond in the conversation where the request was initiated (if applicable).",
        "If this was part of a multi-step task, continue with the next step.",
      ].join("\n");
      break;
    case "denied":
      instructions = [
        `[Clawvisor] Request ${request_id} was denied by the user.`,
        "Action required: Respond in the conversation where the request was initiated (if applicable) to let the user know. Do not retry.",
      ].join("\n");
      break;
    case "timeout":
      instructions = [
        `[Clawvisor] Request ${request_id} timed out waiting for approval.`,
        "Action required: Respond in the conversation where the request was initiated (if applicable). They may need to re-approve.",
      ].join("\n");
      break;
    default:
      instructions = [
        `[Clawvisor] Request ${request_id} returned an error.`,
        "Action required: Respond in the conversation where the request was initiated (if applicable) about the error.",
      ].join("\n");
      break;
  }

  let body: string;
  if (status === "executed" && result) {
    const summary = result.summary ?? JSON.stringify(result.data ?? {});
    body = `Result: ${summary}`;
    if (result.data) body += `\nFull data: ${JSON.stringify(result.data)}`;
  } else if (error) {
    body = `Error: ${error}`;
  } else {
    body = `Status: ${status}`;
  }

  return `${instructions}\n\n---\n${body}`;
}

// ── Gateway WebSocket client ───────────────────────────────────────────

class GatewayClient {
  private ws: WebSocket | null = null;
  private device: DeviceIdentity;
  private gatewayToken: string;
  private gatewayWsUrl: string;
  private log: OpenClawPluginApi["log"];
  private pending = new Map<string, PendingRequest>();
  private connected = false;
  private connecting = false;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private tickTimer: ReturnType<typeof setInterval> | null = null;
  private tickIntervalMs = 15000;

  constructor(device: DeviceIdentity, gatewayToken: string, gatewayWsUrl: string, log: OpenClawPluginApi["log"]) {
    this.device = device;
    this.gatewayToken = gatewayToken;
    this.gatewayWsUrl = gatewayWsUrl;
    this.log = log;
  }

  async ensureConnected(): Promise<void> {
    if (this.connected) return;
    if (this.connecting) {
      // Wait for in-flight connection
      await new Promise<void>((resolve, reject) => {
        const check = setInterval(() => {
          if (this.connected) { clearInterval(check); resolve(); }
          if (!this.connecting) { clearInterval(check); reject(new Error("Connection failed")); }
        }, 100);
        setTimeout(() => { clearInterval(check); reject(new Error("Connection timeout")); }, 10000);
      });
      return;
    }
    await this.connect();
  }

  private connect(): Promise<void> {
    this.connecting = true;
    return new Promise((resolve, reject) => {
      this.ws = new WebSocket(this.gatewayWsUrl);

      this.ws.on("open", () => {
        this.log?.info("clawvisor-webhook: WS connected, waiting for challenge");
      });

      this.ws.on("message", (data: Buffer) => {
        const frame = JSON.parse(data.toString());

        if (frame.type === "event" && frame.event === "connect.challenge") {
          this.handleChallenge(frame.payload, resolve, reject);
          return;
        }

        if (frame.type === "res") {
          // Handle connect response
          if (frame.ok && frame.payload?.type === "hello-ok") {
            this.connected = true;
            this.connecting = false;
            this.tickIntervalMs = frame.payload.policy?.tickIntervalMs ?? 15000;
            this.startTicking();
            this.log?.info("clawvisor-webhook: Gateway handshake complete");
            resolve();
            return;
          }

          // Handle RPC responses
          const pending = this.pending.get(frame.id);
          if (pending) {
            this.pending.delete(frame.id);
            if (frame.ok) {
              pending.resolve(frame.payload);
            } else {
              pending.reject(new Error(frame.error?.message ?? "RPC error"));
            }
          }
        }

        if (frame.type === "event" && frame.event === "tick") {
          // Respond to server ticks with a pong
          return;
        }
      });

      this.ws.on("close", () => {
        this.connected = false;
        this.connecting = false;
        this.stopTicking();
        this.log?.warn("clawvisor-webhook: WS closed, will reconnect on next callback");
      });

      this.ws.on("error", (err: Error) => {
        this.connecting = false;
        this.log?.error(`clawvisor-webhook: WS error: ${err.message}`);
        reject(err);
      });
    });
  }

  private handleChallenge(
    challenge: { nonce: string; ts: number },
    resolve: () => void,
    reject: (err: Error) => void,
  ) {
    const signedAt = Date.now();
    const clientId = "cli";
    const clientMode = "cli";
    const role = "operator";
    const scopes = ["operator.read", "operator.write"];

    // v2 payload: v2|deviceId|clientId|clientMode|role|scopes|signedAt|token|nonce
    const payload = [
      "v2",
      this.device.id,
      clientId,
      clientMode,
      role,
      scopes.join(","),
      String(signedAt),
      this.gatewayToken,
      challenge.nonce,
    ].join("|");
    const signature = signNonce(this.device.privateKey, payload);

    const connectReq = {
      type: "req",
      id: randomUUID(),
      method: "connect",
      params: {
        minProtocol: 3,
        maxProtocol: 3,
        client: {
          id: clientId,
          version: "1.0.0",
          platform: "macos",
          mode: clientMode,
        },
        role,
        scopes,
        caps: [],
        commands: [],
        permissions: {},
        auth: { token: this.gatewayToken },
        locale: "en-US",
        userAgent: "clawvisor-webhook/1.0.0",
        device: {
          id: this.device.id,
          publicKey: publicKeyToBase64Url(this.device.publicKey),
          signature,
          signedAt,
          nonce: challenge.nonce,
        },
      },
    };

    this.ws!.send(JSON.stringify(connectReq));
  }

  private startTicking() {
    this.stopTicking();
    this.tickTimer = setInterval(() => {
      if (this.ws?.readyState === WebSocket.OPEN) {
        this.ws.send(JSON.stringify({ type: "req", id: randomUUID(), method: "tick", params: {} }));
      }
    }, this.tickIntervalMs);
  }

  private stopTicking() {
    if (this.tickTimer) { clearInterval(this.tickTimer); this.tickTimer = null; }
  }

  async chatSend(message: string, sessionKey: string = DEFAULT_SESSION_KEY, idempotencyKey?: string): Promise<unknown> {
    await this.ensureConnected();

    const id = randomUUID();
    const req = {
      type: "req",
      id,
      method: "chat.send",
      params: {
        sessionKey,
        message,
        idempotencyKey: idempotencyKey ?? randomUUID(),
      },
    };

    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.ws!.send(JSON.stringify(req));
      setTimeout(() => {
        if (this.pending.has(id)) {
          this.pending.delete(id);
          reject(new Error("chat.send timeout"));
        }
      }, 30000);
    });
  }

  destroy() {
    this.stopTicking();
    if (this.ws) { this.ws.close(); this.ws = null; }
  }
}

// ── Plugin ─────────────────────────────────────────────────────────────

const plugin = {
  id: "clawvisor-webhook",
  name: "Clawvisor Webhook",
  description: "Receives Clawvisor action callbacks and injects them into the main session via Gateway WS chat.send.",

  register(api: OpenClawPluginApi) {
    const config = api.pluginConfig as ClawvisorConfig;

    const secret = config?.secret || process.env.CLAWVISOR_CALLBACK_SECRET;
    if (!secret) {
      api.log?.warn("clawvisor-webhook: no secret configured (set plugin config or CLAWVISOR_CALLBACK_SECRET env) — webhook handler not registered");
      return;
    }

    const gatewayToken = (api.config as unknown as { gateway?: { auth?: { token?: string }; token?: string } })?.gateway?.auth?.token
      ?? (api.config as unknown as { gateway?: { token?: string } })?.gateway?.token;
    if (!gatewayToken) {
      api.log?.warn("clawvisor-webhook: no gateway token found — cannot connect WS");
      return;
    }

    const webhookPath = config.path ?? DEFAULT_PATH;
    const gatewayWsUrl = config.gatewayWsUrl ?? process.env.OPENCLAW_GATEWAY_WS_URL ?? DEFAULT_GATEWAY_WS_URL;
    const dataDir = join(process.env.HOME ?? "/tmp", ".openclaw", "extensions", "clawvisor-webhook");
    const device = getOrCreateDevice(dataDir);
    const gateway = new GatewayClient(device, gatewayToken, gatewayWsUrl, api.log);

    api.log?.info(`clawvisor-webhook: listening on ${webhookPath} (WS → ${gatewayWsUrl}, device=${device.id})`);

    // Core handler logic shared by both registration methods.
    async function handleCallback(req: IncomingMessage, res: ServerResponse): Promise<void> {
      if (req.method !== "POST") {
        res.writeHead(405, { "Content-Type": "text/plain" });
        res.end("Method Not Allowed");
        return;
      }

      let rawBody: string;
      try {
        rawBody = await readBody(req);
      } catch (err: unknown) {
        res.writeHead(413, { "Content-Type": "text/plain" });
        res.end(err instanceof Error ? err.message : "Read error");
        return;
      }

      const signature = req.headers["x-clawvisor-signature"] as string | undefined;
      if (!verifySignature(rawBody, secret, signature)) {
        res.writeHead(401, { "Content-Type": "text/plain" });
        res.end("Unauthorized: invalid signature");
        return;
      }

      let payload: ClawvisorCallbackPayload;
      try {
        payload = JSON.parse(rawBody);
      } catch {
        res.writeHead(400, { "Content-Type": "text/plain" });
        res.end("Bad Request: invalid JSON");
        return;
      }

      // Extract session key from query param, fall back to default
      const url = req.url ?? "";
      const urlObj = new URL(url, "http://localhost");
      const sessionKey = urlObj.searchParams.get("session") ?? DEFAULT_SESSION_KEY;

      const wakeText = buildWakeText(payload);

      // Derive a stable idempotency key from the payload so that Clawvisor
      // callback retries don't start duplicate agent turns.
      const callbackId = payload.type === "task"
        ? `clawvisor:task:${payload.task_id}:${payload.status}`
        : `clawvisor:request:${payload.request_id}:${payload.status}`;

      try {
        const result = await gateway.chatSend(wakeText, sessionKey, callbackId);
        const payloadId = payload.type === "task" ? payload.task_id : payload.request_id;
        api.log?.info(`clawvisor-webhook: chat.send OK for ${payloadId}: ${JSON.stringify(result)}`);
      } catch (err: unknown) {
        const msg = err instanceof Error ? err.message : "chat.send error";
        api.log?.error(`clawvisor-webhook: chat.send failed: ${msg}`);
        res.writeHead(500, { "Content-Type": "text/plain" });
        res.end(`Internal error: ${msg}`);
        return;
      }

      res.writeHead(202, { "Content-Type": "application/json" });
      const responseId = payload.type === "task"
        ? { task_id: payload.task_id }
        : { request_id: payload.request_id };
      res.end(JSON.stringify({ ok: true, ...responseId }));
    }

    // Prefer registerHttpRoute (new Plugin SDK) with fallback to
    // registerHttpHandler (legacy) for backward compatibility.
    if (typeof api.registerHttpRoute === "function") {
      api.registerHttpRoute("POST", webhookPath, handleCallback);
    } else {
      api.registerHttpHandler(async (req: IncomingMessage, res: ServerResponse): Promise<boolean> => {
        const url = req.url ?? "";
        if (url !== webhookPath && !url.startsWith(webhookPath + "?")) return false;
        await handleCallback(req, res);
        return true;
      });
    }
  },
};

export default plugin;
