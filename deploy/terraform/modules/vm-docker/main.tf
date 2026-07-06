# vm-docker — cloud-agnostic wiring, secret generation, and locals.
#
# AWS resources live in aws.tf; cloud-init rendering in cloudinit.tf; the
# provider/version constraints in versions.tf.

locals {
  # Admin surface listens on a second port; the security group gates it to
  # admin_ingress_cidrs while 443 (agent endpoint) is gated to
  # agent_ingress_cidrs. Caddy path-splits the two zones (see Caddyfile).
  admin_port = 8443

  db_name = "clawvisor"
  db_user = "clawvisor"

  # Non-secret app env merged into the compose `environment:` map. Secret
  # values (DATABASE_URL, JWT_SECRET, VAULT_KEY, CLAWVISOR_BOOTSTRAP_TOKEN)
  # travel via the env_file written at boot from Secrets Manager, NOT here.
  app_env = merge(
    { AUTH_MODE = "password" },
    var.max_users > 0 ? { MAX_USERS = tostring(var.max_users) } : {},
    var.extra_env,
  )

  # Bootstrap admin token (05-lite contract). The MODULE is the single owner
  # of generation — only it can guarantee spec 05's ^cvat_[A-Za-z0-9_-]{43}$
  # shape. random_password.result is sensitive, so it is redacted in
  # `plan -json`; it is exposed only through the sensitive
  # bootstrap_admin_token output and the sensitive Secrets Manager entry.
  bootstrap_token = "cvat_${random_password.bootstrap.result}"

  # Whether the module generates JWT/VAULT on-instance (generated-mode) vs.
  # the operator supplying a Secrets Manager ARN (reference-mode). In
  # generated-mode the value is created ON THE INSTANCE at first boot and
  # written to Secrets Manager, so the literal never enters Terraform state
  # or the plan (PRD §7).
  jwt_generate       = var.jwt_secret_ref == ""
  vault_key_generate = var.vault_key_ref == ""

  jwt_secret_arn = var.jwt_secret_ref != "" ? var.jwt_secret_ref : aws_secretsmanager_secret.jwt[0].arn
  vault_key_arn  = var.vault_key_ref != "" ? var.vault_key_ref : aws_secretsmanager_secret.vault_key[0].arn

  # ARNs the instance profile must be granted read access to.
  read_secret_arns = compact([
    local.jwt_secret_arn,
    local.vault_key_arn,
    aws_secretsmanager_secret.bootstrap.arn,
    aws_secretsmanager_secret.db_url.arn,
  ])
}

# --- Bootstrap admin token (spec 05 contract) ------------------------------
# length 43 over the base64url alphabet: uppers+lowers+digits plus override
# specials "_-". No other special chars can appear, so the value always
# matches ^[A-Za-z0-9_-]{43}$ after the module prepends "cvat_".
resource "random_password" "bootstrap" {
  length           = 43
  special          = true
  override_special = "_-"
  min_special      = 0
  min_upper        = 0
  min_lower        = 0
  min_numeric      = 0
}

resource "aws_secretsmanager_secret" "bootstrap" {
  name = "${var.name}-bootstrap-token"
  # No ignore_changes: this is a rotate-able seed, not a permanent credential.
  # It is safe to delete after first boot (the token burns on first mint —
  # spec 05: 24h expiry + burn-on-first-use). See README rotation runbook.
  recovery_window_in_days = 0
}

resource "aws_secretsmanager_secret_version" "bootstrap" {
  secret_id     = aws_secretsmanager_secret.bootstrap.id
  secret_string = local.bootstrap_token
}

# --- JWT_SECRET (generated-mode only; literal never in state) --------------
resource "aws_secretsmanager_secret" "jwt" {
  count                   = local.jwt_generate ? 1 : 0
  name                    = "${var.name}-jwt-secret"
  recovery_window_in_days = 0
  # No secret_version: the instance generates the value at first boot and
  # PUTs it (see deploy.sh ensure_generated). Keeps the literal out of state.
}

# --- VAULT_KEY (generated-mode only; literal never in state) ---------------
resource "aws_secretsmanager_secret" "vault_key" {
  count                   = local.vault_key_generate ? 1 : 0
  name                    = "${var.name}-vault-key"
  recovery_window_in_days = 0
}

# --- DATABASE_URL ----------------------------------------------------------
# The managed-DB connection string (contains the RDS password) is stored in
# Secrets Manager and read at boot. The password is a random_password
# (sensitive → redacted in plan); it is in state, which the spec accepts:
# db_endpoint is a sensitive output documented to contain the password.
resource "random_password" "db" {
  length  = 30
  special = false # keep the value URL-safe so DATABASE_URL needs no escaping
}

resource "aws_secretsmanager_secret" "db_url" {
  name                    = "${var.name}-database-url"
  recovery_window_in_days = 0
}

resource "aws_secretsmanager_secret_version" "db_url" {
  secret_id     = aws_secretsmanager_secret.db_url.id
  secret_string = "postgres://${local.db_user}:${random_password.db.result}@${aws_db_instance.this.address}:5432/${local.db_name}"
}
