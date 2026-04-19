import type { BufferIngestRequest, PairApprovedResponse } from "./types.js";

const DEFAULT_TIMEOUT_MS = 30_000;
/** Long-poll client timeout — must exceed Clawvisor's 120s max. */
const LONG_POLL_TIMEOUT_MS = 130_000;

async function httpRequest(
  baseUrl: string,
  method: string,
  path: string,
  bearer: string | undefined,
  body?: Record<string, unknown>,
  timeoutMs?: number,
  extraHeaders?: Record<string, string>,
): Promise<unknown> {
  const url = `${baseUrl.replace(/\/+$/, "")}${path}`;
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (bearer) {
    headers.Authorization = `Bearer ${bearer}`;
  }
  if (extraHeaders) {
    for (const [k, v] of Object.entries(extraHeaders)) headers[k] = v;
  }
  const res = await fetch(url, {
    method,
    headers,
    body: body ? JSON.stringify(body) : undefined,
    signal: AbortSignal.timeout(timeoutMs ?? DEFAULT_TIMEOUT_MS),
  });
  const text = await res.text();
  let json: unknown;
  try {
    json = text ? JSON.parse(text) : {};
  } catch {
    json = { raw: text };
  }
  if (!res.ok) {
    const errMsg =
      typeof json === "object" && json !== null && "error" in json
        ? String((json as { error: unknown }).error)
        : text;
    throw new Error(`clawvisor ${method} ${path}: ${res.status} ${errMsg}`);
  }
  return json;
}

/**
 * Agent-token client. Used by OpenClaw agents for task / gateway /
 * feedback calls. One instance per paired agent.
 */
export class ClawvisorClient {
  private baseUrl: string;
  private agentToken: string;

  constructor(baseUrl: string, agentToken: string) {
    this.baseUrl = baseUrl;
    this.agentToken = agentToken;
  }

  async isHealthy(): Promise<boolean> {
    try {
      const res = await fetch(`${this.baseUrl.replace(/\/+$/, "")}/health`, {
        signal: AbortSignal.timeout(5_000),
      });
      return res.ok;
    } catch {
      return false;
    }
  }

  async fetchCatalog(params?: { service?: string }): Promise<unknown> {
    const qs = params?.service ? `?service=${encodeURIComponent(params.service)}` : "";
    return httpRequest(this.baseUrl, "GET", `/api/skill/catalog${qs}`, this.agentToken);
  }

  async createTask(
    body: Record<string, unknown>,
    opts?: { bridgeAttestation?: string },
  ): Promise<unknown> {
    // bridgeAttestation is the plugin's vouch for a group_chat_id injected
    // into the body. Server only trusts group_chat_id when this header is
    // present and the referenced bridge belongs to the same user as the
    // agent token. Passed as an explicit option (not a stored field) so
    // this client never retains the bridge token — the plugin runtime
    // supplies it on each call.
    const extra = opts?.bridgeAttestation
      ? { "X-Clawvisor-Bridge-Attestation": `Bearer ${opts.bridgeAttestation}` }
      : undefined;
    return httpRequest(
      this.baseUrl,
      "POST",
      "/api/tasks",
      this.agentToken,
      body,
      body.wait ? LONG_POLL_TIMEOUT_MS : undefined,
      extra,
    );
  }

  async getTask(id: string, opts?: { wait?: boolean; timeout?: number }): Promise<unknown> {
    const params = new URLSearchParams();
    if (opts?.wait) params.set("wait", "true");
    if (opts?.timeout) params.set("timeout", String(opts.timeout));
    const qs = params.toString();
    return httpRequest(
      this.baseUrl,
      "GET",
      `/api/tasks/${encodeURIComponent(id)}${qs ? `?${qs}` : ""}`,
      this.agentToken,
      undefined,
      opts?.wait ? LONG_POLL_TIMEOUT_MS : undefined,
    );
  }

