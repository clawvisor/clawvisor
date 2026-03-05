# Clawvisor — Cloud Deployment Guide

You are deploying Clawvisor to the user's own infrastructure (VPS, container
platform, or cloud provider). This guide uses Docker with Postgres. Follow
these instructions step by step. Ask the user for clarification when the
environment is ambiguous — do not guess silently.

---

## Goal State

When setup is complete, the user should have:

1. A Clawvisor Docker image built (or pulled)
2. Clawvisor running with Postgres on their server or container platform
3. HTTPS configured (reverse proxy or platform-managed)
4. A user account created
5. An agent token created
6. Security checklist reviewed

---

## Step 1: Locate the Clawvisor repository

The Dockerfile and compose template are in the repository.

Check if the current directory is the Clawvisor repository:

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

## Step 2: Build the Docker image

```bash
cd "$CLAWVISOR_REPO" && docker build -f deploy/Dockerfile -t clawvisor .
```

This runs a multi-stage build: Node (frontend) → Go (binary) → distroless
runtime. The final image is minimal and runs as a static binary.

If the user wants to push to a registry, ask for the registry URL:

```bash
docker tag clawvisor <REGISTRY>/clawvisor:latest
docker push <REGISTRY>/clawvisor:latest
```

---

## Step 3: Choose deployment method

Ask the user: **"How would you like to deploy — Docker Compose on a server, or
a container platform (Cloud Run, Fly.io, Railway, Render)?"**

### Option A: Docker Compose

Use `deploy/docker-compose.yml` as a starting point. It bundles Postgres and
Clawvisor in a single stack.

#### Collect configuration

Ask the user for the following. Generate defaults where noted.

**Required:**

- **JWT secret** — generate one:
  ```bash
  openssl rand -hex 32
  ```
- **Public URL** — the HTTPS URL users will access (e.g.
  `https://clawvisor.example.com`). Ask the user.

**Intent verification** — Clawvisor uses a lightweight LLM to verify that
agent requests match their approved task scope. This is a core safety feature.
Ask which model the user wants to use and for their API key:

| Option | Provider | Endpoint | Model |
|--------|----------|----------|-------|
| Claude Haiku | `anthropic` | `https://api.anthropic.com/v1` | `claude-haiku-4-5-20251001` |
| Gemini Flash | `openai` | `https://generativelanguage.googleapis.com/v1beta/openai` | `gemini-2.0-flash` |
| GPT-4o Mini | `openai` | `https://api.openai.com/v1` | `gpt-4o-mini` |

Save as `CLAWVISOR_LLM_VERIFICATION_ENABLED=true`,
`CLAWVISOR_LLM_VERIFICATION_PROVIDER`, `CLAWVISOR_LLM_VERIFICATION_ENDPOINT`,
`CLAWVISOR_LLM_VERIFICATION_MODEL`, and `CLAWVISOR_LLM_VERIFICATION_API_KEY`.

**Optional — ask the user:**

- **"Do you want to restrict who can register?"** — if yes, collect email
  addresses for `ALLOWED_EMAILS` (comma-separated).
- **"Do you want to connect Google services (Gmail, Calendar, Drive,
  Contacts)?"** — if yes, collect `GOOGLE_CLIENT_ID` and
  `GOOGLE_CLIENT_SECRET`. Point to:
  https://github.com/clawvisor/clawvisor/blob/main/docs/GOOGLE_OAUTH_SETUP.md

#### Write the .env file

```bash
cat > "$CLAWVISOR_REPO/.env.cloud" <<EOF
DATABASE_URL=postgres://clawvisor:clawvisor@postgres:5432/clawvisor
DATABASE_DRIVER=postgres
JWT_SECRET=<generated secret>
SERVER_HOST=0.0.0.0
AUTH_MODE=password
VAULT_BACKEND=local
CALLBACK_REQUIRE_HTTPS=true
LOG_FORMAT=json
PUBLIC_URL=<user's public URL>
CLAWVISOR_LLM_VERIFICATION_ENABLED=true
CLAWVISOR_LLM_VERIFICATION_PROVIDER=<from above>
CLAWVISOR_LLM_VERIFICATION_ENDPOINT=<from above>
CLAWVISOR_LLM_VERIFICATION_MODEL=<from above>
CLAWVISOR_LLM_VERIFICATION_API_KEY=<from above>
ALLOWED_EMAILS=<if provided, or remove this line>
GOOGLE_CLIENT_ID=<if provided, or remove this line>
GOOGLE_CLIENT_SECRET=<if provided, or remove this line>
EOF
```

#### Start the stack

```bash
cd "$CLAWVISOR_REPO" && docker compose -f deploy/docker-compose.yml --env-file .env.cloud up -d
```

Wait for it to become ready:

```bash
until curl -sf http://localhost:25297/ready >/dev/null 2>&1; do sleep 1; done
```

Postgres data and the vault key are persisted in Docker volumes
(`postgres_data` and `vault_key`).

### Option B: Container platform

Ask the user which platform they're using. The general steps are:

