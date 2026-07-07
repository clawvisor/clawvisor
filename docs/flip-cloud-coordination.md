# Default flip — downstream coordination note

The fresh-install default changed from the skill-based gateway to proxy-lite
in the Observe posture. This note is for anyone consuming this repo as a
submodule/vendored dependency (including Clawvisor Cloud) and gating
proxy-lite on their own layer. The detailed, deployment-specific checklist
for Clawvisor Cloud lives in that (private) repo alongside its bump commit.

## What the flip actually changes

The flip is **writer-side only**:

- `internal/setup/setup.go` — the interactive wizard now recommends Observe.
- `internal/daemon/setup.go` — stamps `config_schema: 2` (already wrote
  `proxy_lite.enabled: true`).
- `internal/api/handlers/installer.go` + `installer_scripts/*.tmpl` — the
  per-harness install scripts route LLM traffic by default; `route=skill-only`
  is the opt-out; `route=subscription` handles OAuth seats.
- `pkg/config` — `Config` gains one **additive** field, `ConfigSchema int`
  (`config_schema`), plus the `CurrentConfigSchema` const. `Default()` is
  **unchanged**: `ProxyLite.Enabled` stays `false` permanently
  (`TestFlipDefaultStaysFalse`).

`Default()` not moving is the load-bearing invariant: a downstream deployment
that sets `proxy_lite.enabled` explicitly in its own config is never affected
by this repo's compiled default. No route paths moved in the flip.

## What downstream integrations should verify at bump time

1. **Compile against the new `pkg/config`.** The `ConfigSchema` field is
   additive; config unmarshalling is unaffected. Run your build + full suite
   on the bump.
2. **Re-verify any hand-maintained route allow-lists** against
   `internal/api/server.go registerLiteProxyRoutes`. No routes moved, but
   allow-lists drift silently by nature.
3. **Per-user or per-tenant proxy-lite gates are unaffected by construction**
   — the flip changes what fresh OSS installs write to their own config; it
   cannot enroll users into a downstream gate. Confirm with a test that an
   un-enrolled user is still denied after the bump.
4. **Governance broadening is real, on purpose.** Governance callbacks fire
   only on the proxy-lite path, so post-flip they cover more traffic. If you
   wire `OrgGovOptions`, assert this as a named scenario rather than letting
   it happen silently.
5. **Subscription seats.** Observe passthrough forwards a Claude
   subscription/OAuth bearer unchanged; Govern refusal returns
   `SUBSCRIPTION_SEAT_NOT_GOVERNABLE`. The pipeline in this repo owns that
   behavior — confirm your proxy edge does not rewrite
   `Authorization`/`anthropic-beta`.

## Merge ordering

An OSS flip bump and any downstream companion change should merge in the same
window. Submodule SHA-pinning makes ordering safe, but do not leave them split
across a release.

## Real-client evidence

The flip gate requires one recorded real-client run (deterministic +
Playwright lanes hard-gate; the tmux real-client lane is advisory).

- Command: `CLAWVISOR_REALCLIENT=1 ANTHROPIC_API_KEY=<key> go test
  ./e2e/realclient -tags realclient -run TestRealClaudeAttribution -v`
- Outcome (2026-07-06, two runs incl. one retry): **the automated tmux lane
  did not attribute a session within 90s in either run** — the known
  flakiness of this weekly-shifting-TUI lane, which spec 08 / PRD §11
  explicitly makes advisory (NOT a hard gate). The proxy-lite server side
  worked in both runs: the agent registered (`POST /api/agents` 201), the
  key vaulted, and the server served `/ready` and the API. The gap was
  entirely in the tmux-driven Claude Code TUI not completing a turn (no
  `/v1/*` request reached the proxy) — the lane's documented failure mode,
  not a flip regression. The hard-gating real-client proof for the flip is a
  **manual** recorded run per §11; the deterministic + Playwright lanes
  remain the binding automated gates.
