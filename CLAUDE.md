# Clawvisor — Implementation Guide

Clawvisor is a gatekeeper service that sits between an AI agent and sensitive external APIs. The agent never holds credentials. Every action flows through Clawvisor, which evaluates it against policy, vaults and injects credentials, optionally runs an LLM safety check, and returns semantically formatted results. Full design in `docs/`.

---

## Stack

| Layer | Choice |
|---|---|
| Backend | Go 1.23+, `net/http` ServeMux (no router library) |
| Frontend | Vite + React 18 + TypeScript + Tailwind + shadcn/ui |
| Database | Postgres (prod) + SQLite (local), behind `Store` interface |
| Vault | AES-256-GCM keyfile (local) + GCP Secret Manager, behind `Vault` interface |
| Auth | JWT (HS256, access 15m + refresh 30d), bcrypt passwords |
| Hosting | Google Cloud Run |

**Key Go deps (minimal by design):**
- `github.com/jackc/pgx/v5` — Postgres driver
- `modernc.org/sqlite` — pure-Go SQLite (no CGO)
- `github.com/golang-jwt/jwt/v5`
- `gopkg.in/yaml.v3`
- `golang.org/x/crypto` — bcrypt
- `golang.org/x/oauth2`

No ORM. Raw SQL with a thin repository layer. All DB access through the `Store` interface.

---

## Directory Structure

```
clawvisor-gatekeeper/
├── cmd/server/main.go
├── internal/
│   ├── api/
│   │   ├── server.go                  # ServeMux, middleware wiring, route registration
│   │   ├── middleware/auth.go         # JWT + agent token validation
│   │   ├── middleware/logging.go      # Request logging
│   │   └── handlers/
│   │       ├── auth.go                # /api/auth/*
│   │       ├── health.go              # /health, /ready
│   │       ├── gateway.go             # /api/gateway/request (Phase 3)
│   │       ├── policies.go            # /api/policies/* (Phase 2)
│   │       ├── agents.go              # /api/agents/* (Phase 3)
│   │       ├── services.go            # /api/services, /api/oauth/* (Phase 3)
│   │       ├── approvals.go           # /api/approvals/* (Phase 4)
│   │       ├── audit.go               # /api/audit/* (Phase 4)
│   │       ├── review.go              # /api/review/* (Phase 7)
│   │       └── mcp.go                 # /mcp (Phase 8)
│   ├── auth/
│   │   ├── jwt.go
│   │   └── password.go
│   ├── config/config.go
│   ├── store/
│   │   ├── store.go                   # Store interface + shared types
│   │   ├── postgres/
│   │   │   ├── postgres.go
│   │   │   ├── store.go
│   │   │   └── migrations/            # 001_init.sql, 002_policies.sql, 003_audit.sql, ...
│   │   └── sqlite/
│   │       ├── sqlite.go
│   │       ├── store.go
│   │       └── migrations/
│   ├── vault/
│   │   ├── vault.go                   # Vault interface
│   │   ├── local.go                   # AES-256-GCM + keyfile
│   │   └── gcp.go                     # GCP Secret Manager
│   ├── policy/
│   │   ├── types.go
│   │   ├── parser.go
│   │   ├── validator.go
│   │   ├── compiler.go
│   │   ├── evaluator.go               # Pure function, no I/O
│   │   ├── conflict.go
│   │   ├── registry.go                # In-memory per-user compiled rules
│   │   └── evaluator_test.go
│   ├── adapters/
│   │   ├── adapter.go                 # Adapter interface + Registry
│   │   ├── format/                    # Shared formatting helpers
│   │   ├── google/
│   │   │   ├── gmail/adapter.go
│   │   │   ├── calendar/adapter.go    # Phase 6
│   │   │   ├── drive/adapter.go       # Phase 6
│   │   │   └── contacts/adapter.go    # Phase 6
│   │   └── github/adapter.go          # Phase 6
│   ├── safety/checker.go              # LLM safety check (optional)
│   ├── notify/
│   │   ├── notify.go                  # Notifier interface
│   │   └── telegram/telegram.go
│   ├── callback/callback.go           # Async result delivery to agent
│   ├── audit/audit.go                 # Thin wrapper over store.LogAudit
│   ├── review/                        # Phase 7: retrospective analysis
│   │   ├── engine.go
│   │   └── detector/
│   └── mcp/                           # Phase 8
│       ├── server.go
│       ├── tools.go
│       ├── handler.go
│       └── format.go
├── web/
│   ├── src/
│   │   ├── api/client.ts
│   │   ├── pages/
│   │   ├── components/ui/             # shadcn components
│   │   └── hooks/useAuth.ts
│   ├── vite.config.ts
│   └── package.json
├── deploy/
│   ├── Dockerfile
│   ├── cloudbuild.yaml
│   ├── cloudrun.yaml
│   └── docker-compose.yml
├── docs/                              # All design docs — read before implementing each phase
├── config.example.yaml
├── go.mod
├── Makefile
└── CLAUDE.md
```

