#!/usr/bin/env bash
# Boot the rendered Clawvisor artifacts locally (no cloud) and assert /health
# returns 200. This is the deterministic-lane stand-in for a VM.
#
# Steps:
#   1. terraform init/apply here → renders compose/Caddyfile/config/secrets.
#   2. `docker compose config` validates the rendered compose is engine-valid
#      (this is the light, always-run check).
#   3. If BOOT=1, build the image (repo Dockerfile) and boot app+postgres,
#      then poll https/http /health until 200 (heavier; local/keyed).
#
# Usage: ./run.sh            # render + validate compose (deterministic)
#        BOOT=1 ./run.sh     # also build + boot + health-check
set -euo pipefail
cd "$(dirname "$0")"

REPO_ROOT="$(git rev-parse --show-toplevel)"
IMAGE="${IMAGE:-clawvisor:local}"

echo "== render artifacts =="
terraform init -input=false -no-color >/dev/null
terraform apply -input=false -auto-approve -no-color -var "image=$IMAGE" >/dev/null
cd rendered

echo "== validate rendered compose =="
docker compose --env-file compose.env config >/dev/null
echo "rendered compose is valid"

if [ "${BOOT:-0}" != "1" ]; then
  echo "SKIP boot (set BOOT=1 to build + run + health-check)"
  exit 0
fi

echo "== build image =="
docker build -t "$IMAGE" -f "$REPO_ROOT/deploy/Dockerfile" "$REPO_ROOT"

echo "== boot app + postgres =="
docker compose --env-file compose.env up -d app postgres

cleanup() { docker compose --env-file compose.env down -v || true; }
trap cleanup EXIT

echo "== poll /health =="
deadline=$(( $(date +%s) + 120 ))
until curl -fsS "http://localhost:25297/health" >/dev/null 2>&1; do
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "FAIL: /health did not return 200 within 120s"
    docker compose --env-file compose.env logs app || true
    exit 1
  fi
  sleep 3
done
echo "PASS: /health returned 200"
