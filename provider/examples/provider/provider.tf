terraform {
  required_providers {
    clawvisor = {
      source = "clawvisor/clawvisor"
    }
  }
}

# Endpoint and API token may also be supplied via the CLAWVISOR_ENDPOINT and
# CLAWVISOR_API_TOKEN environment variables. Prefer the environment (or a
# secret manager) over committed values — the api_token is a sensitive,
# instance-admin credential.
provider "clawvisor" {
  endpoint  = "https://clawvisor.internal:25297"
  api_token = var.clawvisor_api_token # sensitive

  # org_id is Clawvisor Cloud only. When set, governance and org-scoped
  # resources route to /api/orgs/{org_id}/...; omit it for a self-hosted
  # (OSS, instance-scoped) server.
  # org_id = "org_abc123"
}

variable "clawvisor_api_token" {
  type      = string
  sensitive = true
}