---

## Dev Commands

```bash
# Backend
go build ./...                        # compile check
go test ./...                         # all tests
go test ./internal/policy/...         # policy engine tests (critical)
go vet ./...

# Run locally (SQLite mode, no Docker needed)
DATABASE_DRIVER=sqlite JWT_SECRET=dev-secret go run ./cmd/server

# Run locally with Postgres
docker compose up                     # starts app + postgres
# or: docker compose up postgres -d  # just the DB, then go run ./cmd/server

# Frontend (in web/)
npm install
npm run dev                           # dev server on :5173, proxies /api to :8080
npm run build                         # builds to web/dist/

# Full local build (Go binary + frontend)
make build

# Run migrations manually (handled automatically on startup)
make migrate

# Cloud Run deploy
make deploy                           # triggers Cloud Build
```

---

## Phase Status

Read the full spec for the current phase before writing any code. Each phase doc is in `docs/PHASE-N-*.md`.

- [ ] **Phase 1** — Foundation & Infrastructure ← START HERE
- [ ] **Phase 2** — Policy Engine
- [ ] **Phase 3** — Core Gateway & First Adapter (Gmail)
- [ ] **Phase 4** — Dashboard (Frontend)
- [ ] **Phase 5** — OpenClaw Skill
- [ ] **Phase 6** — Extended Adapters (Calendar, Drive, Contacts, GitHub)
- [ ] **Phase 7** — Retrospective Review
- [ ] **Phase 8** — MCP Server
- [ ] **Phase 9** — Production Hardening

Mark a phase complete by checking its box when all success criteria in its doc are met.

---

## Per-Phase Implementation Notes

### Phase 1 — Foundation
- Generate a 32-byte `vault.key` on first run if missing (`os.Stat` → if not exists → `crypto/rand`). `chmod 600` it.
- JWT secret must be set via env (`JWT_SECRET`); fail fast on startup if missing.
- Both Postgres and SQLite migrations live in separate subdirectories. SQLite uses `TEXT` everywhere (no `TIMESTAMPTZ`, no `JSONB`). Run migrations on startup in both implementations.
- Refresh tokens: store SHA-256 hash in `sessions` table, return raw token once.
- The frontend shell for Phase 1 is login/register pages only. `Dashboard.tsx` is a placeholder.
- Health endpoint (`/health`) always returns 200. Readiness (`/ready`) checks DB ping and vault connectivity.

### Phase 2 — Policy Engine
- `evaluator.go` must be a pure function — zero I/O. If you find yourself reaching for a DB call or adapter call inside `Evaluate`, stop and use `EvalRequest.ResolvedConditions` instead.
- `rules_json` column does **not** exist. Store `rules_yaml` only. Compile on startup and on each write.
- Policy precedence: block > require_approval > allow. Default (no match) → approve (not block).
- Role resolution: global rules (no role) always apply. Role-specific rules layer on top and win on conflict. Agent with no role gets global rules only.
- Response filters are collected from ALL matched rules (not just the deciding rule) and returned in `PolicyDecision.ResponseFilters`.
- `POST /api/policies` and `PUT /api/policies/:id` return `409` only for hard conflicts (opposing block/allow, no condition differentiation). Shadowed rules are warnings in the response body.
- Write unit tests in `evaluator_test.go` covering all cases listed in the phase doc before moving on.

### Phase 3 — Gateway & Gmail Adapter
- Google credential vault key is `"google"` (not `"google.gmail"`). All Google adapters share this key. `service_meta` tracks each `google.*` service separately.
- `SafetyChecker.Check` signature: `(ctx, userID, req *gateway.Request, result *adapters.Result)`. It checks the **formatted** adapter result, never raw params alone.
- Audit log schema includes `safety_flagged`, `safety_reason`, `filters_applied`, `data_origin` index from day one. `request_id` has a `UNIQUE` constraint in `audit_log`.
- Gateway handler flow: policy evaluate → block/approve/execute. On execute: vault inject → adapter → semantic format → apply response filters → LLM safety check (if enabled) → audit log → respond.
- `session_id` does not exist in the gateway request context. Only `callback_url`.
- Pending approvals survive server restarts: re-register Telegram callback listeners on startup.
- LLM safety checker is disabled by default (`safety.enabled: false`). Implement the interface; don't require a real LLM endpoint for tests.
- Expiry cleanup goroutine runs every minute; delivers timeout callbacks before deleting records.

### Phase 4 — Dashboard
- Access token in React memory (context), refresh token in `httpOnly` cookie. Auto-refresh on 401.
- Approval panel polls `GET /api/approvals` — no WebSocket needed. First response (browser or Telegram) wins; the second is a no-op.
- YAML editor: CodeMirror + `@codemirror/lang-yaml`. Debounce validation calls 500ms.
- Dry-run evaluator accepts optional `role` field so policies can be tested per-role.
- Policy list is filterable by role; editor shows "All agents (global)" or the targeted role name.

