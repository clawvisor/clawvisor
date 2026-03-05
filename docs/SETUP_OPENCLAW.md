# Clawvisor — Sibling Setup Guide

You are setting up Clawvisor alongside an OpenClaw instance on the user's
local machine. Follow these instructions step by step. Ask the user for
clarification when the environment is ambiguous — do not guess silently.

---

## Goal State

When setup is complete, the user should have:

1. Clawvisor running (via Docker or locally) with a working dashboard
2. An agent token registered in Clawvisor
3. The `clawvisor` skill installed in their OpenClaw workspace
4. The `clawvisor-webhook` extension installed and configured
5. Environment variables (`CLAWVISOR_URL`, `CLAWVISOR_AGENT_TOKEN`,
   `OPENCLAW_HOOKS_URL`) set in `~/.openclaw/workspace/.env`
6. A magic link URL they can use to sign into the dashboard

---

## Step 1: Locate the Clawvisor repository

The Clawvisor source is needed for `docker-compose.openclaw.yml` and the
webhook extension files.

First, check if the current working directory is already the Clawvisor
repository:

```bash
ls docker-compose.openclaw.yml extensions/clawvisor-webhook/ 2>/dev/null
```

If both exist, the current directory is the repo — use it as
`$CLAWVISOR_REPO`.

If not, search common locations:

```bash
ls -d ~/code/clawvisor 2>/dev/null \
  || ls -d ~/clawvisor 2>/dev/null \
  || ls -d ~/projects/clawvisor 2>/dev/null
```

Also check for worktree or alternate naming (e.g. `clawvisor-public`,
`clawvisor-oss`). The key indicator is a directory containing both
`docker-compose.openclaw.yml` and `extensions/clawvisor-webhook/`.

If not found, ask the user where the Clawvisor repository is checked out.
Store the path as `$CLAWVISOR_REPO` for subsequent steps.

Verify the repo has the docker-compose file:

```bash
ls "$CLAWVISOR_REPO/docker-compose.openclaw.yml"
```

---

## Step 2: Configure services

Before starting Clawvisor, ask the user what they want to set up. These are
passed as environment variables to the Docker container.

### Intent Verification (LLM)

Clawvisor uses a lightweight LLM to verify that agent requests match their
approved task scope — this is a core safety feature. Ask which model they
want to use:

| Option | Provider | Endpoint | Model |
|--------|----------|----------|-------|
| Claude Haiku | `anthropic` | `https://api.anthropic.com/v1` | `claude-haiku-4-5-20251001` |
| Gemini Flash | `openai` | `https://generativelanguage.googleapis.com/v1beta/openai` | `gemini-2.0-flash` |
| GPT-4o Mini | `openai` | `https://api.openai.com/v1` | `gpt-4o-mini` |

Then ask for their **API key** for the chosen provider.

Save as `CLAWVISOR_LLM_VERIFICATION_ENABLED=true`,
`CLAWVISOR_LLM_VERIFICATION_PROVIDER`, `CLAWVISOR_LLM_VERIFICATION_ENDPOINT`,
`CLAWVISOR_LLM_VERIFICATION_MODEL`, and `CLAWVISOR_LLM_VERIFICATION_API_KEY`.

### Google Services (Gmail, Calendar, Drive, Contacts)

Ask: **"Do you want to connect Google services (Gmail, Calendar, Drive,
Contacts)? This requires a Google Cloud OAuth 2.0 client."**

