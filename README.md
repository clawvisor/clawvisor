<p align="center">
  <img src="web/public/favicon.svg" alt="Clawvisor" width="96" height="96" />
</p>

<h1 align="center">Clawvisor</h1>

<p align="center">
  <strong>Your agents act. You stay in control.</strong><br/>
  The gatekeeper between your AI agents and the APIs they act on.
</p>

<p align="center">
  <a href="#get-started">Get Started</a> · <a href="#cli-reference">CLI</a> · <a href="#agent-integration">Agent Integration</a> · <a href="#dashboard">Dashboard</a> · <a href="#supported-services">Services</a> · <a href="#runtime-proxy-preview">Runtime Proxy</a> · <a href="#security-model">Security</a>
</p>

---

> [!WARNING]
> **Use at your own risk.** Clawvisor is experimental software under active development. It has not been audited for security. LLMs are inherently nondeterministic — we make no guarantees that policies or safety checks will behave as expected in every case. Do not use Clawvisor as your sole safeguard for sensitive data or critical systems.

---

AI agents are getting good at doing things. The problem is letting them — safely. Give an agent your Gmail credentials and it can read, send, and delete anything. Refuse, and it can't help you at all.

Clawvisor sits in the middle. Agents never hold credentials. Instead, they declare **tasks** describing what they need to do, the user approves the scope, and Clawvisor handles credential injection, execution, and audit logging for every request under that task.

The typical flow: a user asks their agent to triage their inbox. The agent creates a Clawvisor task declaring what it needs — read emails, but ask before sending. The user approves the scope once, and the agent makes as many requests as it needs within that scope without further interruption. Actions outside the scope go to the user for per-request approval. Restrictions can hard-block specific actions entirely.

Approve a purpose, not a permission. Clawvisor enforces it on every request.

## Get Started

### Sign up (recommended)

