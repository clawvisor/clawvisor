# Skills — Agent Protocol & Definitions

## OVERVIEW

The `skills/` package defines the agent-facing protocol that Clawvisor publishes to connected agents. It contains the SKILL.md template (the contract agents read), default policy files, Go embedding/rendering code, and a CLI renderer. Published to ClawHub via CI on release.

## STRUCTURE

```
skills/
├── clawvisor/
│   ├── SKILL.md.tmpl          # Agent protocol template (tasks, gateway, callbacks, batch)
│   ├── README.md              # ClawHub-facing readme
│   ├── e2e.mjs                # E2E encryption helper for agent requests
│   └── policies/
│       ├── imessage-safe-default.yaml   # iMessage: read-only, send blocked
│       ├── google-workspace-safe.yaml   # Google: read allowed, write requires approval, delete blocked
│       └── github-read-only.yaml        # GitHub: read allowed, write requires approval
├── embed.go                   # Go embed.FS mounting clawvisor/ directory
├── render.go                  # Template rendering (claude-code, cowork, mcp targets)
└── clawvisor-generate-integration.md  # Integration guide generator prompt
cmd/render-skill/               # CLI tool: renders SKILL.md.tmpl for a given target
.github/workflows/publish-skill.yml  # CI: publishes to ClawHub on release
```

## WHERE TO LOOK

| Task | Location | Notes |
|------|----------|-------|
| Change agent protocol text | `skills/clawvisor/SKILL.md.tmpl` | Rebuild with `go run ./cmd/render-skill` |
| Add a default policy | `skills/clawvisor/policies/` | YAML with `id`, `name`, `description`, `rules` |
| Change rendering logic | `skills/render.go` | Targets: `claude-code`, `cowork`, `mcp` |
| Change embedded files | `skills/embed.go` | `//go:embed all:clawvisor` |
| Render SKILL.md locally | `cmd/render-skill/main.go` | `go run ./cmd/render-skill [target]` |
| Publish to ClawHub | `.github/workflows/publish-skill.yml` | Triggered on release; also syncs cowork-plugins |
| Add adapter risk metadata | `pkg/adapters/yamldef/schema.go` | `RiskDef{Category, Sensitivity, Description}` |

## CONVENTIONS

- SKILL.md.tmpl uses Go `text/template` with conditional blocks per target (`TargetClaudeCode`, `TargetCowork`, `TargetMCP`). Add new targets in `render.go` first.
- Policy YAML files use `id`, `name`, `description`, and `rules` (list of service/action/allow/require_approval entries).
- `RenderWithOptions` accepts `ClawvisorURL`, `ViaRelay`, and `FeedbackEnabled` to bake instance-specific config into the rendered skill.
- The publish workflow checks for skill file changes before building; skips if nothing changed since the previous release tag.
- E2E encryption in `e2e.mjs` uses X25519 ECDH + HKDF + AES-256-GCM. Zero external dependencies.

## ANTI-PATTERNS

- NEVER set DELETE actions to `sensitivity: "low"` — validation rejects it. DELETE actions must be category `"delete"` with minimum `"medium"` sensitivity.
- NEVER set credential/token actions below `"high"` sensitivity.
- NEVER set bulk operations below `"high"` sensitivity.
- NEVER emit `null` or `[]` for `missing_chain_values` in intent extraction (root convention, applies here too).
- NEVER add curl examples that use line continuations or heredocs — agents execute single-line commands.
- NEVER publish to ClawHub without running `go run ./cmd/render-skill claude-code` first to verify the template renders cleanly.