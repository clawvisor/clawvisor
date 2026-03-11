# Clawvisor — Cloud Deployment Guide

You are deploying Clawvisor to the user's own infrastructure — a VPS,
container platform, or cloud provider. This is a remote deployment: the
instance will be accessible over the internet at a public URL with HTTPS and
password-based auth. Follow these instructions step by step. Ask the user
for clarification when the environment is ambiguous — do not guess silently.

If the user just wants to run Clawvisor on their local machine, redirect
them to [SETUP_LOCAL.md](SETUP_LOCAL.md) or [SETUP_DOCKER.md](SETUP_DOCKER.md)
instead.

---

## Goal State

When setup is complete, the user should have:

1. Clawvisor running on their remote infrastructure with Postgres
2. HTTPS configured (reverse proxy or platform-managed)
3. A user account created via password registration
4. The deployment verified and reachable at their public URL
5. Security checklist reviewed

---

## Step 1: Determine the target platform

Ask the user: **"Where are you deploying Clawvisor?"**

- **VPS / dedicated server** — they have SSH access to a Linux server with
  Docker installed (e.g. DigitalOcean, Hetzner, Linode, EC2)
- **Container platform** — a managed service that runs containers (e.g.
  Google Cloud Run, Fly.io, Railway, Render)

Store the answer — the deployment steps differ.

---

## Step 2: Collect configuration

Ask the user for the following. Generate defaults where noted.

**Required:**

- **Public URL** — the HTTPS URL where Clawvisor will be accessible (e.g.
  `https://clawvisor.example.com`). Ask the user. Store as `$PUBLIC_URL`.
  If the user doesn't have a domain, they can use an SSH tunnel instead —
  see Step 5.
- **JWT secret** and **Vault key** — generate both:
  ```bash
  openssl rand -hex 32     # JWT_SECRET
  openssl rand -base64 32  # VAULT_KEY
  ```

**Intent verification** — Clawvisor uses a lightweight LLM to verify that
agent requests match their approved task scope. This is a core safety feature.
Ask which model the user wants to use and for their API key:

| Option | Provider | Endpoint | Model |
|--------|----------|----------|-------|
| Claude Haiku | `anthropic` | `https://api.anthropic.com/v1` | `claude-haiku-4-5-20251001` |
| Gemini Flash | `openai` | `https://generativelanguage.googleapis.com/v1beta/openai` | `gemini-2.0-flash` |
| GPT-4o Mini | `openai` | `https://api.openai.com/v1` | `gpt-4o-mini` |

**Optional — ask the user:**

- **"Do you want to restrict who can register?"** — if yes, collect email
  addresses for `ALLOWED_EMAILS` (comma-separated).
- **"Do you want to connect Google services (Gmail, Calendar, Drive,
  Contacts)?"** — if yes, collect `GOOGLE_CLIENT_ID` and
  `GOOGLE_CLIENT_SECRET`. Point to:
  https://github.com/clawvisor/clawvisor/blob/main/docs/GOOGLE_OAUTH_SETUP.md
  The OAuth redirect URL will be `$PUBLIC_URL/api/oauth/callback`.

---

## Step 3: Build the Docker image

```bash
cd "$CLAWVISOR_REPO" && docker build -f deploy/Dockerfile -t clawvisor .
```

This runs a multi-stage build: Node (frontend) → Go (binary) → distroless
runtime. The final image is minimal and runs as a static binary.

**Push to a registry** — the image needs to be accessible from the target
platform. Ask the user for their registry:

```bash
docker tag clawvisor <REGISTRY>/clawvisor:latest
docker push <REGISTRY>/clawvisor:latest
```

For **VPS deployments** where the user will build on the server itself, they
can skip the push and clone + build directly on the server instead.

---

## Step 4: Deploy

### Option A: VPS / dedicated server

The user needs to run the following on their server. You cannot SSH into the
server directly — present these as instructions for the user to execute on
their remote machine, or help them construct an SSH command.

#### Environment file

Generate the `.env.cloud` contents for the user to place on their server.

**If the user has an HTTPS domain:**

```
DATABASE_URL=postgres://clawvisor:clawvisor@postgres:5432/clawvisor
DATABASE_DRIVER=postgres
JWT_SECRET=<generated secret from Step 2>
VAULT_KEY=<generated key from Step 2>
SERVER_HOST=0.0.0.0
AUTH_MODE=password
VAULT_BACKEND=local
CALLBACK_REQUIRE_HTTPS=true
LOG_FORMAT=json
PUBLIC_URL=<PUBLIC_URL from Step 2>
CLAWVISOR_LLM_VERIFICATION_ENABLED=true
CLAWVISOR_LLM_VERIFICATION_PROVIDER=<from Step 2>
CLAWVISOR_LLM_VERIFICATION_ENDPOINT=<from Step 2>
CLAWVISOR_LLM_VERIFICATION_MODEL=<from Step 2>
CLAWVISOR_LLM_VERIFICATION_API_KEY=<from Step 2>
ALLOWED_EMAILS=<if provided, or remove this line>
GOOGLE_CLIENT_ID=<if provided, or remove this line>
GOOGLE_CLIENT_SECRET=<if provided, or remove this line>
```

