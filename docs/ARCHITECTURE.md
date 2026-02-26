# Clawvisor: Technical Architecture

This document describes how Clawvisor works from first principles. It covers the design motivation, every subsystem, the data model, and key implementation decisions. It is written for someone who wants to understand the full system before modifying it.

---

## 1. What Clawvisor Is

Clawvisor is a gatekeeper process that sits between AI agents and external APIs. The core invariant is simple: **the agent never holds credentials**. Every action an agent wants to take — sending an email, creating a calendar event, posting to Slack — flows through Clawvisor, which decides whether to allow it, asks a human if needed, injects the real credentials from an encrypted vault, executes the call, and returns a sanitized result.

The system exists to solve a trust problem. AI agents are useful when they can take real actions, but giving an agent raw API keys means trusting it completely. Clawvisor lets you grant an agent *capability* without granting it *credentials*. You can revoke access, scope it to specific actions, require human approval for sensitive operations, and audit everything after the fact.

---

## 2. Request Lifecycle

Every agent interaction follows the same path through the system. Here is the complete flow, from the agent's HTTP request to the final response.

### 2.1 Entry Point

The agent sends a POST to `/api/gateway/request` with a bearer token. The request includes:

```json
{
  "service": "google.gmail",
  "action": "send_message",
  "params": {"to": "alice@example.com", "subject": "Hello", "body": "..."},
  "reason": "User asked me to email Alice about the meeting",
  "request_id": "req-abc-123",
  "task_id": "task-uuid",
  "context": {
    "source": "user_message",
    "data_origin": "gmail:msg-xyz",
    "callback_url": "https://agent.example.com/callback"
  }
}
```

The `reason` field is always required — it's shown to the human in approval requests and logged in the audit trail. The `request_id` is used for idempotency (enforced by a UNIQUE constraint in the database). The `context.data_origin` tracks what external content influenced this request, which is critical for detecting prompt injection.

### 2.2 Authentication

The `RequireAgent` middleware extracts the bearer token, computes its SHA-256 hash, and looks up the agent record by that hash. The raw token is never stored — only the hash exists in the database. If the lookup fails, the request gets a 401 immediately.

The agent record includes the `user_id` of the human who created it. All authorization decisions are scoped to that user's configuration.

### 2.3 Request ID Deduplication

If the agent provides a `request_id` that already exists in the audit log, Clawvisor returns the cached outcome instead of re-processing. This makes requests idempotent — an agent can safely retry or poll by re-sending the same request. The `UNIQUE` constraint on `audit_log.request_id` enforces this at the database level.

If no `request_id` is provided, Clawvisor generates a UUID.

### 2.4 Service Alias Resolution

The `service` field can include an account alias: `google.gmail:personal` or `github:work`. This is split into a service type (`google.gmail`) and alias (`personal`). The alias determines which vault credential to use, enabling multiple accounts for the same service. If no alias is specified, it defaults to `"default"`.

### 2.5 Authorization Cascade

Authorization is evaluated in strict order. Each layer either terminates the request or passes it to the next.

**Layer 1: Restrictions**

The system checks the `restrictions` table for a matching `(user_id, service, action)` entry. Restrictions support wildcards: `service = "*"` blocks all services, `action = "*"` blocks all actions for a service. If a restriction matches, the request is blocked immediately and an audit entry is logged with `decision: "block"`. The agent receives `status: "blocked"` with the restriction's reason.

Restrictions are absolute — nothing overrides them.

**Layer 2: Hardcoded Approval Requirements**

Certain actions always require per-request human approval regardless of task scope. Currently, only `apple.imessage:send_message` has this treatment. If the action is hardcoded, it skips task auto-execution and falls through to the approval queue.

**Layer 3: Task Scope**

If the request includes a `task_id`, Clawvisor validates the task:
- The task must exist and belong to the agent's user
- The task's status must be `"active"`
- The task must not be expired (session tasks have a TTL)

Then it checks whether the requested `(service, action)` is in the task's `authorized_actions` list. The matching supports aliases — `google.gmail:personal` matches an authorized action for `google.gmail` regardless of alias.

If the action is **in scope with `auto_execute: true`** and not hardcoded: the request proceeds to execution (see 2.6).

If the action is **in scope with `auto_execute: false`**: it falls through to per-request approval. The task pre-authorized the *type* of action but the human still reviews each instance.

