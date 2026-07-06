# End-to-end test lanes

Clawvisor has four e2e lanes. Two exist and are described here for
orientation; two were added by the default-flip gate work (spec 07).

| Lane | Directory | Runs | Gate? |
|---|---|---|---|
| Smoke / scenarios | `e2e/smoke`, `e2e/scenarios` | every PR (`go test ./...`, `make test-e2e`) | yes |
| Install (Docker) | `e2e/install` | every PR (`e2e-install.yml`) | yes |
| **Browser (Playwright)** | `e2e/browser` | every PR (`ci.yml` → `browser` job) | yes |
| **Real-client (tmux)** | `e2e/realclient` | scheduled + dispatch (`realclient.yml`) | **advisory** |

## Browser-harness prohibition (non-negotiable)

The browser-harness CDP tool is ONLY for agent-driven interactive verification
while developing (exploring a flow, reproducing a report, checking a change in
a live session). It must **never** appear in CI, in a Makefile target, or in any
committed automated test script. **Playwright is the only framework for
automated browser testing.** Do not add browser-harness invocations anywhere
under `e2e/`, the `Makefile`, or `.github/workflows/`.

## Browser lane (`e2e/browser`) — Playwright

Drives every human-session flow (magic-link login, dashboard render, key vault,
approvals, policy, agents) against a freshly built server + real frontend.

- Own npm package (`e2e/browser/package.json`, `@playwright/test` pinned to
  **1.52.0** to match the install lane — bump both together).
- `serve/main.go` boots a real `clawvisor-server` subprocess with proxy-lite
  enabled and the frontend served from `web/dist`. It mirrors `e2e/testapp`'s
  config template + port-collision retry, and prints one JSON readiness line.
- `global-setup.ts` spawns the server, logs in via the two magic-link API calls
  (same as `e2e/testapp/fixture.go`), and saves `storageState.json`.
- Specs seed via the HTTP API only — never by writing SQLite.

Run locally:

```bash
make test-e2e-browser
# or, against a prebuilt binary:
cd web && npm run build            # the browser lane serves the real frontend
go build -o bin/clawvisor-server ./cmd/clawvisor-server
cd e2e/browser && npm ci && npx playwright install chromium
CLAWVISOR_BIN=$PWD/../../bin/clawvisor-server npx playwright test
```

> **Node version:** use Node 20 (the version CI pins). Playwright 1.52.0's TS
> loader does not run under very new Node majors (e.g. Node 26). If `npx
> playwright test` hangs at startup with a `module.register()` deprecation
> warning, you are on an unsupported Node — switch to Node 20.

`verify_dashboard.mjs` lives at `e2e/browser/scripts/verify_dashboard.mjs` — a
single source consumed by both this lane (superseded by
`auth-magic-link.spec.ts` on PRs) and the install lane (which validates the
installed artifact inside Docker).

## Real-client lane (`e2e/realclient`) — tmux

Boots real Claude Code / Codex TUIs in tmux against a live proxy-lite server and
asserts server-side records (attribution, key injection, tool-use holds,
approval round-trips). **Advisory, not a merge gate** (PRD §11/§12): it drives
weekly-shipping agent TUIs by pane assertion and will be chronically flaky;
hard-gating on it would stall or quietly soften the flip gate.

- Go tests behind the `realclient` build tag — excluded from `go test ./...`.
- Every test additionally skips unless `CLAWVISOR_REALCLIENT=1` **and** the
  needed key is set (`ANTHROPIC_API_KEY` for claude, `OPENAI_API_KEY` for codex)
  and the client binary is on `PATH`.
- Assertions poll with deadlines (never fixed sleeps) and dump the full tmux
  pane on timeout.

Run locally (costs real API money — cheapest models, single-shot prompts):

```bash
npm install -g @anthropic-ai/claude-code @openai/codex   # clients on PATH
go build -o bin/clawvisor-server ./cmd/clawvisor-server
CLAWVISOR_REALCLIENT=1 ANTHROPIC_API_KEY=sk-ant-... \
  CLAWVISOR_BIN=$PWD/bin/clawvisor-server \
  go test -tags realclient -v -timeout 30m ./e2e/realclient/
```

Skips cleanly with no keys:

```bash
go test -tags realclient ./e2e/realclient/   # all scenarios SKIP
```

In CI it runs only via `.github/workflows/realclient.yml` (Mondays 06:00 UTC +
`workflow_dispatch`), with keys from repo secrets `E2E_ANTHROPIC_API_KEY` /
`E2E_OPENAI_API_KEY`. Never on PRs (secret exposure), never wired into `ci.yml`.

### Status & known caveats (read before running)

This lane is **wired, gated, compiling, and skip-clean**, but treat it as
**manually-runnable / best-effort** rather than reliably green. It drives
weekly-shipping agent TUIs by tmux pane assertion — the classic flaky surface
the advisory (non-gating) status exists for.

- **Attribution assertion is on real endpoints.** Attribution polls
  `GET /api/audit?agent_id=` (proxy-lite writes one audit row per `/api/v1/*`
  request with the agent id); approvals use `GET /api/approvals` +
  `POST /api/approvals/{request_id}/approve|deny`. (An earlier draft targeted
  `/api/runtime/{sessions,approvals,tool-controls}`, which do not exist — fixed.)
- **Claude Code onboarding drifts by version.** `claudeConfigDir` pre-seeds
  `settings.json` to skip first-run onboarding, but the exact keys track the
  installed `claude` major. Against **claude 2.1.185** a smoke run showed the
  TUI parked on an onboarding/theme screen and never reached the input box, so
  the prompt was not submitted. If a run times out with the pane showing the
  welcome banner but no input, update the onboarding-skip keys and the
  `claudeReady` landmark regex for your installed version.
- **`--dangerously-skip-permissions`** is passed so Claude Code's *own* tool
  prompts don't block the tool_use — Clawvisor's task-approval policy is what
  gates it (the behavior under test).
- **Codex is unverified.** No OpenAI key was available to smoke it, and the real
  `clawvisor agent lite -- codex` path routes Codex through injected
  `-c model_providers.clawvisor.*` TOML + a `CLAWVISOR_AGENT_TOKEN` header
  (see `internal/clawvisorcli/cmd_agent_lite.go`), not a bare `OPENAI_API_KEY`
  bearer as `codexEnv` currently approximates. Prefer wiring the Codex scenarios
  through the CLI when validating them.
- **On any pane-assertion timeout** the full ANSI-stripped pane is dumped to the
  test log for diagnosis.
