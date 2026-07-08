# The org-shared Anthropic key, injected server-side in govern posture. Use
# clawvisor_llm_credential for provider keys (anthropic / openai / google) —
# clawvisor_vault_entry / clawvisor_vault_reference REJECT those reserved ids.

# Push mode: the literal key transits Terraform state (Sensitive) — use an
# encrypted state backend and, where supported, write-only/ephemeral inputs.
resource "clawvisor_llm_credential" "anthropic" {
  llm_provider = "anthropic"
  api_key      = var.anthropic_api_key # sensitive
}

variable "anthropic_api_key" {
  type      = string
  sensitive = true
}

# Reference mode: point at a secret in your own store (AWS/GCP). No secret ever
# enters Terraform state; Clawvisor resolves it at injection time only. The
# target must match the server's vault.reference_allowlist and the instance
# identity must have read access.
resource "clawvisor_llm_credential" "openai" {
  llm_provider = "openai"
  reference = {
    backend = "aws-sm"
    ref_id  = "arn:aws:secretsmanager:us-east-1:123456789012:secret:openai-org-key"
  }
}

# Agent-scoped override: preferred over the shared key for this one agent. The
# agent must be owned by the token's principal (a Terraform-managed
# clawvisor_agent is instance-owned, so an instance-admin token can set it).
resource "clawvisor_llm_credential" "anthropic_ci_agent" {
  llm_provider = "anthropic"
  agent_id     = clawvisor_agent.ci.id
  api_key      = var.anthropic_ci_api_key # sensitive
}

variable "anthropic_ci_api_key" {
  type      = string
  sensitive = true
}