If the action is **out of scope**: the agent receives `status: "pending_scope_expansion"` and must call the scope expansion endpoint to request the new action be added.

**Layer 4: Per-Request Approval (Default)**

If no task covers the request — or no `task_id` was provided — the request enters the approval queue. Before queuing, two checks happen:

1. **Adapter existence**: Is the service recognized? If not, the agent gets an error.
2. **Service activation**: Does the user have a credential in the vault for this service? If not, the agent gets `status: "pending_activation"` with an `activate_url` they can show the user.

If both pass, the request is serialized into a `pending_approvals` record with a 5-minute TTL (configurable). The human is notified via Telegram. The agent receives `status: "pending"`.

### 2.6 Execution Path

When a request is authorized (either via task auto-execute or human approval), execution proceeds:

1. **Intent Verification** (optional): If configured, an LLM checks whether the request parameters are consistent with the task's stated purpose and the agent's reason. This catches cases where an agent claims to be "checking unread emails" but the params request a year's worth of data. The verifier can be configured to fail-open (allow on error) or fail-closed (block on error). Results are cached in memory with a configurable TTL.

2. **Vault Credential Injection**: Clawvisor fetches the encrypted credential from the vault using `(user_id, vault_key)`. For Google services, the vault key is always `"google"` (shared across Gmail, Calendar, Drive, Contacts). For aliased services, the key is `"base:alias"` (e.g., `"google:personal"`). The credential is decrypted with AES-256-GCM using the master key.

3. **Adapter Execution**: The decrypted credential is passed to the adapter's `Execute()` method along with the action and params. The adapter makes the real API call (Gmail API, GitHub API, Slack API, etc.) and returns a semantically formatted `Result` with a human-readable `summary` and structured `data`.

4. **Audit Logging**: The audit entry (created before execution) is updated with the outcome, execution duration, and verification verdict.

5. **Response**: The agent receives `status: "executed"` with the result.

6. **Callback Delivery**: If the agent provided a `callback_url`, the result is POSTed there asynchronously. The callback includes an HMAC-SHA256 signature in the `X-Clawvisor-Signature` header so the agent can verify authenticity.

### 2.7 Async Resolution

When a request is pending approval, it can be resolved three ways:

1. **Human approves** (via dashboard or Telegram): The serialized request blob is re-executed through the adapter, and the result is delivered to the agent's callback URL.
2. **Human denies**: The agent's callback receives `status: "denied"`.
3. **Timeout** (default 5 minutes): A background goroutine runs every 60 seconds, finds expired pending approvals, marks them as timed out, and delivers `status: "timeout"` callbacks.

The Telegram message is updated in-place to reflect the outcome (green checkmark for approved, red X for denied, clock for timeout).

---

## 3. The Task System

Tasks solve the "N approvals for N requests" problem. When an agent needs to make many related requests (triage 50 emails, scan calendar for the week), requiring human approval for each one is impractical. Tasks let the agent declare a scope of actions up front, get one approval, and then execute freely within that scope.

### 3.1 Task Lifecycle

```
                ┌─── deny ──→ denied
                │
pending_approval ──── approve ──→ active ──→ completed (by agent)
                                    │         expired (by TTL)
                                    │         revoked (by user)
                                    │
                                    └─ expand ─→ pending_scope_expansion
                                                    │
                                                    ├─ approve ──→ active (expanded)
                                                    └─ deny ────→ active (unchanged)
```

### 3.2 Creation

The agent calls `POST /api/tasks` with:

```json
{
  "purpose": "Triage inbox and draft replies",
  "authorized_actions": [
    {"service": "google.gmail", "action": "list_messages", "auto_execute": true},
    {"service": "google.gmail", "action": "get_message", "auto_execute": true},
    {"service": "google.gmail", "action": "send_message", "auto_execute": false}
  ],
  "expires_in_seconds": 1800,
  "lifetime": "session"
}
```

Every task starts as `pending_approval`. The human is notified via Telegram with the purpose and action list. The `auto_execute` flag per action controls whether matching requests run immediately or still require per-request approval. In the example above, reading emails is automatic but sending requires a human check each time.

### 3.3 Lifetimes

**Session tasks** expire after a configurable TTL (default 30 minutes). They're meant for a single agent conversation — triage this inbox, plan this week's calendar.

