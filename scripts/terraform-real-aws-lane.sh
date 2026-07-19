#!/usr/bin/env bash
# Keyed real-AWS lane for the vm-docker module (spec 03 Tests, AWS-only).
#
#   apply → health check → SSM-param upgrade → snapshot exists → destroy
#
# Gated on AWS credentials; the terraform-ci.yml workflow only invokes this on
# a manual dispatch with real_cloud=true. It performs REAL, billable AWS
# operations. Resources are tagged clawvisor:managed=true / clawvisor-ci for a
# sweeper to reclaim on failure.
#
# Required env (set by the workflow from repo secrets/vars):
#   AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
#   TF_VAR_region, TF_VAR_public_fqdn, TF_VAR_acme_email, TF_VAR_office_cidrs
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -z "${AWS_ACCESS_KEY_ID:-}" ]; then
  echo "No AWS credentials; skipping real-AWS lane."
  exit 0
fi

DIR=deploy/terraform/examples/aws-minimal
REGION="${TF_VAR_region:?set TF_VAR_region}"
NAME=clawvisor
IMG_V1="ghcr.io/clawvisor/clawvisor:v1.4.2"
IMG_V2="ghcr.io/clawvisor/clawvisor:v1.4.3"

cleanup() {
  echo "== destroy =="
  terraform -chdir="$DIR" destroy -auto-approve -no-color || \
    echo "WARNING: destroy failed; resources tagged clawvisor:managed for the sweeper"
}
trap cleanup EXIT

terraform -chdir="$DIR" init -input=false -no-color

echo "== apply (image $IMG_V1) =="
# The example pins the image internally; override via the module by editing or
# a -var if the example exposes one. Here we drive upgrade through the module's
# SSM parameter directly (the honest upgrade lever), so apply establishes it.
terraform -chdir="$DIR" apply -auto-approve -input=false -no-color

SERVER_URL="$(terraform -chdir="$DIR" output -raw server_url)"
SERVER_IP="$(terraform -chdir="$DIR" output -raw server_ip)"
echo "server_url=$SERVER_URL server_ip=$SERVER_IP"
echo "NOTE: the public_fqdn A record must resolve to $SERVER_IP for ACME."

SSM_PARAM="/clawvisor/${NAME}/image"

echo "== health check (poll /health via the agent endpoint) =="
# TLS verification is intentionally ON: the poll only passes once the ACME
# certificate for public_fqdn is issued and valid, which is part of "healthy".
deadline=$(( $(date +%s) + 600 ))
until curl -fsS "${SERVER_URL}/health" >/dev/null 2>&1; do
  [ "$(date +%s)" -ge "$deadline" ] && { echo "FAIL: /health never returned 200"; exit 1; }
  sleep 15
done
echo "PASS: /health 200"

echo "== upgrade: bump the SSM image parameter to $IMG_V2 =="
# The upgrade lever: set the desired image. In the module, `terraform apply`
# with a new `image` does this; here we set it directly to exercise the
# on-instance timer without editing the example.
aws ssm put-parameter --region "$REGION" --name "$SSM_PARAM" \
  --type String --overwrite --value "$IMG_V2"

echo "== assert a pre-upgrade RDS snapshot appears =="
deadline=$(( $(date +%s) + 600 ))
until aws rds describe-db-snapshots --region "$REGION" \
        --db-instance-identifier "${NAME}-db" --snapshot-type manual \
        --query "DBSnapshots[?starts_with(DBSnapshotIdentifier, '${NAME}-preupgrade-')].DBSnapshotIdentifier" \
        --output text | grep -q "${NAME}-preupgrade-"; do
  [ "$(date +%s)" -ge "$deadline" ] && { echo "FAIL: no pre-upgrade snapshot within 10m"; exit 1; }
  sleep 20
done
echo "PASS: pre-upgrade snapshot exists"

echo "== assert the DEPLOYED image advanced to $IMG_V2 (read .deployed-image via SSM) =="
# A /health re-poll only proves the instance is up — it can be stuck on the
# old-but-healthy image. Read the reconciler's state file over SSM RunShell and
# assert it actually advanced, so a reconcile that never fired is caught.
INSTANCE_ID="$(terraform -chdir="$DIR" output -raw instance_id)"
deadline=$(( $(date +%s) + 600 ))
while true; do
  cmd_id="$(aws ssm send-command --region "$REGION" \
    --instance-ids "$INSTANCE_ID" \
    --document-name "AWS-RunShellScript" \
    --parameters 'commands=["cat /etc/clawvisor/.deployed-image"]' \
    --query 'Command.CommandId' --output text)"
  sleep 5
  deployed="$(aws ssm get-command-invocation --region "$REGION" \
    --command-id "$cmd_id" --instance-id "$INSTANCE_ID" \
    --query 'StandardOutputContent' --output text 2>/dev/null | tr -d '[:space:]')"
  [ "$deployed" = "$IMG_V2" ] && break
  [ "$(date +%s)" -ge "$deadline" ] && { echo "FAIL: .deployed-image never advanced to $IMG_V2 (last: '$deployed')"; exit 1; }
  sleep 15
done
echo "PASS: .deployed-image advanced to $IMG_V2"

echo "== confirm the server is healthy on the new image =="
curl -fsS "${SERVER_URL}/health" >/dev/null 2>&1 && echo "PASS: /health 200 post-upgrade" || { echo "FAIL: server unhealthy after upgrade"; exit 1; }

echo "ALL GOOD (destroy runs on trap exit)"
