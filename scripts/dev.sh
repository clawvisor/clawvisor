#!/usr/bin/env bash
set -euo pipefail

# Run the Clawvisor daemon and web UI locally with hot reload.
#
# - Backend: `air` rebuilds and restarts on .go changes (see .air.toml).
# - Frontend: Vite dev server with HMR; proxies API/WS calls to the backend.
# - Ports: chosen dynamically so this won't collide with the installed daemon.
# - Config: reuses ~/.clawvisor/config.yaml (PORT/SERVER_HOST overridden via env).
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
    node -e "const s=require('net').createServer();s.listen(0,'127.0.0.1',()=>{const p=s.address().port;s.close(()=>console.log(p));});"
}

BACKEND_PORT="$(find_free_port)"
FRONTEND_PORT="$(find_free_port)"

# air's tmp_dir create is non-recursive, so make sure the parent exists.
mkdir -p "$REPO_ROOT/bin/.air"

# Stop the installed daemon (if any) to avoid SQLite lock contention on
# ~/.clawvisor/clawvisor.db. Detect via the daemon's PID file.
PID_FILE="$HOME/.clawvisor/.daemon.pid"
if [[ -f "$PID_FILE" ]] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
    echo "  Stopping installed daemon (PID $(cat "$PID_FILE"))..."
    if command -v clawvisor >/dev/null 2>&1; then
        clawvisor stop >/dev/null 2>&1 || true
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
    echo "  Stopped. Run 'clawvisor start' to resume the installed daemon."
}
trap cleanup EXIT INT TERM

echo ""
echo "  Backend  : http://127.0.0.1:$BACKEND_PORT  (air, hot reload on .go changes)"
echo "  Frontend : http://127.0.0.1:$FRONTEND_PORT  (Vite, HMR — open this URL)"
echo "  Config   : ~/.clawvisor/config.yaml"
echo ""

# Rewrite ":$BACKEND_PORT" → ":$FRONTEND_PORT" in the daemon's output so the
# printed magic-link URL points to Vite (the only URL that serves the live
# frontend; the backend's embedded SPA is just web/dist/placeholder.html in a
# dev checkout). Process substitution keeps $! as air's PID so cleanup works.
PORT="$BACKEND_PORT" SERVER_HOST="127.0.0.1" air -c .air.toml \
    > >(awk -v from=":$BACKEND_PORT" -v to=":$FRONTEND_PORT" '{gsub(from, to); print; fflush()}') \
    2>&1 &
BACKEND_PID=$!

BACKEND_PORT="$BACKEND_PORT" npm --prefix "$REPO_ROOT/web" run dev -- --port "$FRONTEND_PORT" --strictPort &
FRONTEND_PID=$!

# Exit when either process dies (bash 3.2-compatible — macOS ships with 3.2).
while kill -0 "$BACKEND_PID" 2>/dev/null && kill -0 "$FRONTEND_PID" 2>/dev/null; do
    sleep 1
done
