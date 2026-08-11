#!/bin/sh
set -eu

usage() {
    cat >&2 <<'EOF'
Usage: scripts/render-staging-installer.sh CLAIM [claude-code|codex] [AGENT_NAME]
   or: CLAIM=<claim> scripts/render-staging-installer.sh [TARGET] [AGENT_NAME]
   or: scripts/render-staging-installer.sh --claim-stdin [TARGET] [AGENT_NAME]

Renders the current local skill-only installer to stdout while baking in the
staging Clawvisor URLs. The claim must have been minted by that same staging
control plane and is not consumed until the rendered installer is executed.

Examples:
  scripts/render-staging-installer.sh --claim-stdin codex | sh -s -- --dry-run
  # Paste the claim and press return when the script starts.

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
if [ "$ARG_CLAIM" = --claim-stdin ]; then
    [ -z "$ENV_CLAIM" ] || usage
    shift
    # POSIX read returns non-zero when EOF terminates a non-empty final line,
    # but it still stores the bytes it read. Preserve that value so both
    # `printf %s "$claim" | ...` and newline-terminated input work.
    IFS= read -r CLAIM || :
    [ -n "$CLAIM" ] || usage
elif [ -n "$ENV_CLAIM" ] && { [ "$#" -eq 0 ] || [ -z "$ARG_CLAIM" ]; }; then
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
    # The server rejects duplicate names. The PID keeps two renders started in
    # the same second distinct without depending on a non-POSIX random tool.
    AGENT_NAME="${TARGET}-staging-test-$(date -u +%Y%m%d%H%M%S)-$$"
fi

STAGING_URL=${CLAWVISOR_STAGING_URL:-https://app.staging.clawvisor.com}
STAGING_LLM_URL=${CLAWVISOR_STAGING_LLM_URL:-https://llm.staging.clawvisor.com}
REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

cd "$REPO_ROOT"
# Keep the unconsumed bearer claim out of both argv and the environment of the
# long-lived `go run` process (and the temporary renderer binary it starts).
# Unsetting before the pipeline is essential for the documented CLAIM= form,
# because inherited shell variables otherwise remain exported.
unset RENDER_CLAIM
RENDER_CLAIM=$CLAIM
unset CLAIM ENV_CLAIM ARG_CLAIM
printf '%s\n' "$RENDER_CLAIM" | go run ./cmd/render-installer \
    -app-url "$STAGING_URL" \
    -llm-url "$STAGING_LLM_URL" \
    -target "$TARGET" \
    -agent-name "$AGENT_NAME" \
    -claim-stdin
