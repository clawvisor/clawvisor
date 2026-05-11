# Contributing to Clawvisor

Thanks for your interest in contributing. Clawvisor is the gatekeeper between AI agents and the APIs they act on, so we hold a high bar for changes that touch authorization, credential handling, or audit behavior. This guide covers the baseline you need to know before opening an issue or PR.

## Before you start

- **Search first.** Check [open and closed issues](https://github.com/clawvisor/clawvisor/issues) and PRs to avoid duplicates.
- **Open an issue for non-trivial changes.** For anything beyond a small bug fix or doc tweak, file an issue first so we can agree on the approach before you invest time. Adding a new service adapter, changing the gateway protocol, or touching the vault/store interfaces always warrants an issue.
- **One concern per PR.** Smaller PRs land faster. Don't bundle refactors with feature work.

## Security disclosures

**Do not file public GitHub issues for security vulnerabilities.** Email `security@clawvisor.com` instead. Please include reproduction steps and the affected version. We will acknowledge receipt and coordinate a fix and disclosure timeline with you.

## License and contributions

Clawvisor is licensed under the [Elastic License 2.0](LICENSE). By submitting a contribution, you agree that your contribution is licensed under the same terms and that you have the right to submit it. We may add a Developer Certificate of Origin (DCO) sign-off requirement in the future; for now, opening a PR is sufficient.

## Development setup

Requirements:

- Go 1.25+
- Node.js 18+
- (Optional) Docker, for running Postgres locally or the docker-compose dev stack

Clone and run the local stack:

```bash
git clone https://github.com/clawvisor/clawvisor.git
cd clawvisor
make setup    # interactive config wizard — writes config.yaml and vault.key
make run      # start the server (SQLite, magic-link auth, opens dashboard)
```

See [docs/SETUP_LOCAL.md](docs/SETUP_LOCAL.md) for details, and [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for an overview of the codebase.

## Build, test, lint

The full check before pushing:

```bash
make build      # Go binary + frontend
make test       # go test ./...
make lint       # go vet
cd web && npx tsc --noEmit && npm run build
```

CI (`.github/workflows/ci.yml`) runs the Go test/vet pipeline and the frontend type-check + build on every PR against `main`. PRs must pass CI before review.

For changes to LLM-driven subsystems (intent verification, chain context, task risk), also run:

```bash
make eval-intent
```

This requires a configured LLM provider — see [docs/eval-results.md](docs/eval-results.md).

## Code style

- **Go:** standard `gofmt`. Use the existing patterns in `internal/` and `pkg/` — interfaces live in `pkg/`, implementations in `internal/`. Keep public surface area small.
- **Frontend:** React + TypeScript + Tailwind. Match the conventions in `web/src/`.
- **Comments:** write them sparingly, and only when the *why* is non-obvious. Don't restate what the code does.
- **Errors:** wrap with context (`fmt.Errorf("doing X: %w", err)`) at boundaries; don't swallow.
- **Logging:** never log credentials, tokens, or full request/response bodies from adapters. The audit log is the source of truth for gateway activity.

## Commit messages

We use [Conventional Commits](https://www.conventionalcommits.org/) — release-please reads them to generate the changelog and version bumps. Use one of:

- `feat(scope): ...` — user-visible feature
- `fix(scope): ...` — bug fix
- `docs(scope): ...` — documentation only
- `refactor(scope): ...` — no behavior change
- `chore(scope): ...` — tooling, deps, infra

Examples from recent history:

```
feat(llm): Gemini provider with explicit context caching
fix(linear): map issue_id to identifier and add strict assignee filters
refactor: merge internal/vault into pkg/vault
```

Scope is usually a package name (`llm`, `adapters`, `mcp`, `tui`) or a feature area. Keep the subject under ~70 characters; put detail in the body.

## Pull requests

1. Fork and branch from `main`.
2. Make your changes; add or update tests.
3. Run `make build && make test && make lint` locally.
4. Push and open a PR against `clawvisor/clawvisor:main`. Reference the issue it closes (`Closes #123`).
5. Fill in the PR description: what changed, why, and how you tested it. Screenshots for UI changes.
6. Address review feedback by pushing follow-up commits — we squash on merge, so don't worry about rebasing your branch history.

## Adding a service adapter

Adapters live in `internal/adapters/`. The interface is in `pkg/adapters/`. A new adapter typically needs:

- The adapter implementation in `internal/adapters/<service>/`
- Action registration so the gateway knows what actions exist
- Display names in `internal/display/`
- Updates to `README.md`'s supported-services table and the agent skill in `skills/clawvisor/SKILL.md`
- Tests covering parameter handling, response formatting (secrets stripped, HTML sanitized), and error mapping

For local adapters that touch the user's machine (like iMessage), also read [docs/LOCAL_ADAPTER_GUIDE.md](docs/LOCAL_ADAPTER_GUIDE.md).

If you're adding an adapter via integration YAML rather than Go code, see [docs/INTEGRATION_YAML_SPEC.md](docs/INTEGRATION_YAML_SPEC.md).

## What we're unlikely to accept

- Changes that weaken the authorization model (bypassing tasks, downgrading restrictions, logging credentials).
- Net-new dependencies without a clear justification.
- Large refactors without a prior issue to align on direction.
- Cosmetic-only changes to unrelated files.

## Questions

For usage questions, open a [Discussion](https://github.com/clawvisor/clawvisor/discussions) or ask in an issue. For anything sensitive, email `security@clawvisor.com`.
