# Clawvisor — Local Setup Guide

You are setting up Clawvisor natively on the user's local machine. No Docker,
no Postgres — Go, Node, and SQLite only. Follow these instructions step by
step. Ask the user for clarification when the environment is ambiguous — do
not guess silently.

---

## Goal State

When setup is complete, the user should have:

1. Go and Node.js installed
2. Clawvisor built and running locally with SQLite
3. A `config.yaml` generated (via the setup wizard or manually)
4. A magic link URL to sign into the dashboard
5. An agent token created
6. The TUI dashboard working (optional)

---

## Step 1: Check prerequisites

Verify Go 1.25+ is installed:

```bash
go version
```

If not installed, instruct the user:
- macOS: `brew install go`
- Other: https://go.dev/doc/install

Verify Node.js 18+ is installed:

```bash
node --version
```

If not installed, instruct the user:
- macOS: `brew install node`
- Other: https://nodejs.org/

---

## Step 2: Locate the Clawvisor repository

Check if the current working directory is the Clawvisor repository:

```bash
ls Makefile cmd/clawvisor/main.go 2>/dev/null
```

If both exist, use the current directory as `$CLAWVISOR_REPO`.

If not, search common locations:

```bash
ls -d ~/code/clawvisor 2>/dev/null \
  || ls -d ~/clawvisor 2>/dev/null \
  || ls -d ~/projects/clawvisor 2>/dev/null
```

Also check for alternate naming (`clawvisor-public`, `clawvisor-oss`). The key
indicator is a directory containing `Makefile` and `cmd/clawvisor/main.go`.

If not found, ask the user where the repository is checked out — or offer to
clone it:

```bash
git clone https://github.com/clawvisor/clawvisor.git
```

Store the path as `$CLAWVISOR_REPO` for subsequent steps.

---

## Step 3: Run the setup wizard

The setup wizard builds the binary and walks the user through configuration
interactively. It requires a terminal with stdin attached.

```bash
cd "$CLAWVISOR_REPO" && make setup
```

This covers:

- **Database** — defaults to SQLite (press Enter)
- **Intent verification** — Clawvisor uses a lightweight LLM to verify that
  agent requests match their approved task scope. This is a core safety
  feature. The wizard will ask which model to use and for an API key. Options:

| Option | Provider | Endpoint | Model |
|--------|----------|----------|-------|
| Claude Haiku | `anthropic` | `https://api.anthropic.com/v1` | `claude-haiku-4-5-20251001` |
| Gemini Flash | `openai` | `https://generativelanguage.googleapis.com/v1beta/openai` | `gemini-2.0-flash` |
| GPT-4o Mini | `openai` | `https://api.openai.com/v1` | `gpt-4o-mini` |

- **Google OAuth** — needed for Gmail, Calendar, Drive, and Contacts. Requires
  a Google Cloud OAuth 2.0 client ID and secret. Point the user to:
  https://github.com/clawvisor/clawvisor/blob/main/docs/GOOGLE_OAUTH_SETUP.md
- **iMessage** — macOS only, reads the local `chat.db`

The wizard generates `config.yaml` in the repository root and writes
`~/.clawvisor/config.yaml` with the server URL so the TUI knows where to
connect.

**If the setup wizard cannot run** (e.g. no interactive terminal), skip to the
alternative in Step 4.

---

## Step 4: Start the server

```bash
cd "$CLAWVISOR_REPO" && make run
```

On first run, Clawvisor:

1. Auto-selects SQLite (creates `clawvisor.db` in the working directory)
2. Generates `vault.key` — the master encryption key for stored credentials
3. Creates an `admin@local` user
4. Prints a magic link URL and opens it in the user's browser
5. Writes `~/.clawvisor/.local-session` for TUI auto-login

The server listens on `http://localhost:25297` by default.

### Alternative: start without the setup wizard

If the user skipped `make setup`, build and run with env vars:

