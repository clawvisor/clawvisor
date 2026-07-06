# A secret stored in the Clawvisor vault (push mode). The value transits
# Terraform state — use an encrypted state backend and, where supported,
# ephemeral/write-only inputs. The server never returns the value.
resource "clawvisor_vault_entry" "anthropic" {
  service_id = "anthropic"
  value      = var.anthropic_api_key # sensitive
}

variable "anthropic_api_key" {
  type      = string
  sensitive = true
}
