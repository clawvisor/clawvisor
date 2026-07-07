#!/usr/bin/env bash
# CI assertion for spec 03: "No secret literal in terraform plan -json output."
#
# GOTCHA (verified): `terraform show -json` does NOT redact *known* sensitive
# values — it emits them in plaintext alongside a sensitivity map. Only values
# that are still "known after apply" are absent. So this scan runs a FRESH plan
# (no prior state), which is exactly the state an operator reviews before their
# first apply: every generated secret (bootstrap token, DB password) is
# unknown, and the JWT/VAULT values are generated on-instance and never enter
# Terraform at all. The scan therefore passes on a correct module and FAILS if
# a regression makes any secret known-and-plaintext at plan time (e.g. moving a
# generated value into a non-sensitive output or a plaintext local).
#
# Target: examples/local-docker, which uses only the `local` + `random`
# providers, so a fresh plan needs NO cloud credentials and is deterministic.
set -euo pipefail
cd "$(dirname "$0")/.."

DIR=deploy/terraform/examples/local-docker
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK" "$DIR/tfplan.bin" "$DIR/plan.json"' EXIT

# Fresh: no prior state (TF_DATA_DIR isolates the plugin cache from any local
# .terraform, and we never point at an existing state file).
export TF_DATA_DIR="$WORK/.terraform"
terraform -chdir="$DIR" init -input=false -no-color >/dev/null
terraform -chdir="$DIR" plan -out=tfplan.bin -input=false -no-color >/dev/null
terraform -chdir="$DIR" show -json tfplan.bin > "$DIR/plan.json"

fail=0
scan() {
  local label="$1" pattern="$2"
  # -E extended regex, -o so the match (not the whole line) is what we report.
  if grep -Eoc "$pattern" "$DIR/plan.json" >/dev/null 2>&1 && grep -Eq "$pattern" "$DIR/plan.json"; then
    echo "LEAK ($label): pattern '$pattern' found in plan JSON"
    fail=1
  fi
}

# A real bootstrap token: cvat_ + exactly 43 base64url chars.
scan "bootstrap token" 'cvat_[A-Za-z0-9_-]{43}'
# A populated JWT_SECRET / VAULT_KEY / DATABASE password literal.
scan "JWT_SECRET literal"    'JWT_SECRET=[^"[:space:]]+'
scan "VAULT_KEY literal"     'VAULT_KEY=[^"[:space:]]{20,}'
scan "DB password in url"    'postgres://[a-z]+:[^@[:space:]]{10,}@'

if [ "$fail" -ne 0 ]; then
  echo "FAIL: secret literal(s) present in terraform plan -json output."
  exit 1
fi
echo "OK: no secret literals in the fresh plan JSON."
