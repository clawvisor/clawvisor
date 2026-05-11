# internal/api

## OVERVIEW
HTTP server layer for Clawvisor. `net/http` ServeMux with manual middleware chaining (no Gin/Echo). Routes are registered in `server.go` via `mux.HandleFunc("METHOD /path", handler)` using Go 1.22+ method-pattern routing. Every gateway request passes through three authorization layers: Restrictions (hard block) → Task scopes (approved purpose) → Per-request approval (fallback queue).

## STRUCTURE
```
internal/api/
├── server.go              # Server struct, constructor, routes(), middleware wiring (1314 lines)
├── client_ip.go            # Trusted-proxy IP extraction for rate limiting
├── handlers/               # 43 files — request handlers
│   ├── gateway.go          # Gateway request flow, intent verification, adapter dispatch (2399 lines)
│   ├── services.go         # OAuth connect/disconnect, token refresh, service lifecycle (2383 lines)
│   ├── tasks.go            # Task CRUD, scope expansion, approval callbacks (2159 lines)
│   ├── runtime.go          # Runtime proxy session management (850 lines)
│   ├── approvals.go        # Approval queue, per-request decisions (794 lines)
│   ├── agents.go           # Agent token CRUD, rotation
│   ├── auth.go             # Login, register, magic link, refresh, logout
│   ├── audit.go            # Gateway audit log queries
│   ├── connections.go      # Agent connection request flow
│   ├── devices.go          # Device pairing, E2E key exchange
│   ├── events.go           # SSE event stream (real-time dashboard updates)
│   ├── health.go           # /health, /ready, version endpoints
│   ├── llm.go             # LLM status and config update
│   ├── notifications.go    # Telegram/push notification setup
│   ├── onboarding.go       # First-run onboarding flow
│   ├── overview.go         # Dashboard overview stats
│   ├── pairing.go          # Relay pairing code generation
│   ├── restrictions.go     # Restriction CRUD
│   ├── skill.go            # Skill catalog for agents
│   ├── welcome.go          # Welcome screen data
│   └── ...                 # Dedup caches, token caches, extraction trackers, batch handlers
├── middleware/              # 12 files — HTTP middleware
│   ├── auth.go             # RequireUser, RequireAgent, OptionalUser (JWT validation)
│   ├── logging.go          # Request logging with trace IDs
│   ├── recover.go          # Panic recovery → 500 JSON error
│   ├── ratelimit.go        # Per-key rate limiting (agent, user, IP)
│   ├── cors.go             # CORS for relay-originated requests
│   ├── security.go         # Security headers (HSTS, CSP, X-Frame-Options)
│   ├── device.go           # Device HMAC authentication
│   ├── e2e.go              # E2E encryption/decryption for relay traffic
│   ├── agent.go            # Agent context extraction from JWT
│   ├── logfields.go        # Structured log field helpers
│   ├── replay_cache.go     # Replay attack protection
│   └── replay_cache_redis.go  # Redis-backed replay cache
└── *_test.go               # Integration tests at the api package level
```

## WHERE TO LOOK
| Task | Location | Notes |
|------|----------|-------|
| Add an API endpoint | `handlers/<domain>.go` + `server.go` routes() | Register with `mux.Handle("METHOD /path", ...)` |
| Change auth flow | `handlers/auth.go`, `middleware/auth.go` | JWT validation in middleware, token issuance in handler |
| Change gateway logic | `handlers/gateway.go` | Authorization layers, adapter dispatch, intent verification |
| Add middleware | `middleware/<name>.go` | Returns `func(http.Handler) http.Handler`, wire in `server.go` routes() |
| Change rate limiting | `middleware/ratelimit.go`, `server.go` | Keyed limiters created in routes(), per-bucket config |
| Add a service adapter route | `handlers/services.go` | OAuth flows, token refresh, connect/disconnect |
| Change task/approval flow | `handlers/tasks.go`, `handlers/approvals.go` | Task lifecycle, scope expansion, approval decisions |
| Change SSE events | `handlers/events.go` | Ticket-based SSE for real-time dashboard |
| Change E2E encryption | `middleware/e2e.go` | X25519 ECDH + HKDF for relay traffic |

## CONVENTIONS
- Handlers are structs with injected dependencies (store, vault, adapter registry, notifier). Constructed via `NewXHandler()` in `server.go` routes().
- Middleware is `func(http.Handler) http.Handler`. Chained manually: `requireUser(middleware.RateLimit(...)(h))`.
- Route patterns use Go 1.22+ method syntax: `"GET /api/tasks"`, `"POST /api/gateway/request"`.
- Auth middleware produces context values: `middleware.UserFromContext()`, `middleware.AgentFromContext()`.
- Handler methods are `http.HandlerFunc` or `http.Handler`. Error responses use a shared `writeError(w, status, code, msg)` pattern.
- Rate limiters are keyed by agent ID, user ID, or client IP (with trusted-proxy awareness from `client_ip.go`).
- Feature flags (`FeatureSet`) gate routes at registration time, not runtime. Password auth, multi-tenant, and passkeys are feature-gated.
- Redis-backed stores (dedup cache, replay cache, extraction tracker, OAuth state, token cache, pairing store) are injected via `Set*()` methods and fall back to in-memory when Redis isn't configured.

## ANTI-PATTERNS
- NEVER log credentials, tokens, or full adapter request/response bodies
- NEVER return raw adapter responses to agents without sanitization (secrets, HTML, Unicode)
- NEVER skip the authorization chain (restrictions → task scopes → approval) for gateway requests
- NEVER register routes outside `routes()` — all wiring happens in one place
- NEVER use a web framework; this is plain `net/http` + ServeMux
- NEVER write middleware that doesn't return `func(http.Handler) http.Handler`