**Standing tasks** never expire. They persist until the user revokes them from the dashboard. They're meant for recurring workflows — "this agent can always read my email." Standing tasks store a sentinel expiry date of 9999-01-01 internally, which is stripped from API responses.

### 3.4 Scope Expansion

If an agent needs an action not in the original task scope, it calls `POST /api/tasks/{id}/expand` with the new action and a reason. The task enters `pending_scope_expansion` state and the human is notified. On approval, the action is added to the authorized list and the expiry is reset. On denial, the task returns to its previous state unchanged.

Standing tasks cannot be expanded — the user must revoke and create a new one. This is a deliberate constraint to prevent scope creep on indefinite authorizations.

### 3.5 Task Scope Matching

When a gateway request includes a `task_id`, the scope check matches `(service, action)` against the task's `authorized_actions`:

- Exact match: `google.gmail` + `list_messages` matches `{"service": "google.gmail", "action": "list_messages"}`
- Alias transparency: `google.gmail:personal` matches an authorized action for `google.gmail` (aliases don't affect scope)
- Wildcard actions: `{"action": "*"}` matches any action for that service

---

## 4. Supported Services

Clawvisor has 11 service adapters. Each implements the `Adapter` interface: `ServiceID()`, `SupportedActions()`, `Execute()`, and credential management methods.

### Google Services

All four Google adapters share a single OAuth credential stored under vault key `"google"`. Activating one activates all. They request a unified scope set covering Gmail, Calendar, Drive, and Contacts.

| Service ID | Actions |
|---|---|
| `google.gmail` | `list_messages`, `get_message`, `send_message` |
| `google.calendar` | `list_events`, `get_event`, `create_event`, `update_event`, `delete_event`, `list_calendars` |
| `google.drive` | `list_files`, `get_file`, `create_file`, `update_file`, `search_files` |
| `google.contacts` | `list_contacts`, `get_contact`, `search_contacts` |

The contacts adapter also implements `ContactsChecker`, which the gateway uses to pre-resolve the `recipient_in_contacts` condition before policy evaluation. This keeps the policy evaluator pure (no I/O).

### API Key Services

These are activated per-user by pasting a token in the dashboard. Each stores credentials as `{"type": "api_key", "token": "..."}` in the vault.

| Service ID | Credential | Actions |
|---|---|---|
| `github` | Personal access token (`ghp_...`) | `list_issues`, `get_issue`, `create_issue`, `comment_issue`, `list_prs`, `get_pr`, `list_repos`, `search_code` |
| `slack` | Bot token (`xoxb-...`) | `list_channels`, `get_channel`, `list_messages`, `send_message`, `search_messages`, `list_users` |
| `notion` | Integration token (`secret_...`) | `search`, `get_page`, `create_page`, `update_page`, `query_database`, `list_databases` |
| `linear` | API key (`lin_api_...`) | `list_issues`, `get_issue`, `create_issue`, `update_issue`, `add_comment`, `list_teams`, `list_projects`, `search_issues` |
| `stripe` | Secret key (`sk_...` or `rk_...`) | `list_customers`, `get_customer`, `list_charges`, `get_charge`, `list_subscriptions`, `get_subscription`, `create_refund`, `get_balance` |
| `twilio` | Account SID + Auth Token (`AC...:token`) | `send_sms`, `send_whatsapp`, `list_messages`, `get_message` |

### Local Services

| Service ID | Platform | Actions | Notes |
|---|---|---|---|
| `apple.imessage` | macOS only | `search_messages`, `list_threads`, `get_thread`, `send_message` | No credential needed. Reads `~/Library/Messages/chat.db` directly. `send_message` always requires per-request approval regardless of task scope. Sending uses AppleScript via Messages.app. |

---

## 5. Credential Management (Vault)

### 5.1 Design

Credentials are stored encrypted at rest and never leave the Clawvisor process. The vault is keyed by `(user_id, service_id)` — each user has independent credentials for each service. The agent never sees credential bytes in any response, log, or error message.

### 5.2 Local Vault (AES-256-GCM)

The default backend stores encrypted credentials in a `vault_entries` database table. Encryption uses AES-256-GCM with a 32-byte master key loaded from a keyfile (`vault.key`).

**Encryption**: For each credential, a random nonce is generated via `crypto/rand`. The plaintext is sealed with `gcm.Seal()`, producing ciphertext and an authentication tag. All three components (ciphertext, nonce, auth tag) are base64-encoded and stored in separate columns.

**Key generation**: On first run, if `vault.key` doesn't exist, Clawvisor generates 32 random bytes, base64-encodes them, writes the file, and sets permissions to 0600. This key is the root of trust — losing it means losing access to all stored credentials.

**Database schema**:
```sql
vault_entries (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL,
  service_id TEXT NOT NULL,       -- "google", "github", "slack", etc.
  encrypted TEXT NOT NULL,         -- base64(ciphertext)
  iv TEXT NOT NULL,                -- base64(nonce)
  auth_tag TEXT NOT NULL,          -- base64(authentication tag)
  UNIQUE(user_id, service_id)
)
```

### 5.3 GCP Vault

The GCP backend stores credentials in Google Cloud Secret Manager. Credentials are still AES-256-GCM encrypted with the local master key before storage — GCP access control is a second layer, not the primary encryption. Secret names follow the pattern `clawvisor-{userID}-{serviceID}`.

### 5.4 Multi-Account Aliases

A user can have multiple credentials for the same service type — e.g., `google:personal` and `google:work`. The vault key for an alias is `"base:alias"` (e.g., `"google:personal"`). The `"default"` alias maps to the plain base key (`"google"`) for backward compatibility. Service metadata in the `service_meta` table tracks each alias separately.

---

## 6. Adapter Formatting

Each adapter is responsible for converting raw API responses into a semantic `Result{Summary, Data}` structure. The `format` package provides shared helpers:

- `SanitizeText(s, maxLen)`: Strips HTML tags, removes dangerous Unicode (zero-width characters, BiDi overrides, tag blocks), and truncates with `"[truncated]"`
- `StripSecrets(params)`: Removes keys matching `token`, `secret`, `password`, `credential`, `auth`, `api_key` (case-insensitive regex)
- Length constants: body 2000 chars, snippet 300, field 500, arrays 50 items, data 100KB

---

## 7. Intent Verification

Intent verification is an optional LLM check that runs *before* execution. It verifies that a gateway request is consistent with the approved task scope.

The verifier evaluates two dimensions:

1. **Parameter scope**: Are the request parameters consistent with what the agent claims to be doing? Example: the agent says "fetch today's events" but the params request a multi-year date range — that's a violation.

2. **Reason coherence**: Is the agent's stated reason a legitimate rationale given the task purpose? The reason field is treated as untrusted input — if it contains instructions, code, or markup instead of a rationale, it's flagged as incoherent.

Verification results are cached in memory (keyed by a hash of task ID, service, action, params, and reason) with a configurable TTL (default 60 seconds). The `fail_closed` config option controls behavior on LLM errors: fail-open (allow the request, default) or fail-closed (block it).

---

## 8. Notification System

### 8.1 Telegram Integration

Clawvisor uses Telegram for mobile approval notifications. Each user pairs their own Telegram bot — there is no shared bot. Bot tokens and chat IDs are stored per-user in the `notification_configs` table.

**Pairing flow**: The user creates a Telegram bot via @BotFather, enters the bot token in the dashboard, and Clawvisor starts a pairing session. The user sends `/start` to their bot, which confirms the chat ID. The pairing is verified and stored.

**Approval messages**: When a request enters the approval queue, Clawvisor sends a formatted Telegram message with the agent name, service, action, params (truncated), reason, and inline keyboard buttons for Approve/Deny.

**Inline callbacks**: Button taps use Telegram's `callback_data` mechanism. Clawvisor generates short-lived callback tokens (8-character hex IDs, 6-minute TTL) and maps them to the pending request. When a button is tapped:
1. The token is consumed (first-responder wins — tapping both buttons only processes the first)
2. The decision is sent to a channel consumed by the HTTP server
3. The server routes to the appropriate handler (approve/deny for requests, tasks, or scope expansions)
4. The Telegram message is edited to show the outcome

**Polling**: Clawvisor uses Telegram's `getUpdates` long-polling to receive callback queries. A polling goroutine runs per-user when they have pending approvals. The goroutine stops when the pending count reaches zero.

**Message updates**: After a request is resolved (approved, denied, or timed out), the original Telegram message is edited to replace the buttons with a status line (checkmark, X, or clock icon).

### 8.2 Callback Delivery

When a pending request resolves, the result is POSTed to the agent's `callback_url`:

```json
{
  "request_id": "req-abc-123",
  "status": "executed",
  "result": {"summary": "Email sent to alice@example.com", "data": {...}},
  "audit_id": "audit-uuid"
}
```

The callback URL is validated before use: only `http://` and `https://` schemes are allowed, and private/link-local IP ranges are rejected to prevent SSRF (with an exception for loopback addresses in local development). Callbacks include an `X-Clawvisor-Signature` HMAC-SHA256 header for verification. Delivery is best-effort with no retries.

---

## 9. Authentication

### 9.1 User Authentication (JWT)

Human users authenticate via email/password. Passwords are hashed with bcrypt (cost 12, minimum 8 characters). On login, the server issues a token pair:

- **Access token**: JWT signed with HS256, 15-minute TTL. Contains `user_id` and `email` in claims. Validated on every request via the `RequireUser` middleware.
- **Refresh token**: 32 random bytes (hex-encoded), stored as a SHA-256 hash in the `sessions` table with a 30-day TTL. The raw token is returned once and never stored. On refresh, the old session is deleted (token rotation) and a new pair is issued.

The frontend stores the access token in React state (memory only) and the refresh token in `localStorage`. On 401, the API client automatically attempts a silent refresh.

### 9.2 Magic Link (Local Mode)

When the server is bound to a local address (localhost, 127.0.0.1, 0.0.0.0), it auto-creates an `admin@local` user with a random password and generates a one-time magic link token. The link is printed to the terminal:

```
  Clawvisor dashboard
  http://localhost:8080/auth/local?token=abc123...

  Open this link in your browser to sign in.
  Valid for 15 minutes. Single use.
```

Clicking the link validates the token, issues a JWT pair, and redirects to the dashboard. This eliminates the need for registration in local development.

### 9.3 Agent Authentication

Agents authenticate with bearer tokens. When an agent is created in the dashboard, a random token is generated and shown once. The SHA-256 hash is stored in the `agents` table. On each request, the `RequireAgent` middleware hashes the provided token and looks up the agent record.

Agent tokens are scoped to a user — the agent inherits the user's restrictions, service activations, and vault credentials.

---

## 10. Database Schema

The database is behind a `Store` interface with Postgres and SQLite implementations. Both use raw SQL with identical logic, differing only in type syntax (Postgres uses `TIMESTAMPTZ` and `JSONB`; SQLite uses `TEXT` for everything) and placeholder syntax (`$1` vs `?`).

Migrations are embedded in the binary and run automatically on startup. Each migration is tracked in a `schema_migrations` table.

### 10.1 Tables

**Core entities:**
- `users` — email (unique), password hash, timestamps
- `sessions` — refresh token hashes with expiry (FK → users, CASCADE)
- `agents` — name, token hash (unique), FK → users (CASCADE)

**Authorization:**
- `restrictions` — hard blocks on `(user_id, service, action)` with wildcard support. Unique constraint on the triple.
- `tasks` — purpose, status, lifetime, authorized_actions (JSON), pending_action (JSON), expiry, request count. FK → users, agents.

**Service management:**
- `service_meta` — tracks which services are activated per user, with alias support for multi-account. Unique on `(user_id, service_id, alias)`.
- `vault_entries` — encrypted credentials. Unique on `(user_id, service_id)`.

**Approval queue:**
- `pending_approvals` — serialized request blobs awaiting human decision. Includes callback URL, expiry, and audit ID. Unique on `request_id`.

**Audit:**
- `audit_log` — every gateway request. Includes service, action, sanitized params, decision, outcome, verification verdict, duration, and error messages. Legacy columns (`safety_flagged`, `safety_reason`, `filters_applied`) are retained for backward compatibility but no longer populated. `request_id` has a UNIQUE constraint. Indexed on `(user_id, timestamp)`, `(user_id, outcome)`, `(user_id, service)`.

**Notifications:**
- `notification_configs` — per-user channel configs (e.g., Telegram bot token + chat ID). Unique on `(user_id, channel)`.
- `notification_messages` — maps `(target_type, target_id, channel)` to a message ID for in-place updates.

### 10.2 Migration History

| # | Purpose |
|---|---|
| 001 | Foundation: users, sessions, agents, service_meta, notification_configs, vault_entries |
| 002 | Policies table (superseded in 005) |
| 003 | Audit log and pending approvals |
| 004 | Tasks table, task_id column on audit_log |
| 005 | Restrictions table (replaces policies), task lifetime column |
| 006 | Remove agent roles (simplification) |
| 007 | Multi-account alias support on service_meta |
| 008 | Notification messages table (replaces inline telegram_msg_id) |
| 009 | Intent verification column on audit_log |

---

## 11. Frontend

The frontend is a React 18 SPA built with Vite, TypeScript, Tailwind CSS, and shadcn/ui components. It is compiled to `web/dist/` and served by the Go backend as static files.

### 11.1 Pages

- **Overview**: Pending approval count, quick actions
- **Tasks**: Active/standing/pending tasks with approval controls, scope expansion management
- **Services**: Activation status for all adapters, OAuth popup flow for Google, token input for API key services, deactivation
- **Restrictions**: Create and manage hard blocks on service/action pairs
- **Agents**: Create agent tokens (shown once), delete agents
- **Audit Log**: Searchable, filterable history of every gateway request
- **Settings**: Telegram bot pairing, account management

### 11.2 Auth Flow

The frontend detects auth mode (`magic_link` or `password`) from `/api/config/public`. In magic link mode, the login page is skipped entirely — the user clicks the terminal link. In password mode, login/register pages are shown.

Access tokens live in React state (cleared on page refresh). Refresh tokens live in `localStorage`. The API client intercepts 401 responses and attempts a silent refresh, deduplicating concurrent refresh attempts to avoid race conditions.

### 11.3 Approval Polling

The dashboard polls `GET /api/approvals` and `GET /api/tasks` every 15 seconds. Pending counts are shown as badges in the sidebar. A floating approvals panel appears when there are pending items.

---

## 12. Configuration

Configuration is loaded in three layers, each overriding the previous:

1. **Defaults** (hardcoded in `config.Default()`)
2. **YAML file** (`config.yaml` or `CONFIG_FILE` env var)
3. **Environment variables** (highest priority)

### 12.1 Key Settings

| Setting | Default | Env Override | Notes |
|---|---|---|---|
| Server port | 8080 | `PORT` | |
| Server host | 127.0.0.1 | `SERVER_HOST` | Set to `0.0.0.0` for Cloud Run |
| Public URL | (empty) | `PUBLIC_URL` | Used in Telegram notification links |
| Database driver | postgres | `DATABASE_DRIVER` | `postgres` or `sqlite` |
| Postgres URL | (empty) | `DATABASE_URL` | Required for postgres |
| SQLite path | ./clawvisor.db | | |
| JWT secret | (empty) | `JWT_SECRET` | **Required** — server refuses to start without it |
| Access token TTL | 15m | | |
| Refresh token TTL | 720h (30 days) | | |
| Vault backend | local | `VAULT_BACKEND` | `local` or `gcp` |
| Vault key file | ./vault.key | `VAULT_KEY_FILE` | |
| Approval timeout | 300s | | Pending approvals expire after this |
| Task default expiry | 1800s (30 min) | | Session task TTL |
| Google OAuth | (empty) | `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` | Required for Google services |
| LLM verification | disabled | `CLAWVISOR_LLM_VERIFICATION_*` | Optional pre-execution intent check |
| Service toggles | all enabled | `GITHUB_ENABLED`, `SLACK_ENABLED`, etc. | Boolean: `"true"` or `"1"` |

### 12.2 LLM Configuration

There is one LLM slot for intent verification, configurable with provider and model:

- **Verification**: Pre-execution intent consistency checking. Recommended: fast/cheap model.

Settings: `enabled`, `provider` (anthropic/openai), `endpoint`, `api_key`, `model`, `timeout_seconds`, `fail_closed`, `cache_ttl_seconds`.

---

## 13. Startup Sequence

When the server starts (`cmd/server/main.go`):

1. Load config (YAML + env overrides)
2. Validate `JWT_SECRET` is set
3. Connect to database (Postgres or SQLite)
4. Run migrations (embedded SQL files, tracked in `schema_migrations`)
5. Initialize vault (load or generate `vault.key`, connect to DB or GCP)
6. Initialize JWT service
7. Register adapters (Google if OAuth configured, GitHub/Slack/Notion/Linear/Stripe/Twilio if enabled, iMessage if macOS)
8. Create Telegram notifier (always; reads per-user config lazily)
9. If local mode: create `admin@local` user, generate magic link, print to terminal
10. Create HTTP server, register routes, wire middleware
11. Start background goroutines:
    - Approval expiry cleanup (every 60 seconds)
    - Telegram decision consumer (routes inline button taps to handlers)
    - Telegram token cleanup (removes expired callback tokens)
    - Magic link cleanup (every 5 minutes, if local mode)
13. Listen on configured address; graceful shutdown on SIGINT/SIGTERM (30-second drain)

---

## 14. Deployment

### 14.1 Local Development

```bash
DATABASE_DRIVER=sqlite JWT_SECRET=dev-secret go run ./cmd/server
```

This starts with SQLite (no Docker needed), auto-generates `vault.key`, creates a local user, and prints a magic link. The frontend dev server (`npm run dev` in `web/`) proxies API requests to `:8080`.

### 14.2 Docker Compose

`deploy/docker-compose.yml` runs Postgres 16 and the app together. The app builds via a multi-stage Dockerfile that compiles the React frontend and the Go binary into a distroless image.

### 14.3 Google Cloud Run

`deploy/cloudrun.yaml` defines the Cloud Run service:
- `minScale: 1` — required so approval callbacks and Telegram polling have a live process
- `timeoutSeconds: 360` — buffer above the approval timeout (300s) and MCP timeout (240s)
- Secrets (`JWT_SECRET`, `DATABASE_URL`) loaded from GCP Secret Manager
- Cloud SQL connection via sidecar proxy
- `SERVER_HOST: 0.0.0.0` to bind to all interfaces

`deploy/cloudbuild.yaml` builds the Docker image, pushes to GCR, and deploys to Cloud Run in a single pipeline triggered by `make deploy`.

---

## 15. Trust Boundaries

```
Trusted                          Untrusted
──────────────────────           ────────────────────────────────
Clawvisor process                Agent (potentially manipulated)
Restriction rules                External API responses
Vault (encrypted)                Data agent processes (email, web, docs)
Config + environment             Agent's "reason" field
Human approval decision          Agent's "context.data_origin" (logged, not trusted)
```

---

## 16. Design Decisions and Trade-offs

**Why no ORM?** Raw SQL with a thin repository layer keeps queries visible and debuggable. The `Store` interface provides the abstraction boundary. The trade-off is maintaining two nearly identical implementations (Postgres and SQLite) that can drift.

**Why per-user Telegram bots?** A shared bot would require Clawvisor to be a registered Telegram bot with webhook infrastructure. Per-user bots are simpler to set up and don't require the Clawvisor deployment to be publicly reachable (long-polling works behind NAT).

**Why default-approve instead of default-block?** The system is designed for personal use by a single user who *wants* their agent to be capable. Blocking by default would make the agent useless until policies are configured. The approval queue provides a human checkpoint without requiring pre-configuration.

**Why separate vault and database?** Credentials are encrypted before storage, so even a database breach doesn't expose them. The vault key is a separate file (not in config, not in the database) so that compromising one doesn't compromise the other.

**Why task scopes instead of traditional RBAC?** Tasks are temporal and agent-initiated. An agent says "I need to do X for the next 30 minutes" and the human approves that specific scope. This is more natural for AI agent workflows than static role assignments, and it provides a clear audit trail of what was authorized and when.

**Why HMAC-SHA256 for callback signing?** The agent needs to verify that callbacks actually came from Clawvisor. HMAC with a shared secret is simple and doesn't require PKI. The trade-off is that the signing key derivation is currently based on the agent's token hash rather than the raw token, which means the agent can't independently verify signatures without knowing this implementation detail.

**Why a single LLM checkpoint (intent verification)?** Earlier designs included post-execution safety checking and LLM-powered response filtering, but these were removed as dead code — the safety checker always returned "safe" via a noop implementation, and no consumer ever sent response filters. Intent verification is the only LLM checkpoint that provides concrete value: it catches misuse of task-scoped auto-execution by verifying that request parameters match the stated purpose.
