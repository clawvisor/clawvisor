# Repository Instructions

## Orientation

Clawvisor is a Go-based gateway between AI agents and external APIs. Agents do not hold downstream credentials; they declare tasks and send gateway requests, while Clawvisor enforces restrictions, task scope, human approval, credential injection, execution through adapters, and audit logging.

Important areas:

- `cmd/` contains CLI and server entry points.
- `internal/` contains private implementation packages for the API server, gateway flow, adapters, auth, daemon commands, MCP, intent verification, notifications, runtime proxy, TUI, and related subsystems.
- `pkg/` contains public/shared packages such as adapter interfaces, config, gateway types, store, vault, runtime, notifications, and version metadata.
- `web/` contains the React + TypeScript + Vite dashboard.
- `docs/` contains setup, integration, architecture, runtime proxy, and adapter guidance. Start with `docs/ARCHITECTURE.md` for deeper system context.
- `skills/clawvisor/` contains the agent-facing Clawvisor skill and protocol documentation.
- `coworkplugin/` contains the CoWork plugin extension package.
- `e2e/smoke/` contains integration smoke tests that run against a full server binary (`make test-e2e`). `internal/e2e/lite/` contains YAML-based lite scenario tests that do not require a full build.
- `security/`, `deploy/`, `scripts/`, and `extensions/` contain security tooling, deployment assets, release/build scripts, and extension packages.

The scope drift system (`internal/runtime/llmproxy/scope_drift_*.go`) intercepts blocked tool calls and surfaces a continuation menu back to the agent before reaching the user. The agent can expand the active task, create a new task inline, or request a one-off approval with a rationale — only the one-off path reaches the user. Treat this system as security-sensitive: it controls which agents can bypass task scope restrictions and under what conditions.

The highest-risk areas are authorization, task scope evaluation, scope drift enforcement, credential/vault handling, adapter execution, approval behavior, callback signing, audit logging, and anything that could expose tokens, secrets, or unsanitized downstream data. Treat behavior changes in these areas as security-sensitive.

## Development

- Use Go 1.25+ and Node.js 18+.
- Common checks:
  - `make test` runs `go test ./...`.
  - `make lint` runs `go vet ./...`.
  - `make build` builds the Go binary and dashboard assets.
  - `make test-e2e` runs smoke integration tests against the full server binary.
  - `cd web && npm run lint && npm run build` type-checks and builds the dashboard.
  - `make eval-intent` is relevant for LLM-driven intent, chain-context, and task-risk changes and requires LLM configuration.
- Follow existing package boundaries: keep public/shared interfaces in `pkg/` and implementations in `internal/`.
- Use `gofmt` for Go changes and match existing React, TypeScript, and Tailwind patterns in `web/src/`.
- Do not log credentials, bearer tokens, OAuth tokens, vault contents, or full downstream request/response bodies.

## Documentation Maintenance

When making changes, evaluate whether this `AGENTS.md` file should be updated to reflect new architecture, workflows, commands, conventions, risk areas, or repo structure. Update it in the same change when the guidance future agents need would otherwise become stale or incomplete.

## Commits and Pull Requests

All commit messages and pull request titles should follow Conventional Commits semantics. Use a conventional type and optional scope, such as `feat(scope): summary`, `fix(scope): summary`, or `chore: summary`.
