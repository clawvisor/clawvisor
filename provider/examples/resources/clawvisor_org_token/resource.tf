# Mint a cvot_ org-admin token for an org, using an instance-admin (cvat_)
# provider. The cvot_ then configures the org (SSO, governance) with no user
# login. The plaintext is returned once and stored in state (treat as secret).
resource "clawvisor_org" "acme" {
  name = "Acme"
  slug = "acme"
}

resource "clawvisor_org_token" "terraform" {
  org_id          = clawvisor_org.acme.id
  name            = "terraform"
  expires_in_days = 90 # omit for a non-expiring token
}

# Hand the minted cvot_ to a second provider alias to configure the org.
provider "clawvisor" {
  alias     = "org"
  endpoint  = "https://clawvisor.internal"
  api_token = clawvisor_org_token.terraform.token
  org_id    = clawvisor_org.acme.id
}