**If the user will access via SSH tunnel** (no domain):

```
DATABASE_URL=postgres://clawvisor:clawvisor@postgres:5432/clawvisor
DATABASE_DRIVER=postgres
JWT_SECRET=<generated secret from Step 2>
VAULT_KEY=<generated key from Step 2>
SERVER_HOST=0.0.0.0
VAULT_BACKEND=local
LOG_FORMAT=json
CLAWVISOR_LLM_VERIFICATION_ENABLED=true
CLAWVISOR_LLM_VERIFICATION_PROVIDER=<from Step 2>
CLAWVISOR_LLM_VERIFICATION_ENDPOINT=<from Step 2>
CLAWVISOR_LLM_VERIFICATION_MODEL=<from Step 2>
CLAWVISOR_LLM_VERIFICATION_API_KEY=<from Step 2>
GOOGLE_CLIENT_ID=<if provided, or remove this line>
GOOGLE_CLIENT_SECRET=<if provided, or remove this line>
```

Note: `AUTH_MODE`, `PUBLIC_URL`, `CALLBACK_REQUIRE_HTTPS`, and
`ALLOWED_EMAILS` are omitted — the defaults (magic link, no public URL,
allow HTTP callbacks) are correct for SSH tunnel access.

#### Start the stack

Instruct the user to run on their server:

```bash
docker compose -f deploy/docker-compose.yml --env-file .env.cloud up -d
```

If they built locally and pushed to a registry, they should update the
`image` field in the compose file (or use an override) to pull from their
registry instead of building on the server.

#### Verify on the server

```bash
curl -sf http://localhost:25297/ready && echo "RUNNING" || echo "NOT RUNNING"
```

Postgres data is persisted in the `postgres_data` Docker volume. The vault
master key is provided via the `VAULT_KEY` environment variable.

### Option B: Container platform

Ask the user which platform they're using. The general steps are:

