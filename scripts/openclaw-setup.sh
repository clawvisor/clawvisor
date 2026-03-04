#!/usr/bin/env bash
set -euo pipefail

COMPOSE_FILE="docker-compose.openclaw.yml"
OPENCLAW_DIR="${OPENCLAW_DIR:-$HOME/.openclaw}"

# 1. Start Clawvisor + Postgres
echo "Starting Clawvisor..."
docker compose -f "$COMPOSE_FILE" up -d --build

# 2. Wait for ready
echo "Waiting for Clawvisor to be ready..."
until curl -sf http://localhost:8080/ready >/dev/null 2>&1; do sleep 1; done
echo "Clawvisor is ready."

# 3. Create agent via CLI inside container (--replace makes this idempotent)
APP_CONTAINER=$(docker compose -f "$COMPOSE_FILE" ps -q app)
CREDS=$(docker exec "$APP_CONTAINER" /clawvisor agent create openclaw \
  --with-callback-secret --replace --json)
AGENT_TOKEN=$(echo "$CREDS" | jq -r .token)
CALLBACK_SECRET=$(echo "$CREDS" | jq -r .callback_secret)

# 4. Install skill into OpenClaw (if running)
OPENCLAW_CONTAINER=$(docker ps --filter name=openclaw-gateway --format '{{.Names}}' | head -1)
if [ -n "$OPENCLAW_CONTAINER" ]; then
  echo "Installing clawvisor skill..."
  docker exec "$OPENCLAW_CONTAINER" npx clawhub install clawvisor --force \
    --workdir /home/node/.openclaw/workspace 2>/dev/null || true
fi

# 5. Install webhook extension (via host mount)
if [ -d "$OPENCLAW_DIR" ]; then
  EXT_SRC="extensions/clawvisor-webhook"
  EXT_DST="$OPENCLAW_DIR/extensions/clawvisor-webhook"
  mkdir -p "$EXT_DST"
  cp "$EXT_SRC/openclaw.plugin.json" "$EXT_SRC/index.ts" \
     "$EXT_SRC/package.json" "$EXT_DST/"
  cp "$EXT_SRC/tsconfig.json" "$EXT_DST/" 2>/dev/null || true
  (cd "$EXT_DST" && npm install --production 2>/dev/null) || true

  # 6. Configure webhook plugin in openclaw.json
  CONFIG="$OPENCLAW_DIR/openclaw.json"
  if [ -f "$CONFIG" ]; then
    if jq --arg secret "$CALLBACK_SECRET" \
         '.plugins.entries["clawvisor-webhook"] = {
            "enabled": true, "secret": $secret,
            "path": "/clawvisor/callback"
          }' "$CONFIG" > "${CONFIG}.tmp" 2>/dev/null; then
      mv "${CONFIG}.tmp" "$CONFIG"
      echo "Webhook plugin configured in openclaw.json"
    else
      rm -f "${CONFIG}.tmp"
      echo "Warning: could not update openclaw.json — configure the webhook plugin manually."
      echo "  Add clawvisor-webhook plugin with secret: $CALLBACK_SECRET"
    fi
  fi

  # 7. Write Clawvisor env vars to ~/.openclaw/.env
  # Uses non-overriding semantics — existing env vars from the shell take
  # precedence, so this won't clobber user overrides.
  ENV_FILE="$OPENCLAW_DIR/.env"
  # Strip any previous Clawvisor lines, then append fresh values.
  if [ -f "$ENV_FILE" ]; then
    grep -v '^CLAWVISOR_\|^OPENCLAW_HOOKS_URL=' "$ENV_FILE" > "${ENV_FILE}.tmp" 2>/dev/null || true
    mv "${ENV_FILE}.tmp" "$ENV_FILE"
  fi
  cat >> "$ENV_FILE" <<ENVBLOCK
CLAWVISOR_URL=http://host.docker.internal:8080
CLAWVISOR_AGENT_TOKEN=$AGENT_TOKEN
OPENCLAW_HOOKS_URL=http://host.docker.internal:18789
ENVBLOCK
  chmod 600 "$ENV_FILE"
  echo "Environment variables written to $ENV_FILE"
fi

# 8. Extract magic link from container logs
MAGIC_URL=$(docker compose -f "$COMPOSE_FILE" logs app 2>&1 \
  | grep -o 'http://[^ ]*magic-link?token=[^ ]*' | tail -1)

# 9. Summary
cat <<EOF

  Clawvisor + OpenClaw Setup Complete
  ────────────────────────────────────
  Dashboard:  http://localhost:8080
  Agent:      openclaw
  Token:      $AGENT_TOKEN

EOF

if [ -n "$MAGIC_URL" ]; then
cat <<EOF
  Sign in:    $MAGIC_URL

EOF
fi

cat <<EOF
  To stop:    make openclaw-down
EOF
