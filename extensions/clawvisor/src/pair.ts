import { hostname as osHostname } from "node:os";
import { randomUUID } from "node:crypto";
import { promises as fs } from "node:fs";
import { dirname, resolve as resolvePath } from "node:path";
import { fileURLToPath } from "node:url";

import { requestPair } from "./clawvisor-client.js";
import type { PairApprovedResponse } from "./types.js";

export interface PairInput {
  url: string;
  pairCode: string;
  agentIds: string[];
  /** Stable install fingerprint if one has been persisted; otherwise one is generated. */
  installFingerprint?: string;
  /** Stable idempotency key for this pair attempt, survives long-poll retries. */
  idempotencyKey?: string;
}

// Read the plugin's own bundled VERSION file, if present. Written by
// scripts/bundle.mjs into the tarball root so deployed plugins always
// carry a version string. Returns "" in dev runs where the plugin is
// loaded from source (no VERSION file exists).
async function readBundledVersion(): Promise<string> {
  try {
    const here = dirname(fileURLToPath(import.meta.url));
    // In the bundled tarball layout, index.js and VERSION are siblings.
    // In source mode (running from extensions/clawvisor/index.ts), there
    // is no VERSION file — we'll return empty and the server will skip
    // the drift check.
    const v = await fs.readFile(resolvePath(here, "..", "VERSION"), "utf8");
    return v.trim();
  } catch {
    return "";
  }
}

export interface PairOutcome {
  bridgeToken: string;
  agents: Record<string, string>;
  installFingerprint: string;
}

export interface PairDeps {
  logger: {
    info(msg: string, ...args: unknown[]): void;
    warn(msg: string, ...args: unknown[]): void;
    error(msg: string, ...args: unknown[]): void;
  };
}

/**
 * Drives the OpenClaw plugin pair flow. Blocks until the user approves (or
 * the long-poll window elapses). Caller is responsible for persisting the
 * returned tokens to the plugin-owned secrets file.
 */
export async function pairPlugin(input: PairInput, deps: PairDeps): Promise<PairOutcome> {
  if (!input.url) throw new Error("clawvisor: url is required to pair");
  if (!input.pairCode) {
    throw new Error(
      "clawvisor: pairCode is required to pair — the agent should deposit it via /skill/setup-openclaw",
    );
  }

  const installFingerprint = input.installFingerprint ?? `install_${randomUUID()}`;
  const idempotencyKey = input.idempotencyKey ?? randomUUID();
  const host = safeHostname();
  const agentIds = input.agentIds.slice();
  const pluginVersion = await readBundledVersion();

  deps.logger.info(
    `clawvisor: initiating plugin pair — version=${pluginVersion || "dev"} fingerprint=${installFingerprint} host=${host} agents=[${agentIds.join(",")}]`,
  );
  deps.logger.info(
    `clawvisor: approve the pairing request at ${input.url}/dashboard/agents (blocks up to ~2 min)`,
  );

  const resp: PairApprovedResponse = await requestPair(input.url, {
    pair_code: input.pairCode,
    install_fingerprint: installFingerprint,
    hostname: host,
    agent_ids: agentIds,
    idempotency_key: idempotencyKey,
    ...(pluginVersion ? { plugin_version: pluginVersion } : {}),
  });

  if (resp.status !== "approved" || !resp.bridge_token) {
    throw new Error(`clawvisor: pair request did not resolve to approved: status=${resp.status}`);
  }

  return {
    bridgeToken: resp.bridge_token,
    agents: resp.agents ?? {},
    installFingerprint,
  };
}

function safeHostname(): string {
  try {
    return osHostname() || "unknown";
  } catch {
    return "unknown";
  }
}
