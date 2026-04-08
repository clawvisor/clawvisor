# Clawvisor — OpenClaw Integration Guide

You are connecting an OpenClaw instance to a running Clawvisor server. This
guide covers agent creation, skill installation, webhook extension setup, and
environment configuration. Follow these instructions step by step. Ask the
user for clarification when the environment is ambiguous — do not guess
silently.

**Prerequisite:** Clawvisor must be running. If it isn't, set it up first —
see [SETUP.md](SETUP.md).

---

## Goal State

When setup is complete, the user should have:

1. An agent token registered in Clawvisor (with callback secret)
2. The `clawvisor` skill installed in their OpenClaw workspace
3. The `clawvisor-webhook` extension installed and configured
4. Environment variables (`CLAWVISOR_URL`, `CLAWVISOR_AGENT_TOKEN`,
   `OPENCLAW_HOOKS_URL`) set in `~/.openclaw/workspace/.env`

---

## Step 1: Verify Clawvisor is running

```bash
curl -sf http://localhost:25297/ready 2>/dev/null && echo "RUNNING" || echo "NOT RUNNING"
```

If not running, the user needs to set it up first — see
[SETUP.md](SETUP.md).

Store the URL as `$CLAWVISOR_URL` (default `http://localhost:25297`).

---

## Step 2: Create an agent

```bash
clawvisor agent create openclaw --with-callback-secret --replace --json
```

If running in Docker instead:

```bash
docker exec <APP_CONTAINER> /clawvisor agent create openclaw \
  --with-callback-secret --replace --json
```

This returns JSON with `token` and `callback_secret` fields. Save both values
— you'll need them in later steps.

---

## Step 3: Find the OpenClaw instance

Look for a running OpenClaw container:

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

## Step 4: Install the clawvisor skill

If you found an OpenClaw container in Step 3:

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

## Step 5: Install the webhook extension

Install the webhook extension from npm:

```bash
EXT_DST="$OPENCLAW_DIR/extensions/clawvisor-webhook"
mkdir -p "$EXT_DST"
cd "$EXT_DST" && npm init -y && npm install clawvisor-webhook --production
```

If installing from the Clawvisor repository instead:

```bash
EXT_SRC="<clawvisor-repo>/extensions/clawvisor-webhook"
EXT_DST="$OPENCLAW_DIR/extensions/clawvisor-webhook"
mkdir -p "$EXT_DST"
cp "$EXT_SRC/openclaw.plugin.json" "$EXT_SRC/index.ts" "$EXT_SRC/package.json" "$EXT_DST/"
cp "$EXT_SRC/tsconfig.json" "$EXT_DST/" 2>/dev/null || true
cd "$EXT_DST" && npm install --production
```

---

## Step 6: Enable the webhook plugin in openclaw.json

Read `$OPENCLAW_DIR/openclaw.json` and add (or update) the webhook plugin
entry. The plugin reads its signing secret from the `CLAWVISOR_CALLBACK_SECRET`
environment variable (set in Step 7 via `~/.openclaw/workspace/.env`), and uses
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

## Step 7: Write environment variables

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

Then append the new values (from Step 2):

```bash
cat >> "$OPENCLAW_DIR/workspace/.env" <<EOF
CLAWVISOR_URL=<determined URL>
CLAWVISOR_AGENT_TOKEN=<token from Step 2>
CLAWVISOR_CALLBACK_SECRET=<callback_secret from Step 2>
OPENCLAW_HOOKS_URL=<determined URL>
EOF
chmod 600 "$OPENCLAW_DIR/workspace/.env"
```

---

## Step 8: Summary

Present the user with:

```
Clawvisor + OpenClaw Integration Complete
──────────────────────────────────────────
Clawvisor:  <CLAWVISOR_URL>
Agent:      openclaw
Skill:      installed via clawhub
Webhook:    clawvisor-webhook plugin enabled
```

Remind the user to:
- If Google services were configured, connect their Google account under
  the Services tab (one OAuth connection covers Gmail, Calendar, Drive, and
  Contacts)
- For non-OAuth services (GitHub, Slack, Notion, etc.), activate them
  under the Services tab using API keys
- Restart OpenClaw if it was already running so it picks up the new
  `.env` values and webhook plugin