### Phase 5 — OpenClaw Skill
- The skill is a `SKILL.md` file, not Go code. Write it to `skills/clawvisor/SKILL.md`.
- The curl example in the skill uses `callback_url` in context, not `session_id`.
- Agents should always populate `context.data_origin` when processing external content — enforce this in the skill instructions.

### Phase 6 — Extended Adapters
- All Google adapters retrieve credentials from `vault.Get(userID, "google")`.
- `recipient_in_contacts` is resolved in the **gateway handler** before calling `Evaluate`. Pass result via `EvalRequest.ResolvedConditions["recipient_in_contacts"]`. The evaluator stays pure.
- Adding a new Google scope requires re-consent: check if requested scopes are a subset of stored scopes; if not, trigger the OAuth flow again.
- Adapter interface additions for MCP support (`ActionDescription`, `ActionSchema`) can be stubbed in Phase 6 and filled in during Phase 8.

### Phase 7 — Retrospective Review
- Review engine runs as a background goroutine. Schedule relative to user's configured timezone.
- Detectors are independent and should each have unit tests with fixture audit log data.
- `info` findings → dashboard only. `warning`/`critical` → Telegram notification.
- `POST /api/review/run` triggers a synchronous run for the requesting user and returns `202 Accepted`.

### Phase 8 — MCP Server
- MCP approval timeout: `mcp.approval_timeout` (default: 240s), separate from `approval.timeout` (300s). Cloud Run `timeoutSeconds` is 360s.
- Tool naming: `"google.gmail" + "list_messages"` → `"google_gmail__list_messages"` (dots → underscores, double underscore before action).
- MCP tool calls block until resolved; HTTP gateway calls return `pending` immediately. Same underlying gateway handler — just different response strategies.
- Check if `github.com/modelcontextprotocol/go-sdk` is stable before adopting; if not, implement HTTP+SSE transport directly (~300 lines).

### Phase 9 — Production Hardening
- Switch all logging to `slog` with JSON output. Never log credentials, tokens, or raw params that may contain PII.
- Rate limiter is per-agent (gateway) and per-user (OAuth, policy writes). Use `golang.org/x/time/rate` token bucket.
- Retry adapter calls on 429/5xx (max 3 attempts, 500ms base exponential backoff). Never retry 400/401/403/404.
- Cloud Run `minScale: 1` — required for async approval callbacks to reach a live process.

---

## Architectural Invariants

These must never be violated, in any phase:

1. **Agent never receives credentials.** Credentials are injected inside the Clawvisor process after policy approval and safety check. They are stripped from all responses, logs, and error messages.

2. **Policy evaluator is pure.** `Evaluate(EvalRequest, []CompiledRule) → PolicyDecision` has no I/O. Async conditions are pre-resolved by the caller and passed in `ResolvedConditions`.

3. **`reason` and `context` fields are logged but never affect policy decisions.** Policy operates on `service` + `action` + `params` only.

4. **LLM safety check receives formatted output, not raw API responses.** It sees `*adapters.Result` (after semantic formatting and response filters). Raw email bodies / API blobs never reach the safety checker.

5. **Default is approve, not block.** Unmatched requests go to the approval queue. Only an explicit `allow: false` rule blocks outright.

6. **Audit every request, always.** Log before executing, update outcome after. Never skip logging on any code path.

7. **`request_id` is one-time use.** Enforced by `UNIQUE` constraint on `audit_log.request_id`. Validate before processing.

8. **No credentials in logs.** `params_safe` strips `_credential` and redacts keys matching `/token|secret|key|password|auth/i`.

---

## Key Patterns

### Vault injection
```go
// Never pass raw credential to adapter. Always inject via vault:
credBytes, err := vault.Get(ctx, userID, serviceID)  // serviceID is "google" for all Google services
// merge into params as _credential key; strip before any logging
```

### Gateway handler decision routing
```go
// Canonical order: evaluate → block/approve/execute
// On execute: inject → adapt → format → filter → safety check → log → respond
// Safety check flags → reroute to approve (same approval flow as policy approve)
```

### Response filter pipeline
```
Raw API response → Semantic Formatter → Structural Filters → Semantic Filters → Safety Check → Agent
```
Structural filters always before semantic. Safety check always sees fully filtered output.

### Adding a new adapter
1. Implement `Adapter` interface in `internal/adapters/<service>/adapter.go`
2. Use helpers from `internal/adapters/format/` for `SanitizeText`, `StripSecrets`, etc.
3. Register in adapter registry at startup (`cmd/server/main.go`)
4. Add `ActionDescription` and `ActionSchema` methods (used by MCP in Phase 8)
5. For Google services: use vault key `"google"`, register service in `service_meta`

### Migration numbering
`001_init.sql`, `002_policies.sql`, `003_audit.sql`, ... Run in order on startup. Both `postgres/migrations/` and `sqlite/migrations/` must be kept in sync (SQLite uses `TEXT` types).