```bash
cd "$CLAWVISOR_REPO" && make build
DATABASE_DRIVER=sqlite JWT_SECRET=dev-secret ./bin/clawvisor server
```

Or without building:

```bash
DATABASE_DRIVER=sqlite JWT_SECRET=dev-secret go run ./cmd/server
```

### Verify the server is running

```bash
curl -sf http://localhost:25297/ready 2>/dev/null && echo "RUNNING" || echo "NOT RUNNING"
```

---

## Step 5: Extract the magic link

The magic link is printed to the server's stdout during startup. If you have
access to the terminal output, grab it from there.

If the server is running in the background, check the logs or look for the
local session file:

```bash
cat ~/.clawvisor/.local-session 2>/dev/null
```

This contains a JSON object with `server_url` and `magic_token`. The magic
link is `${server_url}/magic-link?token=${magic_token}`.

Present the magic link to the user and instruct them to open it in their
browser to sign into the dashboard.

---

## Step 6: Create an agent

Create an agent token using the CLI. Use `--replace` so this is safe to
re-run:

```bash
cd "$CLAWVISOR_REPO" && ./bin/clawvisor agent create my-agent --replace --json
```

This returns JSON with `id`, `name`, and `token` fields. Save the `token`
value — it is shown only once.

Other useful commands:

```bash
./bin/clawvisor agent list              # List all agents
./bin/clawvisor agent list --json       # JSON output
./bin/clawvisor agent delete my-agent   # Delete by name or ID
```

---

## Step 7: TUI dashboard (optional)

If the user wants the terminal dashboard, launch it in a separate terminal:

```bash
cd "$CLAWVISOR_REPO" && make tui
```

The TUI auto-authenticates using `~/.clawvisor/.local-session` written by the
server in Step 4. On first launch it exchanges the session token for a refresh
token and saves it to `~/.clawvisor/config.yaml` for future use.

Verify it connects:

```bash
ls ~/.clawvisor/config.yaml 2>/dev/null
```

---

## Step 8: Summary

Present the user with:

```
Clawvisor Local Setup Complete
────────────────────────────────
Dashboard:  http://localhost:25297
Sign in:    <magic link from Step 5>
Agent:      my-agent
TUI:        make tui (in a separate terminal)

To stop:    Ctrl+C in the server terminal
```

Remind the user to:
- Open the magic link to access the dashboard
- Connect services under the **Services** tab:
  - Google (Gmail, Calendar, Drive, Contacts) — one OAuth connection covers
    all four. Requires Google OAuth credentials from `make setup`.
  - GitHub, Slack, Notion, Linear, Stripe, Twilio — activate with API
    keys/tokens
  - iMessage — always available on macOS without activation

---

## Configuration reference

Clawvisor loads `config.yaml` from the working directory (override with
`CONFIG_FILE` env var). Key environment variable overrides:

| Env var | Default | Purpose |
|---|---|---|
| `DATABASE_DRIVER` | auto (`sqlite` locally) | `sqlite` or `postgres` |
| `DATABASE_URL` | — | Postgres connection string |
| `JWT_SECRET` | auto-generated locally | HMAC signing key for JWTs |
| `PORT` | `25297` | Server listen port |
| `SERVER_HOST` | `127.0.0.1` | Server bind address |
| `PUBLIC_URL` | — | Used in Telegram notification links |
| `VAULT_BACKEND` | `local` | `local` (AES-256-GCM) or `gcp` (Secret Manager) |
| `VAULT_KEY_FILE` | `./vault.key` | Path to local vault encryption key |
| `AUTH_MODE` | auto (`magic_link` locally) | `magic_link` or `password` |
| `GOOGLE_CLIENT_ID` | — | Google OAuth client ID |
| `GOOGLE_CLIENT_SECRET` | — | Google OAuth client secret |
| `LOG_FORMAT` | auto (`text` locally) | `json` or `text` |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

See `config.example.yaml` for the full reference.