The fastest way to try Clawvisor is hosted: sign up at [clawvisor.com](https://clawvisor.com). No installation required — connect services and agents directly from the dashboard.

### Self-host

Prefer to run Clawvisor yourself? Install the daemon:

```bash
curl -fsSL https://clawvisor.com/install.sh | sh
```

> [!WARNING]
> **Run your agent in a separate environment from Clawvisor** — a sandboxed container, separate machine, or cloud VM. If the agent shares a host with Clawvisor, it can bypass the gateway by reading the database or process environment directly.

This downloads the latest binary, adds it to your PATH, and starts an interactive setup wizard that walks you through configuration — database, LLM provider for intent verification, Google OAuth, and more. No Go, Node, or Docker required.

Once setup completes, the daemon opens the web dashboard in your browser automatically.

### Connect your agent

With Clawvisor running (hosted or self-hosted), connect your agent:

```bash
clawvisor connect-agent
```

This auto-detects installed agents (Claude Code, Claude Desktop) and walks you through connecting them. You can also target a specific agent directly:

```bash
clawvisor connect-agent claude-code      # install skill + env vars for Claude Code
clawvisor connect-agent claude-desktop   # configure MCP for Claude Desktop
```

For manual setup or other agents, see the integration guides: [Claude Code](docs/INTEGRATE_CLAUDE_CODE.md) · [Claude Desktop (MCP)](docs/INTEGRATE_CLAUDE_COWORK.md) · [OpenClaw](docs/INTEGRATE_OPENCLAW.md) · [Any HTTP agent](docs/INTEGRATE_GENERIC.md)

### Other self-host options

<details>
<summary>From source (Go + Node)</summary>

```bash
git clone https://github.com/clawvisor/clawvisor.git
cd clawvisor
make setup    # interactive config wizard
make run      # start server (SQLite, magic link auth)
make tui      # terminal dashboard (separate terminal)
```

`make setup` generates `config.yaml` and a `vault.key` (database, Google OAuth, intent verification). `make run` starts the server, creates an `admin@local` user, and opens the dashboard via a magic link. `make tui` auto-authenticates using the local session file.

Requires Go 1.25+ and Node.js 18+. See [docs/SETUP_LOCAL.md](docs/SETUP_LOCAL.md) for details.

</details>

<details>
<summary>Docker</summary>

```bash
git clone https://github.com/clawvisor/clawvisor.git
cd clawvisor
make up       # docker compose (app + postgres)
```

See [docs/SETUP_DOCKER.md](docs/SETUP_DOCKER.md) for details.

</details>

<details>
<summary>Cloud / remote server</summary>

See [docs/SETUP_CLOUD.md](docs/SETUP_CLOUD.md) for deploying to a VPS, container platform, or Cloud Run.

</details>

For the complete agent API protocol, see [`skills/clawvisor/SKILL.md`](skills/clawvisor/SKILL.md).

## CLI Reference

The `clawvisor` CLI is the primary interface for managing Clawvisor. Run `clawvisor install` on a fresh machine for a guided walkthrough that runs `setup`, `services`, `connect-agent`, and `dashboard` in sequence.

### Setup & installation

| Command | Description |
|---|---|
| `clawvisor install` | Guided first-run: configure, install as system service, start, connect services and agents, and open the dashboard. Use `--pair` to also pair a mobile device. |
| `clawvisor setup` | Interactive configuration wizard — LLM provider, relay, telemetry. Run this to reconfigure an existing install. |
| `clawvisor update` | Update to the latest release (or `--version <tag>` for a specific version). |
| `clawvisor uninstall` | Remove the system service. |

### Daemon lifecycle

| Command | Description |
|---|---|
| `clawvisor start` | Start the daemon as a background service. Use `--foreground` to run in the foreground. |
| `clawvisor stop` | Stop the running daemon. |
| `clawvisor restart` | Restart the daemon. |
| `clawvisor status` | Show whether the daemon is running. |

### Services

| Command | Description |
|---|---|
| `clawvisor services` | Interactive picker to connect downstream services (Gmail, GitHub, Slack, etc.). |
| `clawvisor services list` | List available and connected services. Use `--json` for machine-readable output. |
| `clawvisor services add [service]` | Connect a service by ID or name. Omit the argument for an interactive picker. |
| `clawvisor services remove <service>` | Disconnect a service. |

### Agent connection

| Command | Description |
|---|---|
| `clawvisor connect-agent` | Auto-detect installed agents and walk through connecting them. |
| `clawvisor connect-agent claude-code` | Install the `/clawvisor-setup` slash command and optionally add auto-approve rules. |
| `clawvisor connect-agent claude-desktop` | Configure the MCP connection for Claude Desktop. |

### Agent management

| Command | Description |
|---|---|
| `clawvisor agent create <name>` | Create an agent and print its bearer token. Use `--with-callback-secret` to generate a callback signing secret, `--replace` to overwrite an existing agent. |
| `clawvisor agent list` | List all agents. |
| `clawvisor agent delete <name-or-id>` | Delete an agent. |

### Dashboard & UI

| Command | Description |
|---|---|
| `clawvisor dashboard` | Open the web dashboard in your browser. Use `--no-open` to print the URL only. |
| `clawvisor tui` | Launch the terminal dashboard. Supports `--url` and `--token` overrides. |
| `clawvisor pair` | Pair a mobile device via QR code. |

### Other

| Command | Description |
|---|---|
| `clawvisor server` | Start the API server directly (used internally by `start`). Use `--open` to open the magic link on startup. |
| `clawvisor healthcheck` | Check if the server is ready — queries `/ready` on localhost. Used by Docker HEALTHCHECK. |

### Configuration

Clawvisor loads `config.yaml` from the working directory (override with `CONFIG_FILE` env var). Most settings have env var overrides:

| Setting | Env var | Notes |
|---|---|---|
| Database driver | `DATABASE_DRIVER` | `postgres` or `sqlite` (auto-selects sqlite locally) |
| Postgres URL | `DATABASE_URL` | Required for postgres driver |
| JWT secret | `JWT_SECRET` | Auto-generated locally; **required** in prod |
| Vault key | `VAULT_KEY` | Base64-encoded 32-byte key; alternative to `vault.key` file |
| Auth mode | `AUTH_MODE` | `magic_link` (default locally) or `password` |
| Allowed emails | `ALLOWED_EMAILS` | Comma-separated emails allowed to register |
| Google OAuth | `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` | Needed for Google services |
| Public URL | `PUBLIC_URL` | Used in Telegram notification links |
| LLM config | `CLAWVISOR_LLM_*` | Shared provider, model, API key for all LLM features |
| Intent verification | `CLAWVISOR_LLM_VERIFICATION_*` | Optional LLM check that request params match task purpose |
| Task risk assessment | `CLAWVISOR_LLM_TASK_RISK_*` | Optional LLM risk assessment when tasks are created |
| Chain context | `CLAWVISOR_LLM_CHAIN_CONTEXT_*` | Optional LLM extraction of facts from results for multi-step verification |
| Approval timeout | `APPROVAL_TIMEOUT` | Seconds before pending approvals expire (default 300) |
| Rate limits | `RATE_LIMIT_*` | Per-agent gateway, per-user OAuth, policy API, and review limits |
| MCP timeout | `MCP_APPROVAL_TIMEOUT` | Seconds MCP blocks waiting for approval (default 240) |
| Telemetry | `TELEMETRY_ENABLED` | Opt-in anonymous usage telemetry |

See [`config.example.yaml`](config.example.yaml) for the full configuration reference.

## How It Works

```
Agent                    Clawvisor                         External API
  │                         │                                   │
  ├── POST /api/tasks ─────►│  (declare scope, wait for user)   │
  │◄── task approved ───────┤                                   │
  │                         │                                   │
  ├── POST /api/gateway ───►│                                   │
  │   (with task_id)        ├─ Check restrictions               │
  │                         ├─ Check task scope                 │
  │                         ├─ Auto-execute or queue approval   │
  │                         │                                   │
  │                         │  ┌─ On execute: ──────────────┐   │
  │                         │  │  Inject credentials        │   │
  │                         │  │  Call adapter ────────────────►│
  │                         │  │  Format response  ◄────────────┤
  │                         │  │  Intent verification       │   │
  │                         │  │  Audit log                 │   │
  │                         │  └────────────────────────────┘   │
  │◄── Response ────────────┤                                   │
```

Every gateway request passes through three authorization layers, checked in order:

1. **Restrictions** — hard blocks you configure on specific service/action pairs. If a restriction matches, the request is denied immediately. Use these for actions you never want any agent to take (e.g. "no agent can delete calendar events").
2. **Task scopes** — the primary mechanism. When an agent needs to do something, it creates a task declaring the purpose and which service/action pairs it needs. You review and approve the scope once. After that, requests under that task execute without further interruption — you approved the purpose, not each individual call. Tasks can be session-scoped (expire after a TTL) or standing (persist until you revoke them).
3. **Per-request approval** — the fallback. Any request that isn't covered by a task scope goes to the approval queue, and you're notified via the dashboard, Telegram, or push notification to your phone. This is the default for actions the agent didn't declare upfront, or for task actions marked `auto_execute: false` (e.g. sending emails).

When a task is created, Clawvisor can optionally run an **LLM-powered risk assessment** that evaluates the scope breadth, purpose-scope coherence, and internal consistency of the task. The assessment produces a risk level (low, medium, high, critical) shown in the dashboard to help inform your approval decision. High and critical risk tasks require a confirmation step before approval.

For multi-step tasks, **chain context verification** tracks structural facts (IDs, email addresses, phone numbers) extracted from adapter results and feeds them into subsequent verification prompts. This prevents a compromised agent from reading an inbox and then targeting an entity not present in the results. Chain context is activated by passing a consistent `session_id` across related gateway requests.

All three LLM subsystems (intent verification, chain context extraction, and task risk assessment) are covered by eval suites totaling 249 cases. See [docs/eval-results.md](docs/eval-results.md) for accuracy breakdowns and failure analysis.

## Supported Services

| Service ID | Service | Actions |
|---|---|---|
| `google.gmail` | Gmail | `list_messages`, `get_message`, `send_message` |
| `google.calendar` | Google Calendar | `list_events`, `get_event`, `create_event`, `update_event`, `delete_event`, `list_calendars` |
| `google.drive` | Google Drive | `list_files`, `get_file`, `create_file`, `update_file`, `search_files` |
| `google.contacts` | Google Contacts | `list_contacts`, `get_contact`, `search_contacts` |
| `github` | GitHub | `list_issues`, `get_issue`, `create_issue`, `comment_issue`, `list_prs`, `get_pr`, `list_repos`, `search_code` |
| `slack` | Slack | `list_channels`, `get_channel`, `list_messages`, `send_message`, `search_messages`, `list_users` |
| `notion` | Notion | `search`, `get_page`, `create_page`, `update_page`, `query_database`, `list_databases` |
| `linear` | Linear | `list_issues`, `get_issue`, `create_issue`, `update_issue`, `add_comment`, `list_teams`, `list_projects`, `search_issues` |
| `stripe` | Stripe | `list_customers`, `get_customer`, `list_charges`, `get_charge`, `list_subscriptions`, `get_subscription`, `create_refund`, `get_balance` |
| `twilio` | Twilio | `send_sms`, `send_whatsapp`, `list_messages`, `get_message` |
| `apple.imessage` | iMessage | `search_messages`, `list_threads`, `get_thread`, `send_message` |

Google services share a single OAuth connection — activating one activates all four. GitHub, Slack, Notion, Linear, Stripe, and Twilio each use per-user API keys/tokens. iMessage reads the local `chat.db` on macOS and is always available without activation on supported machines.

## Agent Integration

### Create an agent token

In the dashboard, create an agent and copy the bearer token. The agent uses this for all API calls.

### Tasks

Tasks are the primary authorization mechanism. An agent declares what it needs to do — the purpose, the specific service/action pairs, and whether each can auto-execute — and the user approves the scope once. After that, every gateway request under that task runs without per-request approval (for `auto_execute` actions).

Without a task, every request goes to the approval queue individually.

#### Creating a task

```bash
curl -s -X POST "$CLAWVISOR_URL/api/tasks" \
  -H "Authorization: Bearer $AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "purpose": "Triage inbox and draft replies",
    "authorized_actions": [
      {"service": "google.gmail", "action": "list_messages", "auto_execute": true, "expected_use": "List recent emails to identify unread messages"},
      {"service": "google.gmail", "action": "get_message", "auto_execute": true, "expected_use": "Read individual emails to assess priority"},
      {"service": "google.gmail", "action": "send_message", "auto_execute": false}
    ],
    "expires_in_seconds": 1800,
    "callback_url": "https://your-agent/callback"
  }'
```

The task starts as `pending_approval`. The user approves it via the dashboard, Telegram, or a push notification on their phone. Include a `callback_url` to receive a callback when the task is approved or denied — otherwise poll `GET /api/tasks/{id}` (supports long-polling).

Key fields:

- **`purpose`** — shown to the user during approval and used by intent verification to check that requests are consistent with the declared intent
- **`auto_execute`** — `true` means in-scope requests execute immediately; `false` means they still go to per-request approval (useful for destructive actions like `send_message`)
- **`expected_use`** — per-action description of how the agent intends to use it, shown during approval and checked by intent verification
- **`expires_in_seconds`** — session tasks expire after this TTL once approved

#### Session vs standing tasks

**Session tasks** (the default) expire after their TTL. Use these for bounded work like "triage my inbox" or "review this PR."

**Standing tasks** have no expiry and remain active until the user revokes them. Use these for ongoing workflows:

```bash
curl -s -X POST "$CLAWVISOR_URL/api/tasks" \
  -H "Authorization: Bearer $AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "purpose": "Ongoing email triage",
    "lifetime": "standing",
    "authorized_actions": [
      {"service": "google.gmail", "action": "list_messages", "auto_execute": true},
      {"service": "google.gmail", "action": "get_message", "auto_execute": true}
    ]
  }'
```

#### Task lifecycle

1. **`pending_approval`** — created, waiting for user approval
2. **`active`** — approved, gateway requests with this `task_id` are authorized
3. **`pending_scope_expansion`** — agent requested a new action via `POST /api/tasks/{id}/expand`
4. **`completed`** — agent marked the task done via `POST /api/tasks/{id}/complete`
5. **`expired`** — session task TTL elapsed
6. **`revoked`** — user revoked the task from the dashboard
7. **`denied`** — user denied the task or scope expansion

#### Scope expansion

If the agent needs an action not in the original scope, it requests expansion:

```bash
curl -s -X POST "$CLAWVISOR_URL/api/tasks/<task-id>/expand" \
  -H "Authorization: Bearer $AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "service": "google.gmail",
    "action": "send_message",
    "auto_execute": false,
    "reason": "User asked me to reply to the triage summary"
  }'
```

The user is notified and can approve or deny. On approval, the action is added and the session task TTL is reset. Standing tasks cannot be expanded — create a separate task instead.

### Gateway requests

Once a task is active, the agent makes gateway requests under it:

```bash
curl -s -X POST "$CLAWVISOR_URL/api/gateway/request" \
  -H "Authorization: Bearer $AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "service": "google.gmail",
    "action": "list_messages",
    "params": {"max_results": 10, "query": "is:unread"},
    "reason": "Checking for unread messages to triage",
    "request_id": "req-001",
    "task_id": "task-uuid-here",
    "context": {
      "source": "user_message",
      "callback_url": "https://your-agent/callback"
    }
  }'
```

Requests without a `task_id` go to per-request approval automatically.

### Response statuses

| Status | Meaning |
|---|---|
| `executed` | Action completed. Result in `result.summary` and `result.data`. |
| `pending` | Awaiting human approval. Use `?wait=true` on the original POST to block until resolved, or call `POST /api/gateway/request/{request_id}/execute` after approval. |
| `blocked` | A restriction blocks this action. Do not retry. |
| `restricted` | Intent verification rejected the request. Adjust params/reason and retry with a new `request_id`. |
| `pending_task_approval` | Task declared but not yet approved by the user. |
| `pending_scope_expansion` | Action is outside the task scope. Call `POST /api/tasks/{id}/expand`. |
| `task_expired` | Task TTL elapsed. Expand to extend, or create a new task. |
| `error` | Execution failed. Check `error` field. `code: SERVICE_NOT_CONFIGURED` means the service needs activation. |

### Callbacks

Callbacks carry a `type` field (`"request"` or `"task"`) and use dedicated ID fields.

When a pending gateway request resolves, Clawvisor POSTs to the request's `callback_url`:

```json
{
  "type": "request",
  "request_id": "req-001",
  "status": "executed",
  "result": {"summary": "Found 3 unread messages", "data": [...]},
  "audit_id": "a8f3..."
}
```

When a task is approved, denied, expanded, or expires, Clawvisor POSTs to the task's `callback_url`:

```json
{
  "type": "task",
  "task_id": "task-uuid",
  "status": "approved"
}
```

Task callback statuses: `approved`, `denied`, `scope_expanded`, `scope_expansion_denied`, `expired`.

For the full agent protocol, see [`skills/clawvisor/SKILL.md`](skills/clawvisor/SKILL.md).

## Dashboard

The web UI provides:

- **Overview** — pending approvals queue, activity charts, and notification setup
- **Tasks** — view active/standing tasks, approve/deny/revoke task scopes, filterable by status
- **Services** — activate services via OAuth (Google) or API key (GitHub, Slack, Notion, etc.), re-authenticate, manage aliases
- **Restrictions** — toggle hard blocks on service/action pairs
- **Agents** — create agent tokens, manage connection requests
- **Gateway log** — searchable audit trail of every gateway request with outcomes and verification results
- **Settings** — device pairing (QR code for mobile), Telegram notification setup, password management, account settings
- **Onboarding** — guided first-run flow for connecting services, creating agents, and setting up notifications
- **Light/dark mode** — toggle in the sidebar; persists across sessions

## TUI

The terminal dashboard (`clawvisor tui`) provides the same approval and monitoring capabilities without leaving the terminal.

```bash
# After setup + server are running:
make tui

# Or directly:
clawvisor tui

# With explicit connection details:
clawvisor tui --url http://localhost:25297 --token <refresh_token>
```

Authentication is automatic in local mode. The flow:

1. `clawvisor server` writes `~/.clawvisor/.local-session` with a one-time magic token
2. `clawvisor tui` reads this file, exchanges the token via `POST /api/auth/magic`, and saves the resulting refresh token to `~/.clawvisor/config.yaml`
3. Subsequent launches use the saved refresh token directly

For password-mode servers, the TUI prompts for email and password on first launch.

You can also set credentials via environment variables:

```bash
export CLAWVISOR_URL=http://localhost:25297
export CLAWVISOR_TOKEN=<refresh_token>
clawvisor tui
```

## Daemon Mode

The daemon runs Clawvisor as a persistent background service. See [CLI Reference](#cli-reference) for the full list of commands (`start`, `stop`, `status`, `install`, etc.). Configuration and data are stored in `~/.clawvisor/`.

### Remote access via relay

When paired with a mobile device, the daemon connects to `relay.clawvisor.com` via a WebSocket reverse tunnel. This allows the iOS app to reach a local Clawvisor instance behind NAT without port forwarding. The daemon authenticates to the relay using Ed25519 challenge-response. All traffic is routed through `/d/<daemon_id>/api/...` on the relay.

### MCP integration

Clawvisor exposes an MCP (Model Context Protocol) server at `/mcp` with OAuth 2.1 for integration with Claude Desktop and other MCP clients. Tools available via MCP: `fetch_catalog`, `create_task`, `get_task`, `complete_task`, `expand_task`, `gateway_request`. See [docs/INTEGRATE_CLAUDE_COWORK.md](docs/INTEGRATE_CLAUDE_COWORK.md) for setup.

## Runtime Proxy (preview)

The runtime proxy is a TLS-terminating egress proxy that runs inside the Clawvisor daemon. While the gateway describes what an agent intends to do, the runtime proxy sees what it actually sends on the wire — so Clawvisor can observe model API calls, intercept tool-use, hold inline approvals, and attribute every request to a runtime session.

> [!WARNING]
> **The runtime proxy is in active development.** Behavior, flags, and the API surface may change in any release while it remains pre-1.0. Treat it as preview-quality and pin to a specific Clawvisor version in production.

It ships off by default. To enable, set `runtime_proxy.enabled: true` in `config.yaml` (or `CLAWVISOR_RUNTIME_PROXY_ENABLED=true`), restart the daemon, and run an agent through it:

```bash
clawvisor agent register my-agent
clawvisor agent run --agent my-agent -- claude
```

For configuration, Dockerized agents, container isolation, starter profiles, and observe-vs-enforce mode, see [docs/RUNTIME_PROXY.md](docs/RUNTIME_PROXY.md).

## Architecture

| Layer | Choice |
|---|---|
| Backend | Go 1.25+, `net/http` ServeMux |
| Frontend | Vite 7 + React 18 + TypeScript + Tailwind |
| Database | Postgres (prod) or SQLite (local), behind `Store` interface |
| Vault | AES-256-GCM with master key (env var or keyfile) or GCP Secret Manager, behind `Vault` interface |
| Auth | JWT (HS256), magic links (local), bcrypt passwords (prod) |
| Real-time | SSE event stream for instant dashboard updates |
| Notifications | Telegram (per-user bot tokens), push notifications (APNs via external push service) |
| Relay | WebSocket reverse tunnel for NAT traversal (connects to external relay service) |
| MCP | Model Context Protocol server with OAuth 2.1 for Claude Desktop integration |
| Telemetry | Opt-in anonymous product usage telemetry |

### Directory layout

```
cmd/clawvisor/main.go       — unified CLI (start, stop, status, setup, connect-agent, services, pair, dashboard, server, tui, agent, update, install, healthcheck)
cmd/cvis-e2e/               — E2E encryption test utility
cmd/server/                 — standalone server entry point
internal/
  adapters/                 — service adapters (google/, github/, apple/, slack/, notion/, linear/, stripe/, twilio/)
  api/                      — HTTP server, middleware, handlers
  auth/                     — JWT, passwords, magic link tokens
  browser/                  — opens URLs in the user's browser
  callback/                 — async result delivery to agents
  display/                  — human-readable names for services and actions
  events/                   — SSE event hub for real-time dashboard updates
  daemon/                   — daemon management (install, keygen, pairing, relay, setup, status)
  intent/                   — intent verification and chain context extraction
  llm/                      — LLM HTTP client for intent verification and task risk
  taskrisk/                 — LLM-powered task risk assessment
  mcp/                      — MCP server with OAuth 2.1 provider
  notify/                   — notifications (telegram/ and push/ sub-packages)
  ratelimit/                — rate limiting
  relay/                    — WebSocket reverse tunnel client for NAT traversal
  server/                   — server bootstrap and config loading
  setup/                    — interactive setup wizard
  telemetry/                — opt-in anonymous usage telemetry
  store/                    — Store interface + postgres/ and sqlite/ implementations
  tui/                      — terminal UI dashboard
  vault/                    — Vault interface + local and GCP implementations
pkg/
  adapters/                 — Adapter interface
  auth/                     — TokenService interface and JWT claims
  clawvisor/                — high-level app bootstrap (wires config → store → vault → server)
  config/                   — configuration loading and env var resolution
  gateway/                  — gateway request/response types
  notify/                   — Notifier interface
  store/                    — Store interface (75 methods)
  vault/                    — Vault interface
  version/                  — version information
web/                        — React frontend (Vite)
docs/                       — setup and integration guides
skills/clawvisor/           — agent skill definition (SKILL.md)
extensions/                 — OpenClaw webhook plugin
deploy/                     — Dockerfile, docker-compose, Cloud Run config
```

### Dev commands

```bash
make build                          # full build (Go binary + frontend)
make run                            # local dev server (SQLite, auto-opens browser)
make tui                            # terminal dashboard
make setup                          # re-run interactive setup wizard
make test                           # run all tests
make lint                           # go vet
make eval-intent                    # run LLM intent verification eval suite

make up                             # docker compose (app + postgres)
make web-dev                        # frontend dev server on :5173
make deploy                         # deploy via Cloud Build
make release                        # build release binaries
make clean                          # remove build artifacts
```

## Security Model

- **Agents never receive credentials.** Credentials are injected inside the Clawvisor process after authorization. They are stripped from all responses, logs, and error messages.
- **Vault encryption.** Credentials are encrypted at rest with AES-256-GCM using a master key (via `VAULT_KEY` env var or `vault.key` file).
- **Audit trail.** Every gateway request is logged with a unique `request_id` (enforced by DB constraint). Outcomes are updated after execution.
- **Response formatting.** Adapter results are semantically formatted — secrets are stripped, HTML/Unicode is sanitized before anything reaches the agent.
- **Device authentication.** Paired mobile devices authenticate via HMAC-SHA256 (signing method, path, timestamp, and body hash). Timestamps are checked within a 5-minute skew tolerance.
- **E2E encryption.** Device-authenticated endpoints support end-to-end encryption using X25519 ECDH key exchange with HKDF key derivation and AES encryption.
- **Relay authentication.** The daemon authenticates to the relay service via Ed25519 challenge-response with a 60-second replay window.
- **Rate limiting.** Per-agent gateway rate limits and per-user limits on OAuth, policy API, and review endpoints.
- **Agent isolation.** Clawvisor's security model assumes the agent can only reach it through the API. When self-hosting, run the agent in a separate environment (sandboxed container, separate machine, or cloud VM) — see the [self-host warning](#self-host).
