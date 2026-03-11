#!/usr/bin/env bash
# Generate secrets for a fresh Clawvisor deployment.
# Usage: ./deploy/init.sh [.env-file]
set -euo pipefail

ENV_FILE="${1:-.env}"

if [ -f "$ENV_FILE" ]; then
    echo "Error: $ENV_FILE already exists. Remove it first or specify a different path." >&2
    exit 1
fi

JWT_SECRET=$(openssl rand -hex 32)
VAULT_KEY=$(openssl rand -base64 32)

cat > "$ENV_FILE" <<EOF
JWT_SECRET=$JWT_SECRET
VAULT_KEY=$VAULT_KEY
EOF
chmod 600 "$ENV_FILE"

echo "Secrets written to $ENV_FILE"
