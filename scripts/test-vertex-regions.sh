#!/usr/bin/env bash
# Test Vertex AI Haiku availability across regions.
#
# Usage:
#   ./scripts/test-vertex-regions.sh                          # test default regions
#   ./scripts/test-vertex-regions.sh global us-east5 europe-west1  # test specific regions
#
# Requires:
#   - VERTEX_PROJECT_ID env var (or gcloud default project)
#   - gcloud auth application-default login (ADC)
#
set -euo pipefail

MODEL="${VERTEX_MODEL:-claude-haiku-4-5@20251001}"
PROJECT="${VERTEX_PROJECT_ID:-$(gcloud config get-value project 2>/dev/null)}"
DEFAULT_REGIONS=(global us-east5 us-central1 europe-west1 europe-west4 asia-southeast1)

if [[ -z "$PROJECT" ]]; then
  echo "Error: set VERTEX_PROJECT_ID or configure gcloud default project" >&2
  exit 1
fi

TOKEN=$(gcloud auth application-default print-access-token 2>/dev/null) || {
  echo "Error: run 'gcloud auth application-default login' first" >&2
  exit 1
}

REGIONS=("${@:-${DEFAULT_REGIONS[@]}}")

BODY=$(cat <<EOF
{
  "anthropic_version": "vertex-2023-10-16",
  "max_tokens": 16,
  "temperature": 0,
  "messages": [{"role": "user", "content": "Say ok"}]
}
EOF
)

printf "%-22s  %-6s  %-8s  %s\n" "REGION" "STATUS" "LATENCY" "RESPONSE"
printf "%-22s  %-6s  %-8s  %s\n" "------" "------" "-------" "--------"

for region in "${REGIONS[@]}"; do
  url="https://${region}-aiplatform.googleapis.com/v1/projects/${PROJECT}/locations/${region}/publishers/anthropic/models/${MODEL}:rawPredict"

  start=$(python3 -c 'import time; print(int(time.time()*1000))')

  http_code=$(curl -s -o /tmp/vertex-test-response.json -w "%{http_code}" \
    -X POST "$url" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d "$BODY" \
    --connect-timeout 5 \
    --max-time 30 \
    2>/dev/null) || http_code="ERR"

  end=$(python3 -c 'import time; print(int(time.time()*1000))')
  latency=$(( end - start ))

  if [[ "$http_code" == "200" ]]; then
    # Extract first text content block
    reply=$(python3 -c "
import json, sys
try:
    d = json.load(open('/tmp/vertex-test-response.json'))
    for b in d.get('content', []):
        if b.get('type') == 'text':
            print(b['text'][:60])
            sys.exit()
    print('(no text block)')
except: print('(parse error)')
" 2>/dev/null)
    printf "%-22s  \033[32m%-6s\033[0m  %5dms  %s\n" "$region" "$http_code" "$latency" "$reply"
  else
    error=$(python3 -c "
import json
try:
    d = json.load(open('/tmp/vertex-test-response.json'))
    e = d.get('error', {})
    print(e.get('message', str(e))[:60] if e else '(empty body)')
except: print('(no response body)')
" 2>/dev/null)
    printf "%-22s  \033[31m%-6s\033[0m  %5dms  %s\n" "$region" "$http_code" "$latency" "$error"
  fi
done

rm -f /tmp/vertex-test-response.json
