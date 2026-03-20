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
LDFLAGS="-s -w -X ${MODULE}.Version=${VERSION}"
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
