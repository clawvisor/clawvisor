#!/usr/bin/env bash
set -euo pipefail

# Run the Clawvisor daemon and web UI locally with hot reload.
#
# - Backend: `air` rebuilds and restarts on .go changes (see .air.toml).
# - Frontend: Vite dev server with HMR; proxies API/WS calls to the backend.
# - Ports: Vite defaults to 25297 so it matches the registered OAuth
#   redirect_uri and shares localStorage/cookies with the installed daemon.
#   The backend listens on a random free port. Pass a port as the first arg
#   (or via $PORT) to override the Vite port.
# - Config: reuses ~/.clawvisor/config.yaml (PORT/SERVER_HOST/PUBLIC_URL
#   overridden via env so the daemon advertises the Vite origin).
#
# The Vite URL is the only one you need — open it in your browser.

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

if [[ ! -f "$REPO_ROOT/go.mod" ]]; then
    echo >&2 "Could not find go.mod — run this script from the repo."
    exit 1
fi

if ! command -v node >/dev/null 2>&1; then
    echo >&2 "node is required (used to pick free ports and run the Vite dev server)."
    exit 1
fi

# Ensure air is available for backend hot reload.
if ! command -v air >/dev/null 2>&1; then
    echo "  Installing air for Go hot reload..."
    GOBIN="${GOBIN:-$(go env GOPATH)/bin}"
    PATH="$GOBIN:$PATH"
    if ! command -v air >/dev/null 2>&1; then
        go install github.com/air-verse/air@latest
    fi
fi

find_free_port() {
    local exclude="${1:-}"
    while :; do
        local p
        p="$(node -e "const s=require('net').createServer();s.listen(0,'127.0.0.1',()=>{const port=s.address().port;s.close(()=>console.log(port));});")"
        if [[ "$p" != "$exclude" ]]; then
            echo "$p"
            return
        fi
    done
}

FRONTEND_PORT="${1:-${PORT:-25297}}"
BACKEND_PORT="$(find_free_port "$FRONTEND_PORT")"

# air's tmp_dir create is non-recursive, so make sure the parent exists.
mkdir -p "$REPO_ROOT/bin/.air"

# Stop the installed daemon (if any) to avoid SQLite lock contention on
# ~/.clawvisor/clawvisor.db. Detect via the daemon's PID file.
PID_FILE="$HOME/.clawvisor/.daemon.pid"
if [[ -f "$PID_FILE" ]] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
    echo "  Stopping installed daemon (PID $(cat "$PID_FILE"))..."
    if command -v clawvisor-server >/dev/null 2>&1; then
        clawvisor-server stop >/dev/null 2>&1 || true
    else
        kill -TERM "$(cat "$PID_FILE")" 2>/dev/null || true
    fi
fi

# Install web deps if needed.
if [[ ! -d "$REPO_ROOT/web/node_modules" ]]; then
    echo "  Installing web dependencies..."
    (cd "$REPO_ROOT/web" && npm install --silent)
fi

BACKEND_PID=""
FRONTEND_PID=""

cleanup() {
    trap - EXIT INT TERM
    echo ""
    echo "  Shutting down..."
    if [[ -n "$FRONTEND_PID" ]] && kill -0 "$FRONTEND_PID" 2>/dev/null; then
        kill -TERM "$FRONTEND_PID" 2>/dev/null || true
    fi
    if [[ -n "$BACKEND_PID" ]] && kill -0 "$BACKEND_PID" 2>/dev/null; then
        kill -TERM "$BACKEND_PID" 2>/dev/null || true
    fi
    wait 2>/dev/null || true
    echo "  Stopped. Run 'clawvisor-server start' to resume the installed daemon."
}
trap cleanup EXIT INT TERM

echo ""
echo "  Backend  : http://127.0.0.1:$BACKEND_PORT  (air, hot reload on .go changes)"
echo "  Frontend : http://127.0.0.1:$FRONTEND_PORT  (Vite, HMR — open this URL)"
echo "  Config   : ~/.clawvisor/config.yaml"
echo ""

# PUBLIC_URL pins the daemon's advertised origin to the Vite port, so OAuth
# redirect URIs and printed magic links point at Vite (the only URL that
# serves the live frontend; the backend's embedded SPA is just
# web/dist/placeholder.html in a dev checkout). Vite proxies /api/* back to
# the backend's actual listen port.
#
# Honor a pre-set PUBLIC_URL in the calling shell (e.g. when developing
# from a Docker host and you need `host.docker.internal:25297` so a
# container-side OpenClaw can reach back). Same goes for SERVER_HOST.
: "${PUBLIC_URL:=http://localhost:$FRONTEND_PORT}"
: "${SERVER_HOST:=127.0.0.1}"
export PUBLIC_URL SERVER_HOST
PORT="$BACKEND_PORT" air -c .air.toml &
BACKEND_PID=$!

BACKEND_PORT="$BACKEND_PORT" npm --prefix "$REPO_ROOT/web" run dev -- --port "$FRONTEND_PORT" --strictPort &
FRONTEND_PID=$!

# Exit when either process dies (bash 3.2-compatible — macOS ships with 3.2).
while kill -0 "$BACKEND_PID" 2>/dev/null && kill -0 "$FRONTEND_PID" 2>/dev/null; do
    sleep 1
done
