#!/usr/bin/env bash
set -euo pipefail

# Rebuild the clawvisor binary from source and restart the daemon.
# Run from the repo root: ./scripts/upgrade.sh
#
# By default, discovers the installed binary path from the launchd plist
# (macOS) or systemd unit (Linux). Override with CLAWVISOR_BIN.

PLIST="$HOME/Library/LaunchAgents/com.clawvisor.daemon.plist"
SYSTEMD_UNIT="$HOME/.config/systemd/user/clawvisor.service"

# Resolve the installed binary path.
resolve_binary() {
    if [[ -n "${CLAWVISOR_BIN:-}" ]]; then
        echo "$CLAWVISOR_BIN"
        return
    fi

    case "$(uname -s)" in
        Darwin)
            if [[ -f "$PLIST" ]]; then
                # Extract the first ProgramArguments string (the binary path).
                /usr/libexec/PlistBuddy -c "Print :ProgramArguments:0" "$PLIST" 2>/dev/null && return
            fi
            ;;
        Linux)
            if [[ -f "$SYSTEMD_UNIT" ]]; then
                # ExecStart=/path/to/clawvisor start
                grep -oP '^ExecStart=\K\S+' "$SYSTEMD_UNIT" 2>/dev/null && return
            fi
            ;;
    esac

    echo >&2 "Could not determine installed binary path."
    echo >&2 "Set CLAWVISOR_BIN or run 'clawvisor install' first."
    exit 1
}

BIN=$(resolve_binary)
echo "  Binary: $BIN"

# Find the repo root (directory containing go.mod).
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
if [[ ! -f "$REPO_ROOT/go.mod" ]]; then
    echo >&2 "Could not find go.mod — run this script from the repo."
    exit 1
fi

# Build the frontend if web/package.json exists, then install to ~/.clawvisor.
WEBDIR="$REPO_ROOT/web"
FRONTEND_INSTALL_DIR="$HOME/.clawvisor/web/dist"
if [[ -f "$WEBDIR/package.json" ]]; then
    echo "  Building frontend..."
    (cd "$WEBDIR" && npm install --silent && npm run build --silent)
    echo "  Installing frontend to $FRONTEND_INSTALL_DIR ..."
    rm -rf "$FRONTEND_INSTALL_DIR"
    mkdir -p "$FRONTEND_INSTALL_DIR"
    cp -R "$WEBDIR/dist/." "$FRONTEND_INSTALL_DIR/"
fi

echo "  Building backend from $REPO_ROOT ..."
(cd "$REPO_ROOT" && go build -o "$BIN" ./cmd/clawvisor/)
echo "  Built $(${BIN} --version 2>/dev/null || echo 'ok')"

echo "  Restarting daemon..."
"$BIN" stop 2>/dev/null || true
"$BIN" start

echo "  Done."
