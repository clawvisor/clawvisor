#!/usr/bin/env node
// Bundles the clawvisor OpenClaw plugin into a drop-in tarball.
//
// Output: dist/openclaw-plugin.tgz + dist/openclaw-plugin.sha256
//
// Tarball layout (extracts to ~/.openclaw/plugins/clawvisor/):
//   clawvisor/
//   ├── index.js           (bundled, all non-SDK deps inlined)
//   ├── openclaw.plugin.json
//   ├── package.json        (minimal — no dependencies because bundled)
//   ├── README.md
//   └── VERSION             (plain-text version string, for runtime checks)
//
// Dependency handling:
//   - @sinclair/typebox: bundled.
//   - openclaw/*:         external. OpenClaw's own runtime provides the SDK
//                         when it loads the plugin; bundling it would pin us
//                         to a snapshot and break cross-version compat.

import { build } from "esbuild";
import { execFileSync } from "node:child_process";
import { promises as fs } from "node:fs";
import { createHash } from "node:crypto";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const root = resolve(here, "..");
// The canonical output lives next to the Go code that embeds it — the
// server binary's `//go:embed` sources straight from this directory, so
// a fresh bundle automatically lands in the next `go build`.
const repoRoot = resolve(root, "..", "..");
const dist = resolve(repoRoot, "internal", "pluginbundle", "embed");
const staging = resolve(dist, "staging", "clawvisor");

// Resolve a version string that matches the checked-out tree. Prefer the
// CLAWVISOR_VERSION env var (set by the Makefile / CI) so the plugin and
// the server binary always agree; fall back to `git describe` so local
// bundling works without the Makefile.
function resolveVersion() {
  if (process.env.CLAWVISOR_VERSION) return process.env.CLAWVISOR_VERSION;
  try {
    const raw = execFileSync("git", ["describe", "--tags", "--always", "--dirty"], {
      cwd: root,
      encoding: "utf8",
    }).trim();
    return raw.replace(/^v/, "");
  } catch {
    return "dev";
  }
}

async function rmrf(path) {
  await fs.rm(path, { recursive: true, force: true });
}

async function main() {
  const version = resolveVersion();
  console.log(`→ bundling @clawvisor/openclaw-plugin version=${version}`);

  // Preserve the `.gitkeep` (or NOTE.md) sentinel so go:embed still sees a
  // non-empty directory even if a future run produces no tarball. Only
  // purge the staging and prior build artifacts.
  await rmrf(resolve(dist, "staging"));
  for (const name of await fs.readdir(dist).catch(() => [])) {
    if (name.startsWith("openclaw-plugin")) {
      await fs.unlink(resolve(dist, name));
    }
  }
  await fs.mkdir(staging, { recursive: true });

  // 1. Bundle index.ts → staging/index.js. esbuild handles TS stripping and
  //    inlines @sinclair/typebox; openclaw/* stays external.
  await build({
    entryPoints: [resolve(root, "index.ts")],
    outfile: resolve(staging, "index.js"),
    bundle: true,
    platform: "node",
    format: "esm",
    target: "node20",
    external: ["openclaw", "openclaw/*"],
    sourcemap: false,
    legalComments: "none",
    logLevel: "info",
  });

  // 2. Drop in the manifest + a minimal package.json + README + VERSION.
  //    The packaged package.json carries NO dependencies because the bundle
  //    inlined them — `npm install` in the OpenClaw plugin dir is a no-op.
  await fs.copyFile(
    resolve(root, "openclaw.plugin.json"),
    resolve(staging, "openclaw.plugin.json"),
  );
  await fs.copyFile(resolve(root, "README.md"), resolve(staging, "README.md"));
  await fs.writeFile(resolve(staging, "VERSION"), version + "\n", "utf8");
  // `openclaw.extensions` is required by `openclaw plugins install` —
  // without it, the installer rejects the tarball as "not an OpenClaw
  // plugin." Points at the bundled entrypoint.
  await fs.writeFile(
    resolve(staging, "package.json"),
    JSON.stringify(
      {
        name: "@clawvisor/openclaw-plugin",
        version,
        private: true,
        type: "module",
        main: "index.js",
        description:
          "OpenClaw plugin that connects to a cloud Clawvisor instance for authorized access to external services",
        openclaw: {
          extensions: ["./index.js"],
        },
      },
      null,
      2,
    ) + "\n",
    "utf8",
  );

  // 3. Tar it up. Leading path segment "clawvisor/" means the consumer runs
  //    `tar xz -C ~/.openclaw/plugins` and gets ~/.openclaw/plugins/clawvisor/.
  //
  //    We intentionally don't normalize mtimes here — BSD tar (macOS) and
  //    GNU tar (Linux/CI) differ on flag support, and the embedded hash
  //    changing on every rebuild is harmless (Go's go:embed only rebuilds
  //    the binary when bytes change). If reproducibility becomes important
  //    later, swap in `npm:tar` which is cross-platform.
  const stagingRoot = resolve(dist, "staging");
  const tarPath = resolve(dist, "openclaw-plugin.tgz");
  execFileSync(
    "tar",
    ["--create", "--gzip", "--file", tarPath, "--directory", stagingRoot, "clawvisor"],
    { stdio: "inherit" },
  );

  // 4. SHA-256 file next to the tarball — served over HTTP so users can
  //    verify the download without a second toolchain.
  const buf = await fs.readFile(tarPath);
  const sha = createHash("sha256").update(buf).digest("hex");
  const shaPath = resolve(dist, "openclaw-plugin.sha256");
  await fs.writeFile(shaPath, `${sha}  openclaw-plugin.tgz\n`, "utf8");

  // 5. Version-pinned copy, so consumers that want a stable URL can pin
  //    against exact `clawvisor@version` rather than "whatever the server
  //    happens to have." Same bytes as the moving target.
  const pinnedPath = resolve(dist, `openclaw-plugin-v${version}.tgz`);
  await fs.copyFile(tarPath, pinnedPath);

  // 6. Top-level VERSION file — lets the Go pluginbundle package compare
  //    versions without un-taring. Cheap read, handy at pair time.
  await fs.writeFile(resolve(dist, "VERSION"), version + "\n", "utf8");

  await rmrf(stagingRoot);
  console.log(`✓ ${tarPath}`);
  console.log(`  ${shaPath} (${sha})`);
  console.log(`  ${pinnedPath}`);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
