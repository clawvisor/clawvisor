#!/bin/sh
# Clawvisor Network Proxy launcher, installed by `clawvisor proxy install`.
# Invoked by clawvisor-local; $CLAWVISOR_SOCKET is set by the daemon so the
# proxy's admin endpoint answers /health on that socket (used by the
# daemon's health-probe). Real proxy traffic flows over TCP on 127.0.0.1.
set -eu

SERVICE_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN="$SERVICE_DIR/clawvisor-proxy"
CONFIG="$SERVICE_DIR/config.yaml"
DATA_DIR="{{DATA_DIR}}"

mkdir -p "$DATA_DIR"

exec "$BIN" serve \
  --mode=observe \
  --host=127.0.0.1 \
  --port={{LISTEN_PORT}} \
  --data-dir="$DATA_DIR" \
  --clawvisor-config="$CONFIG"
