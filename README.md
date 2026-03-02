<p align="center">
  <img src="web/public/favicon.svg" alt="Clawvisor" width="96" height="96" />
</p>

<h1 align="center">Clawvisor</h1>

<p align="center">
  <strong>Your agents act. You stay in control.</strong><br/>
  The gatekeeper between your AI agents and the APIs they act on.
</p>

<p align="center">
  <a href="#quickstart">Quickstart</a> · <a href="#agent-integration">Agent Integration</a> · <a href="#dashboard">Dashboard</a> · <a href="#tui">TUI</a> · <a href="#supported-services">Services</a>
</p>

---

> [!WARNING]
> **Use at your own risk.** Clawvisor is experimental software under active development. It has not been audited for security. LLMs are inherently nondeterministic — we make no guarantees that policies or safety checks will behave as expected in every case. Do not use Clawvisor as your sole safeguard for sensitive data or critical systems.

---

AI agents are getting good at doing things. The problem is letting them — safely. Give an agent your Gmail credentials and it can read, send, and delete anything. Refuse, and it can't help you at all.

Clawvisor sits in the middle. Agents never hold credentials. Instead, they declare **tasks** describing what they need to do, the user approves the scope, and Clawvisor handles credential injection, execution, and audit logging for every request under that task.

The typical flow: an agent creates a task ("triage my inbox — read emails, but ask before sending"), the user approves it once, and the agent makes as many requests as it needs within that scope without further interruption. Actions outside the scope go to the user for per-request approval. Restrictions can hard-block specific actions entirely.

Approve a purpose, not a permission. Clawvisor enforces it on every request.

## Quickstart

**Prerequisites:**

