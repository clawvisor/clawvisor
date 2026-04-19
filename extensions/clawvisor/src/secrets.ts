import { promises as fs } from "node:fs";
import { dirname } from "node:path";
import { homedir } from "node:os";
import { resolve as resolvePath } from "node:path";

/**
 * On-disk shape for the plugin-owned secrets file. Separate from the
 * OpenClaw plugin config (which is typically YAML and may be
 * version-controlled) so that tokens never land in plaintext config,
 * logs, or shell scrollback.
 */
export interface PluginSecrets {
  bridgeToken?: string;
  agentTokens?: Record<string, string>;
  installFingerprint?: string;
}

/**
 * Resolve the secrets file path. Override with CLAWVISOR_SECRETS_FILE for
 * tests or unusual deployment layouts; default is
 * ~/.openclaw/plugins/clawvisor/secrets.json.
 *
 * 0600 file + 0700 parent directory: owner read/write only, defense
 * against accidental readability in a multi-user system or when the
 * OpenClaw home gets backed up into a group-readable archive.
 */
export function defaultSecretsPath(): string {
  const override = process.env.CLAWVISOR_SECRETS_FILE;
  if (override && override.length > 0) return override;
  return resolvePath(homedir(), ".openclaw", "plugins", "clawvisor", "secrets.json");
}

export async function readSecrets(path: string): Promise<PluginSecrets> {
  try {
    const raw = await fs.readFile(path, "utf8");
    const parsed = JSON.parse(raw) as unknown;
    if (parsed && typeof parsed === "object") return parsed as PluginSecrets;
    return {};
  } catch (err) {
    const e = err as NodeJS.ErrnoException;
    if (e && (e.code === "ENOENT" || e.code === "ENOTDIR")) return {};
    // Any other error (bad permissions, malformed JSON) we surface so the
    // caller can decide whether to refuse to pair or fail loud.
    throw err;
  }
}

/**
 * Atomically write secrets with 0600 permissions. Creates parent directory
 * with 0700 if missing. Writes to a sibling tempfile and renames to avoid
 * readers seeing a truncated file if we crash mid-write.
 */
export async function writeSecrets(path: string, secrets: PluginSecrets): Promise<void> {
  const dir = dirname(path);
  await fs.mkdir(dir, { recursive: true, mode: 0o700 });
  // Tighten parent dir mode even if it already existed with looser perms —
  // best-effort; a non-owner dir will fail, which is the right outcome.
  try {
    await fs.chmod(dir, 0o700);
  } catch {
    /* non-fatal */
  }

  const tmp = `${path}.tmp.${process.pid}.${Date.now()}`;
  const body = JSON.stringify(secrets, null, 2);
  await fs.writeFile(tmp, body, { encoding: "utf8", mode: 0o600 });
  try {
    await fs.rename(tmp, path);
  } catch (err) {
    // Best-effort cleanup of the tmp file if the rename fails.
    try {
      await fs.unlink(tmp);
    } catch {
      /* ignore */
    }
    throw err;
  }
  // On some filesystems rename preserves the source mode; on others it
  // inherits umask. Re-chmod after rename to be sure.
  try {
    await fs.chmod(path, 0o600);
  } catch {
    /* non-fatal */
  }
}
