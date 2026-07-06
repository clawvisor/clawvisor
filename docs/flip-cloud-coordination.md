# Default flip — cloud submodule-bump coordination

Companion review required by spec 08 (`docs/terraform-centric/08-default-flip.md`).
This doc lives in the OSS repo and describes what the cloud repo
(`clawvisor/cloud`) assumes about proxy-lite being opt-in, what must change (and
what must NOT) when the cloud submodule is bumped to the OSS flip SHA, and the
named test scenarios the cloud bump must add.

Reviewed against the cloud repo at bump time:
`cmd/cloud/proxy_lite_gate.go`, `cmd/cloud/main.go`
(`disableProxyLiteFeatures`, `WrapRoutes`), and
`internal/proxylite/access.go` (the `proxy_lite_users` per-user table).

## What the OSS flip actually changes

The flip is **writer-side only** and lands entirely in surfaces the cloud does
not use for its SaaS onboarding:

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

`Default()` not moving is the load-bearing invariant: cloud sets
`proxy_lite.enabled` explicitly in its own server config, so the OSS default
never decides anything for a cloud deployment. No OSS route paths moved.

## What cloud assumes about opt-in (and why the flip does not break it)

Cloud gates proxy-lite along **two independent axes**, neither of which the OSS
writer-side flip touches:

1. **Server-level toggle** — `cmd/cloud/main.go` mounts the proxy access gate
   only when `opts.Config.ProxyLite.Enabled` is true (`WrapRoutes`, ~L410).
   This reads cloud's *own* config file, not the OSS `Default()`. Unaffected by
   the flip.

2. **Per-user allow-list** — `proxyLiteAccessGate` (`proxy_lite_gate.go`)
   classifies each request and, for a resolved user/agent, calls
   `proxylite.IsUserEnabled(...)` against the `proxy_lite_users` table. A user
   not in the table gets `403 PROXY_LITE_DISABLED` via
   `writeProxyLiteGateError`. The three classifiers are:
   - `isProxyLiteUserRoute` — JWT user routes (`/api/agents/connect/claim`,
     `/api/runtime/llm-credentials[...]`, `/api/vault/items[...]`).
   - `isProxyLiteAgentTokenRoute` — `cvis_` agent-token routes
     (`/v1/messages`, `/v1/messages/count_tokens`, `/v1/chat/completions`,
     `/v1/responses`, `/api/agent/vault/items`).
   - `isProxyLiteNonceRoute` — `/proxy/v1/*` and `/control/*` (excluding
     `/control/capabilities` and `/control/skill`).

   The OSS flip **cannot** enroll a cloud user: nothing in the wizard/installer
   writes to `proxy_lite_users`, and cloud users never run the OSS wizard.
   Enrollment stays exclusively `EnableUserByEmail` (admin action).

Additionally, `disableProxyLiteFeatures` (`main.go` FeaturesHook) strips
`FeatureSet.ProxyLite` (and, when `RuntimeProxy.Enabled` is false, the vault /
runtime-policy / activity / live-session / service-preset surfaces) for any
user who is not enabled — so the dashboard never advertises proxy-lite to a
gated user even after the flip.

## What must change at submodule-bump time

Structurally: **nothing in the gate.** The bump is mechanical because cloud
pins OSS by SHA and the `pkg/config` change is additive. The required work is
verification + one new test area:

1. **Compile against the new `pkg/config`.** The added `ConfigSchema int` field
   is additive; cloud's config unmarshal is unaffected. Run cloud's build +
   full suite on the bump before merging OSS to main.
2. **Re-verify the three route classifiers** in `proxy_lite_gate.go` still
   cover the OSS lite-proxy route surface exactly (no OSS routes moved in the
   flip, but the classifiers are hand-maintained allow-lists — confirm against
   `internal/api/server.go registerLiteProxyRoutes`).
