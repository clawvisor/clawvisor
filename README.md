# Clawvisor

Clawvisor is a gatekeeper service that sits between AI agents and external APIs. Agents never hold credentials. Every request flows through Clawvisor, which checks restrictions, enforces task scopes, routes to human approval when needed, injects credentials from an encrypted vault, and delivers formatted results back to the agent.

## How It Works

```
Agent                         Clawvisor                              External API
  │                              │                                       │
  ├─ POST /api/gateway/request ──►                                       │
  │                              ├─ Check restrictions (hard blocks)      │
  │                              ├─ Check task scope (pre-approved?)      │
  │                              ├─ Route to approval queue OR execute    │
  │                              │                                       │
  │                              │  ┌─ On execute: ──────────────────┐   │
  │                              │  │ Vault inject credentials       │   │
  │                              │  │ Adapter call ──────────────────────►│
  │                              │  │ Format + filter response       │◄──┤
  │                              │  │ Safety check (optional)        │   │
  │                              │  │ Audit log                      │   │
  │                              │  └────────────────────────────────┘   │
  │◄──── Response ───────────────┤                                       │
```

Authorization has three layers:

- **Restrictions** — hard blocks you set on specific service/action pairs. If a restriction matches, the request is blocked immediately.
- **Task scopes** — pre-approved sets of actions the agent declares up front. If the action is in scope with `auto_execute`, it runs without per-request approval. Tasks can be session-scoped (with expiry) or standing (indefinite).
- **Per-request approval** — the default. Anything not covered by a task scope goes to the approval queue and the user is notified via Telegram.

Unmatched requests go to the approval queue, not to a block. The default is approve.

## Supported Services

| Service ID | Service | Actions |
|---|---|---|
| `google.gmail` | Gmail | `list_messages`, `get_message`, `send_message` |
| `google.calendar` | Google Calendar | `list_events`, `get_event`, `create_event`, `update_event`, `delete_event`, `list_calendars` |
| `google.drive` | Google Drive | `list_files`, `get_file`, `create_file`, `update_file`, `search_files` |
| `google.contacts` | Google Contacts | `list_contacts`, `get_contact`, `search_contacts` |
| `github` | GitHub | `list_issues`, `get_issue`, `create_issue`, `comment_issue`, `list_prs`, `get_pr`, `list_repos`, `search_code` |
| `apple.imessage` | iMessage | `search_messages`, `list_threads`, `get_thread`, `send_message` |

Google services share a single OAuth connection — activating one activates all four. GitHub uses a personal access token. iMessage reads the local `chat.db` on macOS and is always available without activation on supported machines.

## Quickstart

**Prerequisites:** Go 1.23+, Node 18+

```bash
# Clone and build
git clone https://github.com/ericlevine/clawvisor-gatekeeper.git
cd clawvisor-gatekeeper
cd web && npm install && npm run build && cd ..

# Run locally with SQLite (no Docker, no Postgres)
DATABASE_DRIVER=sqlite JWT_SECRET=dev-secret go run ./cmd/server
```

On startup, Clawvisor prints a magic link to the terminal:

```
  Clawvisor dashboard
  http://localhost:8080/auth/local?token=abc123...

  Open this link in your browser to sign in.
  Valid for 15 minutes. Single use.
```

This auto-creates an `admin@local` user and logs you in — no registration needed in local mode.

A `vault.key` file is auto-generated on first run if one doesn't exist. This is the master encryption key for stored credentials.

### Configuration

Clawvisor loads `config.yaml` from the working directory (override with `CONFIG_FILE` env var). Most settings have env var overrides:

| Setting | Env var | Notes |
|---|---|---|
| Database driver | `DATABASE_DRIVER` | `postgres` or `sqlite` |
| Postgres URL | `DATABASE_URL` | Required for postgres driver |
| JWT secret | `JWT_SECRET` | **Required** |
| Google OAuth | `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` | Needed for Google services |
| Public URL | `PUBLIC_URL` | Used in Telegram notification links |
| LLM safety | `CLAWVISOR_LLM_SAFETY_*` | Optional post-execution safety check |

See [`config.example.yaml`](config.example.yaml) for the full configuration reference.

## Agent Integration

### 1. Create an agent token

In the dashboard, create an agent and copy the bearer token. The agent uses this for all API calls.

### 2. Create a task (optional, for multi-step workflows)

```bash
curl -s -X POST "$CLAWVISOR_URL/api/tasks" \
  -H "Authorization: Bearer $AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "purpose": "Triage inbox and draft replies",
    "authorized_actions": [
      {"service": "google.gmail", "action": "list_messages", "auto_execute": true},
      {"service": "google.gmail", "action": "get_message", "auto_execute": true},
      {"service": "google.gmail", "action": "send_message", "auto_execute": false}
    ],
    "expires_in_seconds": 1800
  }'
```

The task starts as `pending_approval`. The user approves it via Telegram or the dashboard. Once active, requests with the `task_id` that match an `auto_execute` action run immediately.

### 3. Make a gateway request

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

### Response statuses

| Status | Meaning |
|---|---|
| `executed` | Action completed. Result in `result.summary` and `result.data`. |
| `pending` | Awaiting human approval. Wait for callback or poll with same `request_id`. |
| `blocked` | A restriction blocks this action. Do not retry. |
| `pending_activation` | Service not connected. Response includes `activate_url`. |
| `pending_task_approval` | Task declared but not yet approved by the user. |
| `error` | Execution failed. Check `error` field for details. |

### Callbacks

When a pending request resolves, Clawvisor POSTs to the `callback_url`:

```json
{
  "request_id": "req-001",
  "status": "executed",
  "result": {"summary": "Found 3 unread messages", "data": [...]},
  "audit_id": "a8f3..."
}
```

For the full agent protocol, see [`skills/clawvisor/SKILL.md`](skills/clawvisor/SKILL.md).

## Dashboard

The web UI provides:

- **Approvals** — approve or deny pending requests and task scopes
- **Tasks** — view active/standing tasks, revoke task scopes, see audit trails
- **Services** — activate services via OAuth (Google) or API key (GitHub)
- **Restrictions** — create and manage hard blocks on service/action pairs
- **Audit log** — searchable history of every gateway request with outcomes
- **Agents** — create agent tokens, manage per-agent restrictions
- **Telegram** — pair your Telegram bot for mobile approval notifications

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
  adapters/                 — service adapters (google/, github/, apple/)
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
- **Optional LLM safety check.** A configurable post-execution check scans formatted output for prompt injection or unsafe content before delivery.