- [Go 1.23+](https://go.dev/doc/install) — `brew install go` on macOS
- [Node.js 18+](https://nodejs.org/) — `brew install node` on macOS

```bash
git clone https://github.com/clawvisor/clawvisor.git
cd clawvisor

# Interactive setup — installs deps, builds, generates config
make setup

# Start the server
make run

# Launch the TUI dashboard (in a separate terminal)
make tui
```

That's it — three commands to a working setup.

`make setup` walks through environment, Google OAuth, iMessage, and intent verification configuration. It generates a `config.yaml` with sensible defaults (hit Enter through most prompts for a basic local setup) and writes `~/.clawvisor/config.yaml` with the server URL so the TUI knows where to connect.

`make run` starts the server. In local mode it auto-creates an `admin@local` user, opens the web dashboard via a magic link, and writes a session file (`~/.clawvisor/.local-session`) for the TUI.

`make tui` launches the terminal dashboard. It automatically authenticates using the local session file — no manual token configuration needed. On first launch it exchanges the session token for a refresh token and saves it for future use.

You can also run without the setup script:

```bash
DATABASE_DRIVER=sqlite JWT_SECRET=dev-secret go run ./cmd/server
```

A `vault.key` file is auto-generated on first run if one doesn't exist. This is the master encryption key for stored credentials.

### Configuration

Clawvisor loads `config.yaml` from the working directory (override with `CONFIG_FILE` env var). Most settings have env var overrides:

| Setting | Env var | Notes |
|---|---|---|
| Database driver | `DATABASE_DRIVER` | `postgres` or `sqlite` (auto-selects sqlite locally) |
| Postgres URL | `DATABASE_URL` | Required for postgres driver |
| JWT secret | `JWT_SECRET` | Auto-generated locally; **required** in prod |
| Google OAuth | `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` | Needed for Google services |
| Public URL | `PUBLIC_URL` | Used in Telegram notification links |
| Intent verification | `CLAWVISOR_LLM_VERIFICATION_*` | Optional LLM check that request params match task purpose |

See [`config.example.yaml`](config.example.yaml) for the full configuration reference.

## How It Works

```
Agent                         Clawvisor                              External API
  │                              │                                       │
  ├─ POST /api/tasks ────────────►  (declare scope, wait for approval)   │
  │◄──── task approved ──────────┤                                       │
  │                              │                                       │
  ├─ POST /api/gateway/request ──►                                       │
  │    (with task_id)            ├─ Check restrictions (hard blocks)      │
  │                              ├─ Check task scope (pre-approved?)      │
  │                              ├─ Auto-execute or route to approval     │
  │                              │                                       │
  │                              │  ┌─ On execute: ──────────────────┐   │
  │                              │  │ Vault inject credentials       │   │
  │                              │  │ Adapter call ──────────────────────►│
  │                              │  │ Format + filter response       │◄──┤
  │                              │  │ Intent verification (optional) │   │
  │                              │  │ Audit log                      │   │
  │                              │  └────────────────────────────────┘   │
  │◄──── Response ───────────────┤                                       │
```

Authorization has three layers:

1. **Restrictions** — hard blocks you set on specific service/action pairs. If a restriction matches, the request is blocked immediately.
2. **Task scopes** — the primary mechanism. Agents declare tasks with a purpose and a set of authorized actions. The user approves the scope once; subsequent requests under that task execute without per-request approval (for `auto_execute` actions). Tasks can be session-scoped (with expiry) or standing (indefinite, user-revocable).
3. **Per-request approval** — the fallback. Requests without a task, or with actions not in the task's scope, go to the approval queue and the user is notified via Telegram.

Requests that don't match any restriction or task scope go to the approval queue — the default is approve, not block.

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

The task starts as `pending_approval`. The user approves it via Telegram or the dashboard. Include a `callback_url` to receive a callback when the task is approved or denied — otherwise poll `GET /api/tasks/{id}`.

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
| `pending` | Awaiting human approval. Wait for callback or poll with same `request_id`. |
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

- **Approvals** — approve or deny pending requests and task scopes
- **Tasks** — view active/standing tasks, revoke task scopes, see audit trails
- **Services** — activate services via OAuth (Google) or API key (GitHub, Slack, Notion, etc.)
- **Restrictions** — create and manage hard blocks on service/action pairs
- **Audit log** — searchable history of every gateway request with outcomes
- **Agents** — create agent tokens, manage per-agent restrictions
- **Telegram** — pair your Telegram bot for mobile approval notifications

## TUI

The terminal dashboard (`clawvisor tui`) provides the same approval and monitoring capabilities without leaving the terminal.

```bash
# After setup + server are running:
make tui

# Or directly:
clawvisor tui

# With explicit connection details:
clawvisor tui --url http://localhost:8080 --token <refresh_token>
```

Authentication is automatic in local mode. The flow:

1. `clawvisor server` writes `~/.clawvisor/.local-session` with a one-time magic token
2. `clawvisor tui` reads this file, exchanges the token via `POST /api/auth/magic`, and saves the resulting refresh token to `~/.clawvisor/config.yaml`
3. Subsequent launches use the saved refresh token directly

For password-mode servers, the TUI prompts for email and password on first launch.

You can also set credentials via environment variables:

```bash
export CLAWVISOR_URL=http://localhost:8080
export CLAWVISOR_TOKEN=<refresh_token>
clawvisor tui
```

## Architecture

| Layer | Choice |
|---|---|
| Backend | Go 1.23+, `net/http` ServeMux |
| Frontend | Vite + React 18 + TypeScript + Tailwind + shadcn/ui |
| Database | Postgres (prod) or SQLite (local), behind `Store` interface |
| Vault | AES-256-GCM with keyfile (local) or GCP Secret Manager, behind `Vault` interface |
| Auth | JWT (HS256), bcrypt passwords |
| Notifications | Telegram (per-user bot tokens) |

### Directory layout

```
cmd/server/main.go          — entry point
internal/
  api/                      — HTTP server, middleware, handlers
  adapters/                 — service adapters (google/, github/, apple/, slack/, notion/, linear/, stripe/, twilio/)
  store/                    — Store interface + postgres/ and sqlite/ implementations
  vault/                    — Vault interface + local and GCP implementations
  config/                   — config loading with env var overrides
  auth/                     — JWT, passwords, magic link tokens
  safety/                   — LLM safety checker
  notify/                   — Telegram notifier
  callback/                 — async result delivery to agents
  gateway/                  — gateway request types
  filters/                  — response filter pipeline
  intent/                   — intent verification
web/                        — React frontend (Vite)
skills/clawvisor/           — agent skill definition (SKILL.md)
deploy/                     — Dockerfile, docker-compose, Cloud Run config
```

### Dev commands

```bash
go build ./...                      # compile check
go test ./...                       # run all tests
go vet ./...                        # lint

make run-sqlite                     # run with SQLite
make up                             # docker compose (app + postgres)
make web-dev                        # frontend dev server on :5173
make build                          # full build (Go binary + frontend)
```

## Security Model

- **Agents never receive credentials.** Credentials are injected inside the Clawvisor process after authorization. They are stripped from all responses, logs, and error messages.
- **Vault encryption.** Credentials are encrypted at rest with AES-256-GCM using a master keyfile (`vault.key`).
- **Audit trail.** Every gateway request is logged with a unique `request_id` (enforced by DB constraint). Outcomes are updated after execution.
- **Response formatting.** Adapter results are semantically formatted — secrets are stripped, HTML/Unicode is sanitized before anything reaches the agent.

### Agent isolation

Clawvisor's security model assumes the agent can only reach it through the API — it should never have direct access to Clawvisor's database, configuration, or runtime environment. If the agent runs on the same machine with unrestricted shell access, it could bypass Clawvisor entirely by reading the database or inspecting process environment variables. The recommended deployment is to run the agent in an environment without direct access to Clawvisor's host — a sandboxed container (e.g. Docker), a separate machine, or in the cloud. This ensures the agent can only interact with Clawvisor through the gateway API, where restrictions, approval flows, and audit logging are enforced.
