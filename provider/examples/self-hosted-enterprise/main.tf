# End-to-end self-hosted enterprise flow, entirely in Terraform:
#   1. an instance-admin cvat_ token creates the org and mints a cvot_ org token
#   2. that cvot_ token configures the org's SSO + governance
# The cvat_ never touches org config; the cvot_ never touches the instance.

terraform {
  required_providers {
    clawvisor = { source = "clawvisor/clawvisor", version = "0.0.1" }
  }
}

variable "endpoint" { type = string }
variable "cvat_token" {
  type      = string
  sensitive = true # instance-admin bootstrap token (self-hosted)
}
variable "okta_issuer" { type = string }
variable "okta_client_id" { type = string }
variable "okta_client_secret" {
  type      = string
  sensitive = true
}

# ── Instance-admin provider: creates the org + mints its first org token ──
provider "clawvisor" {
  alias     = "admin"
  endpoint  = var.endpoint
  api_token = var.cvat_token
}

resource "clawvisor_org" "acme" {
  provider = clawvisor.admin
  name     = "Acme Corp"
  slug     = "acme"
}

resource "clawvisor_org_token" "terraform" {
  provider        = clawvisor.admin
  org_id          = clawvisor_org.acme.id
  name            = "terraform-ci"
  expires_in_days = 90 # rotate quarterly; omitting it mints a non-expiring token (not recommended)
}

# ── Org-scoped provider: uses the freshly-minted cvot_ to configure the org ──
provider "clawvisor" {
  alias     = "org"
  endpoint  = var.endpoint
  api_token = clawvisor_org_token.terraform.token
  org_id    = clawvisor_org.acme.id
}

resource "clawvisor_model_policy" "approved" {
  provider = clawvisor.org
  mode     = "allow"
  models   = ["anthropic/claude-3-5-haiku-latest", "anthropic/claude-3-5-sonnet-latest"]
}

resource "clawvisor_content_policy" "no_secrets" {
  provider      = clawvisor.org
  name          = "no-provider-secrets"
  pattern       = "sk-[a-zA-Z0-9]{20,}"
  pattern_kind  = "regex"
  action        = "block"
  block_message = "Blocked: a secret was detected in the prompt."
}

# SSO (Okta OIDC), configured by the cvot_ — JIT-provisions users on first login.
resource "clawvisor_sso_connection" "okta" {
  provider           = clawvisor.org
  kind               = "oidc"
  email_domain       = "acme.com"
  oidc_issuer        = var.okta_issuer
  oidc_client_id     = var.okta_client_id
  oidc_client_secret = var.okta_client_secret
  jit_provision      = true
  default_role       = "member"
}

output "org_id" { value = clawvisor_org.acme.id }
output "org_token_prefix" { value = clawvisor_org_token.terraform.token_prefix }
