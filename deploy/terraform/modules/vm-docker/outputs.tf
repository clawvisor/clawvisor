output "server_url" {
  description = "Base URL of the Clawvisor server (agent endpoint on 443)."
  value       = "https://${var.public_fqdn}"
}

output "bootstrap_admin_token" {
  description = "Single-use instance-admin bootstrap token (spec 05 contract: cvat_ + 43 base64url chars, 24h expiry, burns on first mint). Requires server >= the release that consumes CLAWVISOR_BOOTSTRAP_TOKEN. Pipe `terraform output -raw bootstrap_admin_token` straight into your secret store — do not paste it."
  value       = local.bootstrap_token
  sensitive   = true
}

output "admin_url" {
  description = "Admin dashboard + management API URL (admin surface on the admin port, gated to admin_ingress_cidrs)."
  value       = "https://${var.public_fqdn}:${local.admin_port}"
}

output "install_commands" {
  description = "Per-harness agent install one-liners (served by internal/api/handlers/installer.go at the agent endpoint)."
  value = {
    claude-code = "curl -fsSL https://${var.public_fqdn}/skill/install/claude-code.sh | sh"
    codex       = "curl -fsSL https://${var.public_fqdn}/skill/install/codex.sh | sh"
    hermes      = "curl -fsSL https://${var.public_fqdn}/skill/install/hermes.sh | sh"
    openclaw    = "curl -fsSL https://${var.public_fqdn}/skill/install/openclaw.sh | sh"
  }
}

output "provider_block" {
  description = "Ready-to-paste Terraform provider block for terraform-provider-clawvisor (spec 06b). The credential attribute is `api_token` (never `token`). Wire it to the sensitive bootstrap_admin_token output rather than pasting the literal."
  value       = <<-EOT
    provider "clawvisor" {
      endpoint  = "https://${var.public_fqdn}:${local.admin_port}"
      api_token = var.clawvisor_api_token # e.g. terraform output -raw bootstrap_admin_token
    }
  EOT
}

output "instance_id" {
  description = "EC2 instance id of the application VM."
  value       = aws_instance.app.id
}

output "server_ip" {
  description = "Public IP the public_fqdn DNS A/AAAA record must point at. Create that record before first apply completes so the ACME challenge can issue a certificate."
  value       = aws_instance.app.public_ip
}

output "db_endpoint" {
  description = "Managed-DB connection string. Contains the database password (managed mode) — treat as a credential."
  value       = "postgres://${local.db_user}:${random_password.db.result}@${aws_db_instance.this.address}:5432/${local.db_name}"
  sensitive   = true
}

output "image_ssm_parameter" {
  description = "SSM parameter that holds the desired image tag. `terraform apply` with a new `image` updates it; the on-instance timer reconciles. Also the manual rollback lever (reset to the previous tag)."
  value       = aws_ssm_parameter.image.name
}