3. **Confirm `disableProxyLiteFeatures` still runs** on both the
   unauthenticated and authenticated FeaturesHook paths (`main.go` ~L305/L335).
4. **Governance broadening is now real, on purpose.** OrgGov callbacks
   (`CheckModelPolicy` / `CheckSpendCap` / `ScanContentPolicy` /
   `RecordViolation`, wired in `main.go` ~L458) fire **only on the proxy-lite
   path** (OSS gotcha #3). Post-flip, more org agents route through proxy-lite,
   so org policy is enforced for more traffic — but still **only** for users
   the per-user gate admits. A no-org agent degrades to nil callbacks
   (unaffected). This must be a named, asserted scenario, not a silent effect.
5. **Subscription seats.** Observe passthrough forwards a Claude
   subscription/OAuth bearer unchanged (OSS spec 02 §4c). Cloud must not strip
   or rebill it; Govern refusal returns `SUBSCRIPTION_SEAT_NOT_GOVERNABLE`.
   No cloud change required — the OSS pipeline owns this — but the cloud bump
   should confirm its proxy edge does not rewrite `Authorization`/`anthropic-beta`.

## Named test scenarios the cloud bump needs

Add under cloud's proxy-lite test package (verbatim names, per spec 08):

- `TestGateStillDeniesUnenabledUser` — with OSS default flipped, a user NOT in
  `proxy_lite_users` still gets `403 PROXY_LITE_DISABLED` on `/v1/messages` and
  `/proxy/v1/*`. Exercise every classifier in `proxy_lite_gate.go`
  (user-route, agent-token-route, nonce-route).
- `TestDisableProxyLiteFeatureSet` — FeatureSet stripping still applies for
  unenabled users on both FeaturesHook paths (`disableProxyLiteFeatures`).
- `TestFlipBroadensGovernance` — an org agent newly routed through proxy-lite:
  model-policy deny, spend-cap warn + hard block, and content-policy block each
  fire and record a violation; the same requests for a no-org agent are
  unaffected (nil-callback degrade).
- Re-run cloud's **existing** proxy-lite suite unchanged (gate classifiers,
  nonce flow, credential/vault routes) — it must stay green.

## Merge ordering

OSS flip branch and the cloud companion PR merge in the same window. Cloud pins
OSS by submodule SHA, so ordering is safe, but do not leave them split across a
release. Merge OSS → main only after the OSS deterministic + Playwright lanes
are green, the recorded real-client run is attached (see the flip PR / this
repo's report), and the cloud companion PR (this checklist walked, new
scenarios green, existing suite green) is approved.

## Real-client evidence

The flip gate requires one recorded real-client run (deterministic + browser
lanes hard-gate; the tmux real-client lane is advisory). The recorded run for
this branch is noted in the flip PR report:

- Command: `CLAWVISOR_REALCLIENT=1 ANTHROPIC_API_KEY="$CLAWVISOR_ANTHROPIC_E2E_KEY"
  go test ./e2e/realclient -tags realclient -run TestRealClaudeAttribution -v`
  (claude CLI at `/Users/ericlevine/.superset/bin/claude`, tmux installed; the
  key is mapped inline, never echoed or committed).
- Outcome (2026-07-06, two runs incl. one retry): **the automated tmux lane did
  not attribute a session within 90s in either run** — the known flakiness of
  this weekly-shifting-TUI lane, which spec 08 / PRD §11 explicitly makes
  advisory (NOT a hard gate). The proxy-lite server side worked in both runs:
  the agent registered (`POST /api/agents 201`), the Anthropic key vaulted
  (`lite-proxy: llm credential created`), and the server served
  `/ready`/`/api`. The gap was entirely in the tmux-driven Claude Code TUI not
  completing a turn (no `/api/v1/*` request reached the proxy) — the lane's
  documented failure mode, not a flip regression. The hard-gating real-client
  proof for the flip is a **manual** recorded run per §11; the deterministic +
  Playwright lanes remain the binding automated gates.
