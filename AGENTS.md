# PROJECT KNOWLEDGE BASE

**Generated:** 2026-05-11T17:31:40Z
**Commit:** b76d9f4
**Branch:** main

## OVERVIEW
Clawvisor is an AI agent gatekeeper service. Backend: Go 1.25 (`net/http`). Frontend: React 18 + TypeScript + Vite + Tailwind.

## STRUCTURE
```
.
├── cmd/                    # CLI entry points (clawvisor, server, clawvisor-local)
├── internal/               # Private packages
│   ├── adapters/           # Service adapters (Google, GitHub, Slack, etc.)
│   ├── api/                # HTTP server, handlers, middleware
│   ├── auth/               # JWT, magic links, passwords
│   ├── daemon/             # Daemon lifecycle, relay, pairing
│   ├── intent/             # LLM intent verification, chain context
│   ├── llm/                # LLM HTTP client
│   ├── mcp/                # MCP server (OAuth 2.1)
│   ├── notify/             # Telegram, push notifications
│   ├── runtime/            # Runtime policy
│   ├── server/             # Server bootstrap
│   ├── setup/              # Interactive setup wizard
│   ├── taskrisk/           # Task risk assessment
│   ├── telemetry/          # Usage telemetry
│   ├── tui/                # Terminal dashboard
│   └── vault/              # Vault implementations
├── pkg/                    # Public/shared packages
│   ├── adapters/           # Adapter interface
│   ├── auth/               # Token service interface
│   ├── clawvisor/          # App bootstrap
│   ├── config/             # Configuration loading
│   ├── gateway/            # Gateway types
│   ├── notify/             # Notifier interface
│   ├── runtime/proxy/      # Runtime proxy (preview)
│   ├── store/              # Store interface + implementations
│   ├── vault/              # Vault interface + implementations
│   └── version/            # Version info
├── web/                    # React frontend
├── skills/                 # Agent skill definitions
├── extensions/             # OpenClaw webhook plugin
├── deploy/                 # Docker, Cloud Run configs
└── docs/                   # Setup and integration guides
```

## WHERE TO LOOK
| Task | Location | Notes |
|------|----------|-------|
| Add a service adapter | `internal/adapters/` | Implement Adapter interface from `pkg/adapters/` |
| Add an API endpoint | `internal/api/handlers/` | Wire in `internal/api/routes.go` |
| Change database schema | `pkg/store/sqlite/migrations/`, `pkg/store/postgres/migrations/` | Add up+down SQL |
| Change frontend UI | `web/src/` | React + Tailwind + Vite |
| Change agent protocol | `skills/clawvisor/SKILL.md.tmpl` | Run `make eval-intent` after |
| Add LLM eval case | `internal/intent/testdata/eval_cases.json` | Run `make eval-intent` |
| Build release binaries | `scripts/build-release.sh` | Called by `make release` |
| iMessage helper release | `cmd/imessage-helper/` | macOS only, separate version pin |

## CONVENTIONS
- Interfaces in `pkg/`, implementations in `internal/` (or `pkg/` for shared impls like store)
- `gofmt` standard formatting
- Wrap errors with context at boundaries: `fmt.Errorf("doing X: %w", err)`
- Comments: write sparingly, only for non-obvious *why*
- Commits: Conventional Commits (`feat`, `fix`, `docs`, `refactor`, `chore`)
- One concern per PR

## ANTI-PATTERNS (THIS PROJECT)
- NEVER log credentials, tokens, or full adapter request/response bodies
- NEVER emit `null` or `[]` for `missing_chain_values` in intent extraction
- NEVER extract JSON key names as chain-context fact values
- NEVER truncate `fact_value` in chain context — drop the fact instead
- NEVER extract sensitive content (OTPs, PINs, banking secrets) in chain context
- Don't bundle refactors with feature work
- Don't file public issues for security vulnerabilities (email security@clawvisor.com)

## UNIQUE STYLES
- `net/http` ServeMux with manual middleware (no framework like Gin/Echo)
- Three binary targets: `clawvisor` (CLI), `clawvisor-local` (macOS menu bar), `server` (standalone)
- Gateway authorization: Restrictions → Task scopes → Per-request approval (3 layers)
- Session vs standing tasks; scope expansion requires re-approval
- LLM subsystems: intent verification, chain context extraction, task risk assessment
- Runtime proxy (preview): TLS-terminating MITM proxy for observing agent API calls
- Relay: WebSocket reverse tunnel to `relay.clawvisor.com` for mobile NAT traversal
- E2E encryption: X25519 ECDH + HKDF + AES for device-authenticated endpoints
- MCP server: OAuth 2.1 provider for Claude Desktop integration

## COMMANDS
```bash
make build              # Build Go binary + frontend
make test               # Run all Go unit tests
make test-e2e           # Build + run E2E smoke tests
make test-e2e-ci        # Build + run CI E2E subset
make eval-intent        # Run intent verification eval suite (249 cases)
make lint               # go vet ./...
make run                # Build + start dev server (SQLite, magic link)
make tui                # Build + launch terminal dashboard
make setup              # Build + run interactive config wizard
make web-dev            # Start Vite dev server (port 8080)
make web-build          # TypeScript compile + Vite build
make up                 # Docker compose (app + postgres)
make deploy             # GCP Cloud Build deploy
make release            # Cross-platform release binaries
```

## NOTES
- `clawvisor-local` uses `CGO_ENABLED=1` on Darwin only (for iMessage helper); Linux uses `CGO_ENABLED=0`
- Air hot-reload (`.air.toml`) excludes `_test.go` files
- `scripts/dev.sh` runs Air + Vite in parallel with dynamic port finding
- iMessage helper is released separately with its own version pin (`pkg/version/imessage_helper.go`)
