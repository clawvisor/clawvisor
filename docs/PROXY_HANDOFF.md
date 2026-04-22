# Network Proxy E2E Effort — Handoff State (2026-04-22)

**Branch**: `ericlevine/integration-spec` in `clawvisor-public/zurich`
**Tip**: `41e74068` (loopback CORS fix)
**Feature flag**: `CLAWVISOR_NETWORK_PROXY=1` gates everything we built

---

## What shipped on this branch

### Proxy-side hardening (third_party/proxy submodule)

- **Three-tier agent attribution** — `verified | labeled | anonymous` based on `(label, token)` in `Proxy-Authorization`. Bans only enforce on `verified`. (`78153587`, `c2ac0ccf`)
- **WebSocket / upgrade request support** — skip body-tee + credential injection on upgrade requests (breaks `http.Transport`'s 101 handoff otherwise). Connection-header list-value parsing catches `keep-alive, Upgrade`. (`3ede5672`, `8c2daad2`, `8e2b182`)
- **Credential-injection log leak fix** — snapshot URL/headers pre-injection so the traffic log never contains secrets. (`8e2b182`)
- **Bun-compatible `HTTP_PROXY` URL shape** — never emit empty-half userinfo; repeat label or use `agent:` synthetic user. (`c2ac0ccf`)

### Daemon CLI + endpoints (`cmd/clawvisor-local`, `internal/local/`)

- **Proxy commands moved** from `clawvisor` to `clawvisor-local`. Cloud users install only the daemon binary. (`61dfe6ce`)
- **Top-level commands regrouped** under `services` / `daemon` / `proxy` parents. (`bbcf7d79`)
- **`proxy run` runtime coverage**: auto-label from `args[0]`, env-var sweep (npm / curl / git / AWS / Deno), Node fetch shim (undici `setGlobalDispatcher`), macOS `.app` dispatch for Electron / Chromium apps. (`d3399df1`, `9851eaa6`, `3d9bd739`)
- **`Enable()` reconciliation bug fix** — persisted `enabled=true` was a silent no-op when proxy already running across a daemon restart. (`c2ac0ccf`)
- **CA path fallback fixed** — was `~/.clawvisor/proxy-data`, now `~/.clawvisor/local/proxy-data`. (`da60b5e0`)
- **New daemon endpoints**: `POST /api/proxy/trust-ca`, `POST /api/proxy/install-binary`. Shared helpers in `internal/local/proxy/{trustca.go,binaryfetch.go}`. (`bde856b5`, `ba4ede41`)
- **Loopback CORS auto-allow** on the daemon — any `http(s)://127.0.0.1|localhost|[::1]` origin is trusted, no pairing required. (`41e74068`)

### Server-side (main repo)

- **`TurnEvent.AgentAttribution` field + migration 041.** Ingest validation. (`78153587`)
- **Server-hosted proxy binary download endpoint** `GET /api/proxy/download?platform=…` (dev-only, opt-in via `$CLAWVISOR_PROXY_BINARY_DIR`, loud "DELETE BEFORE MERGE" comment on the handler). (`530529d2`, `bd67c18c`)
- **`CredentialHandler.Close()`** called during graceful shutdown so Ctrl+C doesn't hang on the invalidations SSE hub. (`df2120b3`)
- **Feature flag plumbing**: `FeatureSet.NetworkProxy` bool, env-var read in `pkg/clawvisor/defaults.go`, surfaced to frontend via `/api/features`. (`28f1c77a`)

### Dashboard (`web/src/pages/Proxies.tsx`, `Dashboard.tsx`)

- **First-class Proxies tab + Vault nav entries**, both gated behind `features.network_proxy`. (`28f1c77a`)
- **Persona-driven default install tab** (cloud → daemon, self-host → docker) + plain-English explainer.
- **Mint-enforcement-token UI** button (calls `/api/agents`). (`f6a7ff7b`)
- **One-click Connect orchestration** — "Connect proxy" button walks through binary download → configure + start → trust-CA → "wrap your agent" guidance, with per-step status and retry. (`3d5fb386`)

---

## What's working end-to-end (validated by hand)

With `CLAWVISOR_NETWORK_PROXY=1 CLAWVISOR_PROXY_BINARY_DIR=$(pwd)/third_party/proxy/dist make run`:

- ✅ curl, git, `gh` CLI, Python `requests`, Go `net/http`
- ✅ Claude Code (Bun)
- ✅ Codex (Rust WebSockets via `tokio-tungstenite`; intermittent "Attack attempt detected" on first ~5 connects, then works — WAF graylist on MITM TLS fingerprint)
- ✅ Claude Cowork (Electron)
- ✅ Dashboard one-click Connect — binary install, configure + start, trust-CA (keychain prompt)
- ✅ Bridge pairing via OpenClaw plugin (unchanged from before)

---

## What's blocked on the cloud repo

The Settings → "Pair local computer" flow depends on `/api/daemon/pair` + `/api/daemon/services/enabled` + related routes that **only exist in the cloud repo** (plugged in via `WithExtraRoutes` at server boot). The open-source build cannot serve these regardless of env vars — the handler code isn't in this tree.

Implication: on a fully open-source `make run`, the "Pair local computer" card in Settings would 404 on click. We keep `local_daemon` off by default to hide it. The proxy Connect flow doesn't require pairing thanks to the loopback-CORS auto-allow landed in `41e74068`.

**Action items for the cloud repo**:

1. The cloud endpoints (`/api/daemon/pair`, `/api/daemon/services/enabled`, etc.) already exist there — no changes needed on the pair handler itself.
2. **Add `FeatureSet.NetworkProxy` plumbing in the cloud build** — mirror the `os.Getenv("CLAWVISOR_NETWORK_PROXY")` read in whatever the cloud's equivalent of `pkg/clawvisor/defaults.go` is, OR gate it on a GrowthBook flag / cloud-side config.
3. **Expose `/api/proxy/download`** via `WithExtraRoutes` if the cloud wants dev-path binary distribution (optional — prod should use CI release artifacts).

---

## What's deferred (not on this branch)

- **Phase 2 — Docker auto-run via daemon.** `POST /api/proxy/docker-run` endpoint + dashboard button with preflight checks (`docker` binary present, daemon running). Scoped but not built.
- **Phase 3 — `clawvisor-local proxy docker-compose` CLI subcommand** that templates a yaml from persisted config. Scoped but not built.
- **Transcript dedupe bug**: `event_id` derivation ignores `tool_calls` / `tool_results`, collapsing distinct turns on ingest. Server-side dedupe at `internal/api/handlers/proxy.go:412` globalizes on `event_id`. Requires redesign across `parsers.go`, `parsers_openai.go`, and the server dedupe scope. High priority, own PR.
- **Linux `trust-ca` UX**: daemon's `/api/proxy/trust-ca` shells out to `sudo` which fails without a terminal. Dashboard should fall back to a copy-paste command on Linux; currently surfaces the shell error directly.

---

## Known dev caveats

- **`/api/proxy/download` endpoint is dev-only**, opt-in via `$CLAWVISOR_PROXY_BINARY_DIR`. Endpoint + its `--from-server` CLI flag should be deleted entirely once CI publishes per-platform release artifacts (handler has a "DELETE BEFORE MERGE" banner pointing at this).
- **WAF-style "Attack attempt detected"** on first few Codex connects. Not our bug — OpenAI's WAF graylists new TLS fingerprints from a MITM. Works after ~5 retries.
- **Claude Code bundled Bun**: works today but Anthropic pins their own Bun version. Older Bun rejects userinfo in proxy URLs (fixed in ~1.3.x). If Claude Code regresses to an older Bun, our label-based attribution breaks and we'd need to fall back to anonymous tier for their users.

---

## Testing recipe (on this repo / branch)

```bash
make build-proxy                               # build proxy binary
make install-local                             # install clawvisor-local with new endpoints
clawvisor-local daemon restart                 # pick up new HTTP handlers

# In one terminal:
CLAWVISOR_NETWORK_PROXY=1 \
CLAWVISOR_PROXY_BINARY_DIR=$(pwd)/third_party/proxy/dist \
make run

# In another terminal (optional — for web hot-reload):
cd web && npm run dev -- --port 8080 --host 127.0.0.1
```

Browse to `http://127.0.0.1:25297` (or `:8080` if using Vite), log in via magic link, go to **Proxies** → **Add proxy** → **Enable Proxy** → click the green **Connect proxy** button. Expect three checkmarks + keychain prompt. Then verify:

```bash
clawvisor-local proxy run -- curl -sv https://api.anthropic.com/v1/messages \
  -H "x-api-key: test" -d '{}'
tail -f ~/.clawvisor/local/proxy-data/logs/traffic-$(date +%Y-%m-%d).jsonl
```

---

## Commit chain (chronological, this effort)

```
41e74068 fix(daemon): auto-allow loopback CORS origins, drop local_daemon flip
5440f6a1 feat(features): surface local_daemon flag via env var      [partial revert in 41e74068]
3d5fb386 feat(proxies/web): one-click Connect orchestration
ba4ede41 feat(daemon): add /api/proxy/trust-ca and /api/proxy/install-binary
bde856b5 refactor(proxy-local): extract TrustCA + DownloadBinaryFromServer
da60b5e0 fix(proxy-local): CA cert fallback path matches daemon layout
8e2b182  fix(proxy): secrets + upgrade-header robustness
bd67c18c docs(proxy-download): loud banner on the dev-only binary endpoint
df2120b3 fix(server): close credential invalidations SSE hub on shutdown
28f1c77a feat(features): gate Network Proxy UX behind network_proxy flag
3ede5672 chore: bump proxy submodule (skip body tee on WebSocket upgrades)
8c2daad2 chore: bump proxy submodule (WebSocket upgrade fix)
c2ac0ccf fix(proxy): two bugs surfaced during E2E testing
3d9bd739 feat(proxy/run): macOS .app dispatch for Electron / Chromium agents
9851eaa6 feat(proxy/run): inject Node fetch shim so undici routes through proxy
d3399df1 feat(proxy/run): broaden runtime coverage + auto-derive label
61dfe6ce refactor(proxy): move proxy commands from clawvisor to clawvisor-local
bbcf7d79 refactor(clawvisor-local): regroup top-level commands
530529d2 feat(proxy): server-hosted binary download for dev iteration
f6a7ff7b feat(proxies/web): mint-enforcement-token UI on the install panel
78153587 feat(proxy): three-tier agent attribution end-to-end
```
