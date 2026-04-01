#!/bin/sh
set -eu

# Clawvisor daemon installer — curl-pipe-sh friendly.
# Must be POSIX sh compatible (dash, ash, etc.) since `curl | sh` ignores shebangs.
# Usage: curl -fsSL https://clawvisor.com/install.sh | sh

INSTALL_DIR="${CLAWVISOR_INSTALL_DIR:-$HOME/.clawvisor/bin}"
DATA_DIR="${CLAWVISOR_DATA_DIR:-$HOME/.clawvisor}"
REPO="${CLAWVISOR_REPO:-clawvisor/clawvisor}"
BINARY="clawvisor"
API_BASE="${CLAWVISOR_API_BASE:-https://api.github.com}"
DOWNLOAD_BASE="${CLAWVISOR_DOWNLOAD_BASE:-https://github.com}"

# Detect OS and architecture.
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64)  ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)
    echo "Error: unsupported architecture $ARCH" >&2
    exit 1
    ;;
esac

case "$OS" in
  darwin|linux) ;;
  *)
    echo "Error: unsupported OS $OS" >&2
    exit 1
    ;;
esac

echo "  Installing Clawvisor daemon ($OS/$ARCH)..."

# Fetch latest release tag.
LATEST="$(curl -fsSL "${API_BASE}/repos/${REPO}/releases/latest" | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')"
if [ -z "$LATEST" ]; then
  echo "Error: could not determine latest release" >&2
  exit 1
fi
echo "  Version: $LATEST"

ASSET="${BINARY}_${LATEST#v}_${OS}_${ARCH}.tar.gz"
URL="${DOWNLOAD_BASE}/${REPO}/releases/download/${LATEST}/${ASSET}"

# Download and extract.
mkdir -p "$INSTALL_DIR"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

curl -fsSL "$URL" -o "$TMP/$ASSET"
tar -xzf "$TMP/$ASSET" -C "$TMP"
mv "$TMP/$BINARY" "$INSTALL_DIR/$BINARY"
chmod +x "$INSTALL_DIR/$BINARY"

# Ensure data directory exists.
mkdir -p "$DATA_DIR/logs"

echo "  Installed to $INSTALL_DIR/$BINARY"

# Try to symlink into an existing user-writable PATH directory so the binary
# is available immediately — no terminal restart needed.
link_into_path() {
  for dir in "$HOME/.local/bin" "$HOME/bin" "/usr/local/bin"; do
    # Must already be on PATH, exist, and be writable.
    case ":$PATH:" in
      *":$dir:"*) ;;
      *) continue ;;
    esac
    if [ -d "$dir" ] && [ -w "$dir" ]; then
      ln -sf "$INSTALL_DIR/$BINARY" "$dir/$BINARY"
      echo "  Symlinked $dir/$BINARY → $INSTALL_DIR/$BINARY"
      return 0
    fi
  done
  return 1
}

# Add to PATH if not already present.
add_to_path() {
  local rc_file="$1"
  local export_line="export PATH=\"$INSTALL_DIR:\$PATH\""
  if [ -f "$rc_file" ] && grep -qF "$INSTALL_DIR" "$rc_file"; then
    return 0
  fi
  echo "" >> "$rc_file"
  echo "# Added by Clawvisor installer" >> "$rc_file"
  echo "$export_line" >> "$rc_file"
  echo "  Added $INSTALL_DIR to PATH in $rc_file"
}

if ! echo "$PATH" | grep -q "$INSTALL_DIR"; then
  if ! link_into_path; then
    # No writable PATH dir found — fall back to editing shell rc.
    SHELL_NAME="$(basename "${SHELL:-/bin/bash}")"
    case "$SHELL_NAME" in
      zsh)  add_to_path "$HOME/.zshrc" ;;
      bash)
        if [ -f "$HOME/.bash_profile" ]; then
          add_to_path "$HOME/.bash_profile"
        else
          add_to_path "$HOME/.bashrc"
        fi
        ;;
      *)
        echo ""
        echo "  Could not auto-configure PATH for $SHELL_NAME."
        echo "  Add this to your shell config:"
        echo "    export PATH=\"$INSTALL_DIR:\$PATH\""
        echo ""
        ;;
    esac
  fi
  export PATH="$INSTALL_DIR:$PATH"
fi

echo ""
echo "  Starting Clawvisor daemon for first-run setup..."
echo ""

# Allow tests to stop here without exec'ing into the daemon.
if [ "${CLAWVISOR_SKIP_START:-}" = "1" ]; then
  echo "  Skipping daemon start (CLAWVISOR_SKIP_START=1)."
  exit 0
fi

# Start the daemon in the foreground for first-run.
exec "$INSTALL_DIR/$BINARY" start
