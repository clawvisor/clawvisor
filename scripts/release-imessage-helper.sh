#!/bin/sh
# Build the iMessage helper for darwin/{arm64,amd64}, compute SHAs, and
# rewrite pkg/version/imessage_helper.go with the new pin. Run this only when
# the helper itself changes (e.g. protocol bump in cmd/imessage-helper).
#
# Usage: scripts/release-imessage-helper.sh <tag>
#   e.g. scripts/release-imessage-helper.sh v0.10.0
#
# After this runs:
#   1. Review and commit the changes to pkg/version/imessage_helper.go.
#   2. Tag and release <tag> with the dist/ tarballs uploaded as assets.
set -eu

if [ $# -lt 1 ]; then
  echo "Usage: $0 <tag>" >&2
  echo "  e.g. $0 v0.10.0" >&2
  exit 1
fi

TAG="$1"
VERSION="${TAG#v}"

MODULE="github.com/clawvisor/clawvisor/pkg/version"
BUILD_DATE=$(date -u +%Y-%m-%d)
HELPER_APP="Clawvisor iMessage Helper.app"
HELPER_PLATFORMS="darwin/arm64 darwin/amd64"
HELPER_LDFLAGS="-s -w -X ${MODULE}.Version=${VERSION} -X ${MODULE}.SkillPublishedAt=${BUILD_DATE}"

mkdir -p dist
SHA_ARM64=""
SHA_AMD64=""
for PLATFORM in $HELPER_PLATFORMS; do
  GOOS="${PLATFORM%/*}"
  GOARCH="${PLATFORM#*/}"
  TARBALL="dist/clawvisor-imessage-helper-${GOOS}-${GOARCH}.tar.gz"

  echo "Building ${TARBALL}..."
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    go build -ldflags="$HELPER_LDFLAGS" -o "dist/.helper-tmp" ./cmd/imessage-helper

  rm -rf "dist/${HELPER_APP}"
  mkdir -p "dist/${HELPER_APP}/Contents/MacOS"
  mv "dist/.helper-tmp" "dist/${HELPER_APP}/Contents/MacOS/clawvisor-imessage-helper"
  cp cmd/imessage-helper/Info.plist "dist/${HELPER_APP}/Contents/Info.plist"
  tar -czf "$TARBALL" -C dist "${HELPER_APP}"
  rm -rf "dist/${HELPER_APP}"

  if command -v sha256sum >/dev/null 2>&1; then
    SHA=$(sha256sum "$TARBALL" | awk '{print $1}')
  else
    SHA=$(shasum -a 256 "$TARBALL" | awk '{print $1}')
  fi

  case "$GOARCH" in
    arm64) SHA_ARM64="$SHA" ;;
    amd64) SHA_AMD64="$SHA" ;;
  esac
done

PIN_FILE="pkg/version/imessage_helper.go"
echo "Rewriting ${PIN_FILE}..."

# Replace the tag and SHAs in place. Using awk for portable in-place edit.
TMP=$(mktemp)
awk -v tag="$TAG" -v arm64="$SHA_ARM64" -v amd64="$SHA_AMD64" '
  /^const IMessageHelperReleaseTag =/ {
    print "const IMessageHelperReleaseTag = \"" tag "\""
    next
  }
  /"darwin\/arm64":/ {
    print "\t\"darwin/arm64\": \"" arm64 "\","
    next
  }
  /"darwin\/amd64":/ {
    print "\t\"darwin/amd64\": \"" amd64 "\","
    next
  }
  { print }
' "$PIN_FILE" > "$TMP"
mv "$TMP" "$PIN_FILE"

cat <<EOF

Done. Pinned ${PIN_FILE} to:
  tag:           ${TAG}
  darwin/arm64:  ${SHA_ARM64}
  darwin/amd64:  ${SHA_AMD64}

Next steps:
  1. git diff ${PIN_FILE}    # review the rewrite
  2. git add ${PIN_FILE} && git commit
  3. Tag and release ${TAG}, uploading:
       dist/clawvisor-imessage-helper-darwin-arm64.tar.gz
       dist/clawvisor-imessage-helper-darwin-amd64.tar.gz
EOF