1. Push the image to the platform's container registry
2. Set environment variables (see the [reference table](#environment-variables)
   below)
3. Attach a managed Postgres instance and set `DATABASE_URL`
4. Expose port `25297`

**Google Cloud Run example:**

```bash
gcloud builds submit --tag gcr.io/YOUR_PROJECT/clawvisor
gcloud run deploy clawvisor \
  --image gcr.io/YOUR_PROJECT/clawvisor \
  --port 25297 \
  --set-env-vars "DATABASE_URL=...,JWT_SECRET=...,SERVER_HOST=0.0.0.0,VAULT_BACKEND=local,AUTH_MODE=password"
```

**Vault considerations:**
- For platforms with persistent volumes (Fly.io, Railway), mount a volume at
  `/vault` for the vault key file.
- For ephemeral platforms (Cloud Run), use GCP Secret Manager instead:
  `VAULT_BACKEND=gcp`, `GCP_PROJECT=<project-id>`.

---

## Step 4: HTTPS / TLS

Clawvisor does not terminate TLS itself. Ask the user how they handle HTTPS:

- **Reverse proxy** — nginx, Caddy, or Traefik in front of port 25297
- **Platform-managed** — Cloud Run, Fly.io, Railway, and Render handle this
  automatically
- **Load balancer** — cloud provider ALB/NLB with TLS termination

Ensure `PUBLIC_URL` is set to the HTTPS URL so Telegram notification links
and OAuth redirects work correctly.

---

## Step 5: Create the first user

In production mode (`AUTH_MODE=password`), the user needs to register an
account. Instruct them to open the dashboard at `$PUBLIC_URL` and register
with their email and password.

If `ALLOWED_EMAILS` was set in Step 3, only those emails can register.

Wait for the user to confirm they've registered before proceeding.

---

## Step 6: Create an agent token

Instruct the user to create an agent in the dashboard under the **Agents**
tab. The token is shown once on creation — they should copy and save it.

Wait for the user to confirm they have the token before proceeding.

---

## Step 7: Security checklist

Walk through these items with the user:

- [ ] **Strong JWT secret** — at least 32 random bytes (`openssl rand -hex
  32`). Never use `dev-secret` in production.
- [ ] **HTTPS everywhere** — TLS via reverse proxy or platform. Set
  `CALLBACK_REQUIRE_HTTPS=true`.
- [ ] **Restrict registration** — set `ALLOWED_EMAILS` to control who can
  create accounts.
- [ ] **Backup the vault key** — if using `VAULT_BACKEND=local`, the
  `vault.key` file is the master encryption key for all stored credentials.
  Losing it means losing access to all encrypted credentials.
- [ ] **Persistent volumes** — mount volumes for Postgres data and the vault
  key so they survive container restarts.
- [ ] **Agent isolation** — run agents in a separate environment (container,
  VM) without direct access to Clawvisor's database, config, or filesystem.

---

## Step 8: Summary

Present the user with:

```
Clawvisor Cloud Deployment Complete
─────────────────────────────────────
Dashboard:  <PUBLIC_URL>
Auth mode:  password
Agent:      my-agent
Database:   Postgres

To stop (Docker Compose):
  docker compose -f deploy/docker-compose.yml down
```

Remind the user to:
- Log into the dashboard at `<PUBLIC_URL>` with the credentials from Step 5
- Connect services under the **Services** tab:
  - Google (Gmail, Calendar, Drive, Contacts) — one OAuth connection covers
    all four
  - GitHub, Slack, Notion, Linear, Stripe, Twilio — activate with API
    keys/tokens
- Set up Telegram notifications for mobile approvals (optional)

---

## Environment variables

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `DATABASE_URL` | Yes | — | Postgres connection string |
| `DATABASE_DRIVER` | No | auto (`postgres` when `DATABASE_URL` is set) | `postgres` or `sqlite` |
| `JWT_SECRET` | Yes | — | HMAC signing key for JWTs (32+ bytes) |
| `SERVER_HOST` | Yes | `127.0.0.1` | Set to `0.0.0.0` for container deployments |
| `PORT` | No | `25297` | Server listen port |
| `PUBLIC_URL` | Recommended | — | Full public URL (e.g. `https://clawvisor.example.com`) |
| `AUTH_MODE` | Recommended | auto | `password` or `magic_link` |
| `VAULT_BACKEND` | No | `local` | `local` (AES-256-GCM file) or `gcp` (Secret Manager) |
| `VAULT_KEY_FILE` | No | `./vault.key` | Path to local vault encryption key |
| `GCP_PROJECT` | If `gcp` vault | — | GCP project ID for Secret Manager |
| `CALLBACK_REQUIRE_HTTPS` | Recommended | `false` | Reject `http://` callback URLs (except localhost) |
| `ALLOWED_EMAILS` | No | — | Comma-separated emails allowed to register |
| `GOOGLE_CLIENT_ID` | No | — | Google OAuth client ID |
| `GOOGLE_CLIENT_SECRET` | No | — | Google OAuth client secret |
| `LOG_FORMAT` | No | auto (`json` in prod) | `json` or `text` |
| `LOG_LEVEL` | No | `info` | `debug`, `info`, `warn`, `error` |
