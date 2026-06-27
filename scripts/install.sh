#!/bin/sh
# Clawvisor daemon installer
# Usage: curl -fsSL https://raw.githubusercontent.com/clawvisor/clawvisor/main/scripts/install.sh | sh
set -eu

REPO="${CLAWVISOR_REPO:-clawvisor/clawvisor}"
INSTALL_DIR="${CLAWVISOR_INSTALL_DIR:-$HOME/.clawvisor/bin}"
BINARY="clawvisor-server"
API_BASE="${CLAWVISOR_API_BASE:-https://api.github.com}"
DOWNLOAD_BASE="${CLAWVISOR_DOWNLOAD_BASE:-https://github.com}"

# -- Helpers ----------------------------------------------------------------

info()  { printf '  %s\n' "$@"; }
error() { printf '  Error: %s\n' "$@" >&2; exit 1; }

# Use curl or wget, whichever is available.
fetch() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO- "$1"
  else
    error "curl or wget is required"
  fi
}

# Download a URL to a file.
download() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then
    wget -q "$1" -O "$2"
  else
    error "curl or wget is required"
  fi
}

# -- Detect platform --------------------------------------------------------

OS="$(uname -s)"
case "$OS" in
  Darwin) OS="darwin" ;;
  Linux)  OS="linux"  ;;
  *)      OS="unsupported" ;;
esac

ARCH="$(uname -m)"
case "$ARCH" in
  arm64|aarch64) ARCH="arm64" ;;
  x86_64)        ARCH="amd64" ;;
  *)             ARCH="unsupported" ;;
esac

if [ "$OS" = "unsupported" ] || [ "$ARCH" = "unsupported" ]; then
  RAW_OS="$(uname -s)"
  RAW_ARCH="$(uname -m)"
  printf '\n'
  info "Unsupported platform: ${RAW_OS}/${RAW_ARCH}"
  info "Pre-built binaries are available for macOS and Linux (arm64, amd64)."
  info ""
  info "To install from source (requires Go 1.25+ and Node.js 18+):"
  info "  git clone https://github.com/${REPO}.git"
  info "  cd clawvisor && make build"
  info "  ./bin/clawvisor-server install"
  printf '\n'
  exit 1
fi

info "Installing Clawvisor ($OS/$ARCH)..."

# -- Resolve version --------------------------------------------------------

if [ -n "${VERSION:-}" ]; then
  # Allow both "0.6.0" and "v0.6.0"
  case "$VERSION" in
    v*) TAG="$VERSION" ;;
    *)  TAG="v$VERSION" ;;
  esac
  info "Version: $TAG (from \$VERSION)"
else
  TAG="$(fetch "${API_BASE}/repos/${REPO}/releases/latest" \
    | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')"
  if [ -z "$TAG" ]; then
    error "could not determine latest release"
  fi
  info "Version: $TAG (latest)"
fi

# -- Download binary + checksums --------------------------------------------

ASSET="${BINARY}-${OS}-${ARCH}"
BASE_URL="${DOWNLOAD_BASE}/${REPO}/releases/download/${TAG}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

info "Downloading ${ASSET}..."
download "${BASE_URL}/${ASSET}" "${TMP}/${ASSET}"
download "${BASE_URL}/checksums.txt" "${TMP}/checksums.txt"

# -- Verify checksum --------------------------------------------------------

EXPECTED="$(awk -v asset="$ASSET" '$2 == asset { print $1 }' "${TMP}/checksums.txt")"
if [ -z "$EXPECTED" ]; then
  error "no checksum found for ${ASSET} in checksums.txt"
fi

if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL="$(sha256sum "${TMP}/${ASSET}" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  ACTUAL="$(shasum -a 256 "${TMP}/${ASSET}" | awk '{print $1}')"
else
  error "sha256sum or shasum is required for checksum verification"
fi

if [ "$EXPECTED" != "$ACTUAL" ]; then
  error "checksum mismatch: expected ${EXPECTED}, got ${ACTUAL}"
fi
info "Checksum verified."

# -- Install binary ---------------------------------------------------------

mkdir -p "$INSTALL_DIR"
mv "${TMP}/${ASSET}" "${INSTALL_DIR}/${BINARY}"
chmod +x "${INSTALL_DIR}/${BINARY}"
info "Installed to ${INSTALL_DIR}/${BINARY}"

# -- Add to PATH ------------------------------------------------------------

add_to_path() {
  rc_file="$1"
  export_line="export PATH=\"\$HOME/.clawvisor/bin:\$PATH\""
  if [ -f "$rc_file" ] && grep -qF ".clawvisor/bin" "$rc_file"; then
    return 0
  fi
  printf '\n# Added by Clawvisor installer\n%s\n' "$export_line" >> "$rc_file"
  info "Added ~/.clawvisor/bin to PATH in $rc_file"
}

if ! echo "$PATH" | grep -q "\.clawvisor/bin"; then
  SHELL_NAME="$(basename "${SHELL:-/bin/sh}")"
  case "$SHELL_NAME" in
    zsh)  add_to_path "$HOME/.zshrc" ;;
    bash)
      if [ -f "$HOME/.bash_profile" ]; then
        add_to_path "$HOME/.bash_profile"
      else
        add_to_path "$HOME/.bashrc"
      fi
      ;;
    fish) add_to_path "$HOME/.config/fish/config.fish" ;;
    *)
      info ""
      info "Could not auto-configure PATH for $SHELL_NAME."
      info "Add this to your shell config:"
      info "  export PATH=\"\$HOME/.clawvisor/bin:\$PATH\""
      ;;
  esac
  export PATH="$INSTALL_DIR:$PATH"
fi

# -- Launch setup -----------------------------------------------------------

printf '\n'
if [ "${CLAWVISOR_SKIP_START:-}" = "1" ]; then
  info "Installation complete!"
  printf '\n'
elif [ -t 0 ]; then
  info "Starting daemon installer..."
  printf '\n'
  exec "$INSTALL_DIR/$BINARY" install
else
  info "Installation complete!"
  info ""
  info "To finish setup, restart your shell and run:"
  info "  clawvisor-server install"
  info ""
  info "Or run it now with:"
  info "  ~/.clawvisor/bin/clawvisor-server install"
  printf '\n'
fi