  async completeTask(id: string): Promise<unknown> {
    return httpRequest(
      this.baseUrl,
      "POST",
      `/api/tasks/${encodeURIComponent(id)}/complete`,
      this.agentToken,
    );
  }

  async expandTask(id: string, body: Record<string, unknown>): Promise<unknown> {
    return httpRequest(
      this.baseUrl,
      "POST",
      `/api/tasks/${encodeURIComponent(id)}/expand`,
      this.agentToken,
      body,
      body.wait ? LONG_POLL_TIMEOUT_MS : undefined,
    );
  }

  async gatewayRequest(body: Record<string, unknown>): Promise<unknown> {
    return httpRequest(
      this.baseUrl,
      "POST",
      "/api/gateway/request",
      this.agentToken,
      body,
      body.wait ? LONG_POLL_TIMEOUT_MS : undefined,
    );
  }

  async executeRequest(requestId: string, body?: Record<string, unknown>): Promise<unknown> {
    return httpRequest(
      this.baseUrl,
      "POST",
      `/api/gateway/request/${encodeURIComponent(requestId)}/execute`,
      this.agentToken,
      body,
      LONG_POLL_TIMEOUT_MS,
    );
  }

  async reportBug(body: Record<string, unknown>): Promise<unknown> {
    return httpRequest(this.baseUrl, "POST", "/api/feedback/report", this.agentToken, body);
  }
}

/**
 * Bridge-token client. Used ONLY by the plugin runtime — never passed into
 * any agent tool handler. Narrow surface: forwarding messages into the
 * approval buffer, and requesting additional agent tokens post-pair. A
 * leaked bridge token cannot create tasks or execute gateway requests.
 */
export class BridgeClient {
  private baseUrl: string;
  private bridgeToken: string;

  constructor(baseUrl: string, bridgeToken: string) {
    this.baseUrl = baseUrl;
    this.bridgeToken = bridgeToken;
  }

  async ingestMessage(msg: BufferIngestRequest): Promise<void> {
    await httpRequest(
      this.baseUrl,
      "POST",
      "/api/buffer/ingest",
      this.bridgeToken,
      msg as unknown as Record<string, unknown>,
    );
  }

  /** Request a new agent token for an additional OpenClaw agent post-pair. */
  async requestAgentAdd(agentId: string): Promise<unknown> {
    return httpRequest(
      this.baseUrl,
      "POST",
      "/api/plugin/agents?wait=true",
      this.bridgeToken,
      { agent_id: agentId },
      LONG_POLL_TIMEOUT_MS,
    );
  }
}

/**
 * Unauthenticated pair-request helper — used once during initial setup.
 * The plugin redeems a one-time pair_code (minted by the dashboard), posts
 * its install identity, and waits for the dashboard user to approve; after
 * which bridge + agent tokens are returned inline. The idempotency_key
 * lets long-poll retries collapse onto the same pending pair request
 * instead of fanning out into duplicate approval cards.
 */
export async function requestPair(
  baseUrl: string,
  body: {
    pair_code: string;
    install_fingerprint: string;
    hostname: string;
    agent_ids: string[];
    idempotency_key: string;
    /** Optional plugin_version (the bundle version). Server logs drift. */
    plugin_version?: string;
  },
): Promise<PairApprovedResponse> {
  return httpRequest(
    baseUrl,
    "POST",
    "/api/plugin/pair?wait=true",
    undefined,
    body,
    LONG_POLL_TIMEOUT_MS,
    { "Idempotency-Key": body.idempotency_key },
  ) as Promise<PairApprovedResponse>;
}

/** Resume a previously-initiated pair request (e.g. after plugin restart). */
export async function pollPair(baseUrl: string, pairId: string): Promise<PairApprovedResponse> {
  return httpRequest(
    baseUrl,
    "GET",
    `/api/plugin/pair/${encodeURIComponent(pairId)}?wait=true`,
    undefined,
    undefined,
    LONG_POLL_TIMEOUT_MS,
  ) as Promise<PairApprovedResponse>;
}