If yes, they need:
- **Google Client ID** — from the Google Cloud Console
  (https://console.cloud.google.com/apis/credentials)
- **Google Client Secret** — from the same page

Point them to the setup guide if they haven't created OAuth credentials yet:
https://github.com/clawvisor/clawvisor/blob/main/docs/GOOGLE_OAUTH_SETUP.md

The OAuth redirect URL will be `http://localhost:25297/api/oauth/callback`
(Clawvisor sets this automatically based on its host/port).

Save as `GOOGLE_CLIENT_ID` and `GOOGLE_CLIENT_SECRET`.

---

## Step 3: Start Clawvisor

Check if Clawvisor is already running:

```bash
curl -sf http://localhost:25297/ready 2>/dev/null && echo "RUNNING" || echo "NOT RUNNING"
```

If already running, skip to Step 4.

If not running, start it with Docker Compose. Pass any env vars collected in
Step 2 by writing a `.env` file next to the compose file, or by exporting
them before running `docker compose`:

```bash
cd "$CLAWVISOR_REPO"

# Write env vars for docker compose (idempotent)
cat > .env.openclaw <<EOF
GOOGLE_CLIENT_ID=<from Step 2, or empty>
GOOGLE_CLIENT_SECRET=<from Step 2, or empty>
CLAWVISOR_LLM_VERIFICATION_ENABLED=<true or false>
CLAWVISOR_LLM_VERIFICATION_PROVIDER=<from Step 2, or empty>
CLAWVISOR_LLM_VERIFICATION_ENDPOINT=<from Step 2, or empty>
CLAWVISOR_LLM_VERIFICATION_MODEL=<from Step 2, or empty>
CLAWVISOR_LLM_VERIFICATION_API_KEY=<from Step 2, or empty>
EOF

docker compose -f docker-compose.openclaw.yml --env-file .env.openclaw up -d --build
```

Wait for it to become ready:

```bash
until curl -sf http://localhost:25297/ready >/dev/null 2>&1; do sleep 1; done
```

This typically takes 15–30 seconds on first build.

If Clawvisor was already running and the user wants to add/change Google or
LLM config, update `.env.openclaw` and restart:

```bash
docker compose -f docker-compose.openclaw.yml --env-file .env.openclaw up -d
```

---

## Step 4: Create an agent

Find the app container:

```bash
docker compose -f "$CLAWVISOR_REPO/docker-compose.openclaw.yml" ps -q app
```

Create the agent inside the container. Use `--replace` so this is safe to
re-run:

```bash
docker exec <APP_CONTAINER> /clawvisor agent create openclaw \
  --with-callback-secret --replace --json
```

This returns JSON with `token` and `callback_secret` fields. Save both values
— you'll need them in later steps.

---

## Step 5: Find the OpenClaw instance

Look for a running OpenClaw container. It could be named anything — look for
containers running the OpenClaw gateway image:

```bash
docker ps --format '{{.Names}}\t{{.Image}}' | grep -i openclaw
```

If multiple containers match, ask the user which one is their OpenClaw
gateway. If none match, check if OpenClaw is running directly on the host
(not in Docker) — the `~/.openclaw/` directory existing is a strong signal.

Determine the OpenClaw data directory:

```bash
OPENCLAW_DIR="${OPENCLAW_DIR:-$HOME/.openclaw}"
ls "$OPENCLAW_DIR/openclaw.json" 2>/dev/null
```

If `~/.openclaw` doesn't exist, ask the user where their OpenClaw data
directory is.

---

## Step 6: Install the clawvisor skill

If you found an OpenClaw container in Step 5:

```bash
docker exec <OPENCLAW_CONTAINER> npx clawhub install clawvisor --force \
  --workdir /home/node/.openclaw/workspace
```

If OpenClaw is running on the host (not in Docker):

```bash
npx clawhub install clawvisor --force
```

Verify the skill is installed:

```bash
ls "$OPENCLAW_DIR/workspace/skills/clawvisor/SKILL.md" 2>/dev/null
```

---

## Step 7: Install the webhook extension

Copy the extension files from the Clawvisor repo into OpenClaw's extensions
directory:

```bash
EXT_SRC="$CLAWVISOR_REPO/extensions/clawvisor-webhook"
EXT_DST="$OPENCLAW_DIR/extensions/clawvisor-webhook"
mkdir -p "$EXT_DST"
cp "$EXT_SRC/openclaw.plugin.json" "$EXT_SRC/index.ts" "$EXT_SRC/package.json" "$EXT_DST/"
cp "$EXT_SRC/tsconfig.json" "$EXT_DST/" 2>/dev/null || true
cd "$EXT_DST" && npm install --production
```

---

## Step 8: Enable the webhook plugin in openclaw.json

Read `$OPENCLAW_DIR/openclaw.json` and add (or update) the webhook plugin
entry. The plugin reads its signing secret from the `CLAWVISOR_CALLBACK_SECRET`
environment variable (set in Step 9 via `~/.openclaw/workspace/.env`), and uses
sensible defaults for `path` (`/clawvisor/callback`) and `gatewayWsUrl`
(`ws://127.0.0.1:18789`), so typically only `enabled` is needed.

Check the gateway config in `openclaw.json` — if the gateway port or bind
address differs from the default (e.g. `"port": 19000` or
`"bind": "0.0.0.0"`), set `gatewayWsUrl` accordingly.

The target structure under the `plugins` key:

```json
{
  "plugins": {
    "entries": {
      "clawvisor-webhook": {
        "enabled": true
      }
    }
  }
}
```

Or if the gateway uses a non-default port/bind:

```json
{
  "plugins": {
    "entries": {
      "clawvisor-webhook": {
        "enabled": true,
        "config": {
          "gatewayWsUrl": "ws://127.0.0.1:<gateway port>"
        }
      }
    }
  }
}
```

Note: `enabled` is a top-level plugin lifecycle flag. Plugin-specific
settings like `gatewayWsUrl` go inside a nested `config` object — OpenClaw
passes only the `config` contents to `api.pluginConfig`.

**Important:** Read the file first and merge — do not overwrite the entire
file. Preserve all existing keys. If `plugins` or `plugins.entries` doesn't
exist yet, create them.

Use `jq` if the file is valid JSON:

```bash
jq '.plugins.entries["clawvisor-webhook"] = {"enabled": true}' \
  "$OPENCLAW_DIR/openclaw.json" > /tmp/openclaw.json.tmp \
  && mv /tmp/openclaw.json.tmp "$OPENCLAW_DIR/openclaw.json"
```

If `jq` fails (the file may use JSON5 syntax), read the file, parse it,
merge the plugin config, and write it back.

---

## Step 9: Write environment variables

Determine the correct Clawvisor URL for the OpenClaw process to reach:

- If **both** run in Docker on the same host: use `http://host.docker.internal:25297`
- If OpenClaw runs on the **host** and Clawvisor in Docker: use `http://localhost:25297`
- If both run on the **host**: use `http://localhost:25297`

Similarly for `OPENCLAW_HOOKS_URL`:

- If both in Docker: `http://host.docker.internal:18789`
- If OpenClaw on host: `http://localhost:18789`

Write the variables to `~/.openclaw/workspace/.env`. This file uses non-overriding
semantics — existing shell env vars take precedence, so it won't clobber
user overrides.

First, strip any previous Clawvisor-related lines to make this idempotent:

```bash
grep -v '^CLAWVISOR_\|^OPENCLAW_HOOKS_URL=' "$OPENCLAW_DIR/workspace/.env" > /tmp/openclaw-env.tmp 2>/dev/null || true
mv /tmp/openclaw-env.tmp "$OPENCLAW_DIR/workspace/.env" 2>/dev/null || true
```

Then append the new values (the `callback_secret` comes from Step 4):

```bash
cat >> "$OPENCLAW_DIR/workspace/.env" <<EOF
CLAWVISOR_URL=<determined URL>
CLAWVISOR_AGENT_TOKEN=<token from Step 4>
CLAWVISOR_CALLBACK_SECRET=<callback_secret from Step 4>
OPENCLAW_HOOKS_URL=<determined URL>
EOF
chmod 600 "$OPENCLAW_DIR/workspace/.env"
```

---

## Step 10: Extract the magic link

Get the dashboard sign-in URL from the Clawvisor container logs:

```bash
docker compose -f "$CLAWVISOR_REPO/docker-compose.openclaw.yml" logs app 2>&1 \
  | grep -o 'http://[^ ]*magic-link?token=[^ ]*' | tail -1
```

---

## Step 11: Summary

Present the user with:

```
Clawvisor + OpenClaw Setup Complete
────────────────────────────────────
Dashboard:  http://localhost:25297
Sign in:    <magic link URL from Step 10>
Agent:      openclaw

To stop Clawvisor:
  docker compose -f <CLAWVISOR_REPO>/docker-compose.openclaw.yml down
```

Remind the user to:
- Open the magic link to access the dashboard
- If Google services were configured in Step 2, connect their Google
  account under the Services tab (one OAuth connection covers Gmail,
  Calendar, Drive, and Contacts)
- For non-OAuth services (GitHub, Slack, Notion, etc.), activate them
  under the Services tab using API keys
- Restart OpenClaw if it was already running so it picks up the new
  `.env` values and webhook plugin
