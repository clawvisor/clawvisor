#!/bin/sh
# Build cross-platform release binaries and checksums.
# Usage: scripts/build-release.sh <version>
#   e.g. scripts/build-release.sh v0.6.0
set -eu

if [ $# -lt 1 ]; then
  echo "Usage: $0 <version>" >&2
  echo "  e.g. $0 v0.6.0" >&2
  exit 1
fi

TAG="$1"
VERSION="${TAG#v}"

if [ ! -d "web/dist" ]; then
  echo "Error: web/dist/ not found. Build the frontend first:" >&2
  echo "  cd web && npm ci && npm run build" >&2
  exit 1
fi

MODULE="github.com/clawvisor/clawvisor/pkg/version"
BUILD_DATE=$(date -u +%Y-%m-%d)
LDFLAGS="-s -w -X ${MODULE}.Version=${VERSION} -X ${MODULE}.SkillPublishedAt=${BUILD_DATE}"
PLATFORMS="darwin/arm64 darwin/amd64 linux/arm64 linux/amd64"

rm -rf dist
mkdir -p dist

for PLATFORM in $PLATFORMS; do
  GOOS="${PLATFORM%/*}"
  GOARCH="${PLATFORM#*/}"
  OUTPUT="dist/clawvisor-${GOOS}-${GOARCH}"

  echo "Building ${OUTPUT}..."
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    go build -ldflags="$LDFLAGS" -o "$OUTPUT" ./cmd/clawvisor
done

# Build the iMessage helper .app bundle for macOS only. This is a separate,
# stable binary that holds Full Disk Access so users don't need to re-grant
# FDA on every clawvisor update. The .app bundle structure lets macOS read
# Info.plist for the display name in FDA settings, no CGO required.
HELPER_APP="Clawvisor iMessage Helper.app"
HELPER_PLATFORMS="darwin/arm64 darwin/amd64"
for PLATFORM in $HELPER_PLATFORMS; do
  GOOS="${PLATFORM%/*}"
  GOARCH="${PLATFORM#*/}"
  TARBALL="dist/clawvisor-imessage-helper-${GOOS}-${GOARCH}.tar.gz"

  echo "Building ${TARBALL}..."
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    go build -ldflags="$LDFLAGS" -o "dist/.helper-tmp" ./cmd/imessage-helper

  mkdir -p "dist/${HELPER_APP}/Contents/MacOS"
  mv "dist/.helper-tmp" "dist/${HELPER_APP}/Contents/MacOS/clawvisor-imessage-helper"
  cp cmd/imessage-helper/Info.plist "dist/${HELPER_APP}/Contents/Info.plist"
  tar -czf "$TARBALL" -C dist "${HELPER_APP}"
  rm -rf "dist/${HELPER_APP}"
done

echo "Generating checksums..."
cd dist
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum clawvisor-* > checksums.txt
else
  shasum -a 256 clawvisor-* > checksums.txt
fi
cd ..

echo "Done. Release artifacts in dist/:"
ls -lh dist/
