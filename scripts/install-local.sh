#!/bin/sh
set -eu

# Clawvisor local daemon installer — curl-pipe-sh friendly.
# Must be POSIX sh compatible since `curl | sh` ignores shebangs.
# Usage: curl -fsSL https://raw.githubusercontent.com/clawvisor/clawvisor/main/scripts/install-local.sh | sh

INSTALL_DIR="${CLAWVISOR_LOCAL_INSTALL_DIR:-$HOME/.clawvisor/bin}"
DATA_DIR="${CLAWVISOR_LOCAL_DATA_DIR:-$HOME/.clawvisor/local}"
REPO="${CLAWVISOR_REPO:-clawvisor/clawvisor}"
BINARY="clawvisor-local"
API_BASE="${CLAWVISOR_API_BASE:-https://api.github.com}"
DOWNLOAD_BASE="${CLAWVISOR_DOWNLOAD_BASE:-https://github.com}"

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64) ARCH="amd64" ;;
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

echo "  Installing Clawvisor local daemon ($OS/$ARCH)..."

LATEST="$(curl -fsSL "${API_BASE}/repos/${REPO}/releases/latest" | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')"
if [ -z "$LATEST" ]; then
  echo "Error: could not determine latest release" >&2
  exit 1
fi
echo "  Version: $LATEST"

ASSET="${BINARY}-${OS}-${ARCH}"
BASE_URL="${DOWNLOAD_BASE}/${REPO}/releases/download/${LATEST}"
ASSET_URL="${BASE_URL}/${ASSET}"
CHECKSUMS_URL="${BASE_URL}/checksums.txt"

mkdir -p "$INSTALL_DIR" "$DATA_DIR"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "  Downloading checksums..."
curl -fsSL "$CHECKSUMS_URL" -o "$TMP/checksums.txt"

EXPECTED_HASH="$(awk -v asset="$ASSET" '$2 == asset { print $1 }' "$TMP/checksums.txt")"
if [ -z "$EXPECTED_HASH" ]; then
  echo "Error: latest release does not include a published asset for $ASSET" >&2
  echo "  Expected to find it in checksums.txt at ${CHECKSUMS_URL}" >&2
  exit 1
fi

echo "  Downloading binary..."
curl -fsSL "$ASSET_URL" -o "$TMP/$ASSET"

if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL_HASH="$(sha256sum "$TMP/$ASSET" | awk '{print $1}')"
else
  ACTUAL_HASH="$(shasum -a 256 "$TMP/$ASSET" | awk '{print $1}')"
fi

if [ "$EXPECTED_HASH" != "$ACTUAL_HASH" ]; then
  echo "Error: checksum mismatch for $ASSET" >&2
  echo "  expected: $EXPECTED_HASH" >&2
  echo "  actual:   $ACTUAL_HASH" >&2
  exit 1
fi

mv "$TMP/$ASSET" "$INSTALL_DIR/$BINARY"
chmod +x "$INSTALL_DIR/$BINARY"

if [ "$OS" = "darwin" ] && command -v codesign >/dev/null 2>&1; then
  codesign -s - "$INSTALL_DIR/$BINARY" 2>/dev/null || true
fi

echo "  Installed to $INSTALL_DIR/$BINARY"

link_into_path() {
  for dir in "$HOME/.local/bin" "$HOME/bin" "/usr/local/bin"; do
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

add_to_path() {
  rc_file="$1"
  export_line="export PATH=\"$INSTALL_DIR:\$PATH\""
  if [ -f "$rc_file" ] && grep -qF "$INSTALL_DIR" "$rc_file"; then
    return 0
  fi
  echo "" >> "$rc_file"
  echo "# Added by Clawvisor local installer" >> "$rc_file"
  echo "$export_line" >> "$rc_file"
  echo "  Added $INSTALL_DIR to PATH in $rc_file"
}

if ! echo "$PATH" | grep -q "$INSTALL_DIR"; then
  if ! link_into_path; then
    SHELL_NAME="$(basename "${SHELL:-/bin/bash}")"
    case "$SHELL_NAME" in
      zsh) add_to_path "$HOME/.zshrc" ;;
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

if [ "${CLAWVISOR_LOCAL_SKIP_START:-}" = "1" ] || [ "${CLAWVISOR_SKIP_START:-}" = "1" ]; then
  echo "  Skipping service install/start."
  exit 0
fi

echo ""
echo "  Installing and starting the local daemon..."
echo ""

exec "$INSTALL_DIR/$BINARY" install-service
