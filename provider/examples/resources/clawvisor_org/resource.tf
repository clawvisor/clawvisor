# Provision an org on a self-hosted deployment using an instance-admin (cvat_)
# token. Configure the provider with a cvat_ token and NO org_id — the
# /api/admin/orgs surface is instance-scoped and self-hosted-only.
provider "clawvisor" {
  endpoint  = "https://clawvisor.internal"
  api_token = var.clawvisor_admin_token # a cvat_ instance-admin token
}

resource "clawvisor_org" "acme" {
  name = "Acme"
  slug = "acme"
}

variable "clawvisor_admin_token" {
  type      = string
  sensitive = true
}
