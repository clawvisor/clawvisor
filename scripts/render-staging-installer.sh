#!/bin/sh
set -eu

usage() {
    cat >&2 <<'EOF'
Usage: scripts/render-staging-installer.sh CLAIM [claude-code|codex] [AGENT_NAME]
   or: CLAIM=<claim> scripts/render-staging-installer.sh [TARGET] [AGENT_NAME]

Renders the current local skill-only installer to stdout while baking in the
staging Clawvisor URLs. The claim must have been minted by that same staging
control plane and is not consumed until the rendered installer is executed.

Examples:
  CLAIM=<claim> scripts/render-staging-installer.sh > /tmp/clawvisor-installer.sh
  sh /tmp/clawvisor-installer.sh --dry-run

  CLAIM=<claim> scripts/render-staging-installer.sh codex | sh -s -- --dry-run

Overrides:
  CLAWVISOR_STAGING_URL      defaults to https://app.staging.clawvisor.com
  CLAWVISOR_STAGING_LLM_URL  defaults to https://llm.staging.clawvisor.com
EOF
    exit 2
}

# Preserve an inline environment assignment before reading positional args. In
# `CLAIM=value script "$CLAIM"`, the caller's shell expands "$CLAIM" before
# applying the inline assignment, so argv contains an empty string even though
# the child environment correctly contains CLAIM=value.
ENV_CLAIM=${CLAIM:-}
ARG_CLAIM=${1:-}
if [ -n "$ENV_CLAIM" ] && { [ "$#" -eq 0 ] || [ -z "$ARG_CLAIM" ]; }; then
    CLAIM=$ENV_CLAIM
    if [ "$#" -gt 0 ]; then
        shift
    fi
elif [ -n "$ENV_CLAIM" ] && { [ "$ARG_CLAIM" = claude-code ] || [ "$ARG_CLAIM" = codex ]; }; then
    # Environment form with the target as the first real positional argument.
    CLAIM=$ENV_CLAIM
elif [ -n "$ARG_CLAIM" ]; then
    CLAIM=$ARG_CLAIM
    shift
else
    usage
fi

[ "$#" -le 2 ] || usage

TARGET=${1:-claude-code}
case "$TARGET" in
    claude-code|codex) ;;
    *) usage ;;
esac

if [ "$#" -ge 2 ]; then
    AGENT_NAME=$2
else
    AGENT_NAME="${TARGET}-staging-test-$(date -u +%Y%m%d%H%M%S)"
fi

STAGING_URL=${CLAWVISOR_STAGING_URL:-https://app.staging.clawvisor.com}
STAGING_LLM_URL=${CLAWVISOR_STAGING_LLM_URL:-https://llm.staging.clawvisor.com}
REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

cd "$REPO_ROOT"
exec go run ./cmd/render-installer \
    -app-url "$STAGING_URL" \
    -llm-url "$STAGING_LLM_URL" \
    -target "$TARGET" \
    -agent-name "$AGENT_NAME" \
    -claim "$CLAIM"
