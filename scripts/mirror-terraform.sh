#!/usr/bin/env bash
# Publish-only registry mirror for the Terraform modules.
#
# DEFERRED TO v1.1 (PRD §6/§13): the monorepo path
# (github.com/clawvisor/clawvisor//deploy/terraform/modules/vm-docker) is the
# v1 distribution. The Terraform Registry requires a dedicated repo named
# `terraform-clawvisor-modules`; this script subtree-pushes the modules there
# on release tags. The terraform-ci.yml `mirror` job is disabled (`if: false`)
# until that repo exists — flip it on then.
#
# This script is intentionally present-but-inert so the wiring is reviewable
# now and a single flag flip enables it later. Running it without
# MIRROR_REPO set is a no-op.
set -euo pipefail
cd "$(dirname "$0")/.."

MIRROR_REPO="${MIRROR_REPO:-}"
if [ -z "$MIRROR_REPO" ]; then
  echo "mirror-terraform: MIRROR_REPO unset — publishing is deferred to v1.1 (no-op)."
  exit 0
fi

SUBTREE_PREFIX="deploy/terraform/modules"
TAG="${GITHUB_REF_NAME:-$(git describe --tags --abbrev=0)}"

echo "Splitting $SUBTREE_PREFIX and pushing to $MIRROR_REPO @ $TAG"
SPLIT_SHA="$(git subtree split --prefix="$SUBTREE_PREFIX" HEAD)"
git push "$MIRROR_REPO" "${SPLIT_SHA}:refs/heads/main" --force
git push "$MIRROR_REPO" "HEAD:refs/tags/${TAG}" || true
echo "Mirror push complete."
