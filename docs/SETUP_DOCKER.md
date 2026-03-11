# Clawvisor — Docker Setup Guide

You are setting up Clawvisor locally using Docker Compose. This is the
simplest path if the user already has Docker installed — no Go or Node
toolchain needed. Follow these instructions step by step. Ask the user for
clarification when the environment is ambiguous — do not guess silently.

---

## Goal State

When setup is complete, the user should have:

1. Clawvisor running locally via Docker Compose with Postgres
2. A magic link URL to sign into the dashboard

---

## Step 1: Check prerequisites

Verify Docker is installed and running:

```bash
docker --version && docker compose version
```

If not installed, instruct the user:
- macOS: Install [Docker Desktop](https://www.docker.com/products/docker-desktop/)
- Linux: https://docs.docker.com/engine/install/

---

## Step 2: Locate the Clawvisor repository

Check if the current working directory is the Clawvisor repository:

```bash
ls deploy/Dockerfile deploy/docker-compose.yml 2>/dev/null
```

If both exist, use the current directory as `$CLAWVISOR_REPO`.

If not, search common locations:

```bash
ls -d ~/code/clawvisor 2>/dev/null \
  || ls -d ~/clawvisor 2>/dev/null \
  || ls -d ~/projects/clawvisor 2>/dev/null
```

Also check for alternate naming (`clawvisor-public`, `clawvisor-oss`). The key
indicator is a directory containing `deploy/Dockerfile`.

If not found, ask the user where the repository is checked out — or offer to
clone it:

```bash
git clone https://github.com/clawvisor/clawvisor.git
```

---

## Step 3: Configure services (optional)

Before starting Clawvisor, ask the user what they want to configure. These
are passed as environment variables to the Docker container.

### Intent Verification (LLM)

Clawvisor uses a lightweight LLM to verify that agent requests match their
approved task scope — this is a core safety feature. Ask which model the
user wants to use:

| Option | Provider | Endpoint | Model |
|--------|----------|----------|-------|
| Claude Haiku | `anthropic` | `https://api.anthropic.com/v1` | `claude-haiku-4-5-20251001` |
| Gemini Flash | `openai` | `https://generativelanguage.googleapis.com/v1beta/openai` | `gemini-2.0-flash` |
| GPT-4o Mini | `openai` | `https://api.openai.com/v1` | `gpt-4o-mini` |

Then ask for their **API key** for the chosen provider.

### Google Services (Gmail, Calendar, Drive, Contacts)

Ask: **"Do you want to connect Google services (Gmail, Calendar, Drive,
Contacts)? This requires a Google Cloud OAuth 2.0 client."**

If yes, they need:
- **Google Client ID** and **Google Client Secret** from the Google Cloud
  Console. Point to:
  https://github.com/clawvisor/clawvisor/blob/main/docs/GOOGLE_OAUTH_SETUP.md

The OAuth redirect URL will be `http://localhost:25297/api/oauth/callback`.

---

## Step 4: Start Clawvisor

Write a `.env` file with any configuration collected in Step 3. The compose
file references these variables via `${VAR:-}` interpolation, and
`--env-file` makes them available during composition:

```bash
cd "$CLAWVISOR_REPO"

# Generate secrets
./deploy/init.sh .env.docker

# Add service configuration
cat >> .env.docker <<EOF
GOOGLE_CLIENT_ID=<from Step 3, or empty>
GOOGLE_CLIENT_SECRET=<from Step 3, or empty>
CLAWVISOR_LLM_VERIFICATION_ENABLED=<true or false>
CLAWVISOR_LLM_VERIFICATION_PROVIDER=<from Step 3, or empty>
CLAWVISOR_LLM_VERIFICATION_ENDPOINT=<from Step 3, or empty>
CLAWVISOR_LLM_VERIFICATION_MODEL=<from Step 3, or empty>
CLAWVISOR_LLM_VERIFICATION_API_KEY=<from Step 3, or empty>
EOF

docker compose -f deploy/docker-compose.yml --env-file .env.docker up -d --build
```

Wait for it to become ready:

```bash
until curl -sf http://localhost:25297/ready >/dev/null 2>&1; do sleep 1; done
```

This typically takes 30–60 seconds on first build.

Postgres data is persisted in the `postgres_data` Docker volume. The vault
master key is provided via the `VAULT_KEY` environment variable.

> **OpenClaw users:** If you're integrating with OpenClaw, use
> `docker-compose.openclaw.yml` instead — it includes private-network
> callback support and a shared data volume. See
> [INTEGRATE_OPENCLAW.md](INTEGRATE_OPENCLAW.md) for the full guide.

---

## Step 5: Extract the magic link

Get the dashboard sign-in URL from the container logs:

```bash
docker compose -f deploy/docker-compose.yml logs app 2>&1 \
  | grep -o 'http://[^ ]*magic-link?token=[^ ]*' | tail -1
```

Present the magic link to the user and instruct them to open it in their
browser to sign into the dashboard.

---

## Step 6: Summary

Present the user with:

```
Clawvisor Docker Setup Complete
─────────────────────────────────
Dashboard:  http://localhost:25297
Sign in:    <magic link from Step 5>
Database:   Postgres (Docker volume)

To stop:  docker compose -f deploy/docker-compose.yml down
```

Remind the user to:
- Open the magic link to access the dashboard
- Connect services under the **Services** tab:
  - Google (Gmail, Calendar, Drive, Contacts) — one OAuth connection covers
    all four. Requires Google OAuth credentials from Step 3.
  - GitHub, Slack, Notion, Linear, Stripe, Twilio — activate with API
    keys/tokens

**Next:** Connect your agent — see [SETUP.md](SETUP.md#2-connect-your-agent)
for integration guides.
