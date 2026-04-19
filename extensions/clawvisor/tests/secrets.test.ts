import { test } from "node:test";
import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { readSecrets, writeSecrets, type PluginSecrets } from "../src/secrets.js";

async function mktemp(): Promise<string> {
  return await fs.mkdtemp(join(tmpdir(), "clawvisor-secrets-"));
}

test("readSecrets returns empty object when file does not exist", async () => {
  const dir = await mktemp();
  try {
    const result = await readSecrets(join(dir, "nope", "secrets.json"));
    assert.deepEqual(result, {});
  } finally {
    await fs.rm(dir, { recursive: true, force: true });
  }
});

test("writeSecrets creates file with 0600 perms and readSecrets round-trips", async () => {
  const dir = await mktemp();
  try {
    const path = join(dir, "clawvisor", "secrets.json");
    const secrets: PluginSecrets = {
      bridgeToken: "cvisbr_xyz",
      agentTokens: { main: "cvis_a", researcher: "cvis_b" },
      installFingerprint: "install_abc",
    };
    await writeSecrets(path, secrets);

    const stat = await fs.stat(path);
    // Mask out anything above the permission bits; 0o600 = owner rw, no one else.
    assert.equal(stat.mode & 0o777, 0o600, "secrets file must be 0600");

    const parentStat = await fs.stat(join(dir, "clawvisor"));
    assert.equal(
      parentStat.mode & 0o777,
      0o700,
      "parent directory must be 0700 so group/other can't read the file through directory listing",
    );

    const read = await readSecrets(path);
    assert.deepEqual(read, secrets);
  } finally {
    await fs.rm(dir, { recursive: true, force: true });
  }
});

test("writeSecrets is atomic: mid-crash tempfile must not stand in for real file", async () => {
  const dir = await mktemp();
  try {
    const path = join(dir, "secrets.json");
    await writeSecrets(path, { bridgeToken: "first" });
    // Second write should overwrite atomically — readers always see the full payload.
    await writeSecrets(path, { bridgeToken: "second" });
    const read = await readSecrets(path);
    assert.equal(read.bridgeToken, "second");
    // No leftover .tmp files.
    const entries = await fs.readdir(dir);
    const strays = entries.filter((e) => e.includes(".tmp"));
    assert.deepEqual(strays, [], "rename should leave no .tmp stragglers");
  } finally {
    await fs.rm(dir, { recursive: true, force: true });
  }
});