1. Push the image to the platform's container registry
2. Set environment variables (see the [reference table](#environment-variables)
   below) — all the values from Step 2
3. Attach a managed Postgres instance and set `DATABASE_URL`
4. Expose port `25297`

Key env vars for all platforms: `DATABASE_URL`, `DATABASE_DRIVER=postgres`,
`JWT_SECRET`, `SERVER_HOST=0.0.0.0`, `AUTH_MODE=password`,
`CALLBACK_REQUIRE_HTTPS=true`, `PUBLIC_URL`, `LOG_FORMAT=json`.

**Google Cloud Run example:**

```bash
gcloud builds submit --tag gcr.io/YOUR_PROJECT/clawvisor
gcloud run deploy clawvisor \
  --image gcr.io/YOUR_PROJECT/clawvisor \
  --port 25297 \
  --set-env-vars "DATABASE_URL=...,JWT_SECRET=...,SERVER_HOST=0.0.0.0,AUTH_MODE=password,PUBLIC_URL=..."
```

**Vault key:** Set `VAULT_KEY` as an environment variable (base64-encoded
32-byte key). This works on all platforms — no volume mounts needed.

---

## Step 5: HTTPS / TLS

Clawvisor does not terminate TLS itself. Ask the user: **"How will you
access the Clawvisor dashboard?"**

### Option A: HTTPS with a domain

The user has a domain name and can set up TLS. Ask how:

- **Reverse proxy** — nginx, Caddy, or Traefik in front of port 25297
- **Platform-managed** — Cloud Run, Fly.io, Railway, and Render handle this
  automatically
- **Load balancer** — cloud provider ALB/NLB with TLS termination

Ensure `PUBLIC_URL` is set to the HTTPS URL so Telegram notification links
and OAuth redirects work correctly.

### Option B: SSH tunnel (no domain required)

If the user doesn't have an HTTPS hostname available, they can access the
remote instance through an SSH tunnel. This forwards the server's port to
their local machine, so the dashboard is available at `localhost` — no
domain, no TLS, no public exposure.

Instruct the user to run on their local machine:

```bash
ssh -L 25297:localhost:25297 user@<server-ip>
```

While the tunnel is open, Clawvisor is accessible at
`http://localhost:25297`.

When using an SSH tunnel, adjust the environment on the server:

- Set `AUTH_MODE=magic_link` (or omit — it defaults to magic link when
  accessed via localhost)
- Remove `PUBLIC_URL` (not needed — the instance isn't publicly exposed)
- Set `CALLBACK_REQUIRE_HTTPS=false` (callbacks go through localhost)

This is a good option for getting started quickly or for single-user setups
where the user always accesses the server via SSH.

---

## Step 6: Verify the deployment

Confirm Clawvisor is reachable:

- **HTTPS deployment:** `curl -sf "$PUBLIC_URL/ready"`
- **SSH tunnel:** `curl -sf http://localhost:25297/ready` (with tunnel open)

If this fails, troubleshoot:
- Is the container running? (check platform logs or `docker ps` on the server)
- Is port 25297 exposed and reachable?
- Is HTTPS / reverse proxy configured correctly?
- Is DNS pointing to the right IP?
- Is the SSH tunnel open? (for tunnel deployments)

Do not proceed until the deployment responds.

---

## Step 7: Create the first user

**If using `AUTH_MODE=password`** (HTTPS deployments): instruct the user to
open the dashboard at `$PUBLIC_URL` and register with their email and
password. If `ALLOWED_EMAILS` was set in Step 2, only those emails can
register.

**If using `AUTH_MODE=magic_link`** (SSH tunnel): the magic link is printed
to the container logs. Retrieve it:

```bash
docker compose -f deploy/docker-compose.yml logs app 2>&1 \
  | grep -o 'http://[^ ]*magic-link?token=[^ ]*' | tail -1
```

Instruct the user to run this on the server (or via SSH), then open the
magic link in their local browser (it will resolve through the tunnel).

Wait for the user to confirm they've signed in before proceeding.

---

## Step 8: Security checklist

Walk through these items with the user:

- [ ] **Strong JWT secret** — at least 32 random bytes (`openssl rand -hex
  32`). Never use `dev-secret` in production.
- [ ] **HTTPS everywhere** — TLS via reverse proxy or platform. Set
  `CALLBACK_REQUIRE_HTTPS=true`.
- [ ] **Restrict registration** — set `ALLOWED_EMAILS` to control who can
  create accounts.
- [ ] **Backup the vault key** — `VAULT_KEY` is the master encryption key
  for all stored credentials. Store it securely (e.g. a secrets manager).
  Losing it means losing access to all encrypted credentials.
- [ ] **Persistent volumes** — mount a volume for Postgres data so it
  survives container restarts.
- [ ] **Agent isolation** — run agents in a separate environment (container,
  VM) without direct access to Clawvisor's database, config, or filesystem.

---

## Step 9: Summary

**HTTPS deployment:**

```
Clawvisor Cloud Deployment Complete
─────────────────────────────────────
Dashboard:  <PUBLIC_URL>
Auth mode:  password
Database:   Postgres
```

**SSH tunnel deployment:**

```
Clawvisor Cloud Deployment Complete
─────────────────────────────────────
Dashboard:  http://localhost:25297 (via SSH tunnel)
Auth mode:  magic_link
Database:   Postgres

To connect:  ssh -L 25297:localhost:25297 user@<server-ip>
```

Remind the user to:
- Connect services under the **Services** tab:
  - Google (Gmail, Calendar, Drive, Contacts) — one OAuth connection covers
    all four
  - GitHub, Slack, Notion, Linear, Stripe, Twilio — activate with API
    keys/tokens
- Set up Telegram notifications for mobile approvals (optional)

**Next:** Connect your agent — see [SETUP.md](SETUP.md#2-connect-your-agent)
for integration guides.

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
| `VAULT_BACKEND` | No | `local` | `local` (AES-256-GCM) or `gcp` (Secret Manager) |
| `VAULT_KEY` | Yes | — | Base64-encoded 32-byte vault master key |
| `VAULT_KEY_FILE` | No | `./vault.key` | Path to vault key file (alternative to `VAULT_KEY`) |
| `GCP_PROJECT` | If `gcp` vault | — | GCP project ID for Secret Manager |
| `CALLBACK_REQUIRE_HTTPS` | Recommended | `false` | Reject `http://` callback URLs (except localhost) |
| `ALLOWED_EMAILS` | No | — | Comma-separated emails allowed to register |
| `GOOGLE_CLIENT_ID` | No | — | Google OAuth client ID |
| `GOOGLE_CLIENT_SECRET` | No | — | Google OAuth client secret |
| `LOG_FORMAT` | No | auto (`json` in prod) | `json` or `text` |
| `LOG_LEVEL` | No | `info` | `debug`, `info`, `warn`, `error` |
