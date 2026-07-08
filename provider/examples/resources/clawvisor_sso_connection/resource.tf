# clawvisor_sso_connection configures a per-org SSO connection (SAML or OIDC).
# Enterprise / Clawvisor Cloud only: it requires the server's `sso` capability
# and the provider's org_id to be set — it errors cleanly on an OSS deployment.

variable "clawvisor_api_token" {
  type      = string
  sensitive = true
}

variable "okta_client_id" {
  type = string
}

variable "okta_client_secret" {
  type      = string
  sensitive = true
}

provider "clawvisor" {
  endpoint = "https://clawvisor.example.com:8443"
  api_token = var.clawvisor_api_token # sensitive
  org_id   = "org_abc123"
}

# Okta via OIDC (the fastest hookup):
resource "clawvisor_sso_connection" "okta" {
  kind               = "oidc"
  email_domain       = "acme.com"
  oidc_issuer        = "https://acme.okta.com"
  oidc_client_id     = var.okta_client_id
  oidc_client_secret = var.okta_client_secret # sensitive; encrypted server-side, never returned
  jit_provision      = true
  default_role       = "member" # "owner" is not allowed
  sso_team_attribute = "groups"
  enabled            = true
}

# SAML alternative:
# resource "clawvisor_sso_connection" "okta_saml" {
#   kind                 = "saml"
#   email_domain         = "acme.com"
#   saml_entity_id       = "http://www.okta.com/exk..."
#   saml_sso_url         = "https://acme.okta.com/app/.../sso/saml"
#   saml_certificate_pem = file("${path.module}/okta.pem")
# }
