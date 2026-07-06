# Two-phase bootstrap apply (AGENT-GUIDE §3).
#
# On a fresh instance the only credential that exists is the deploy module's
# bootstrap token — which creates a chicken-and-egg the naive single-apply
# hits (the provider block can't read an output that doesn't exist until apply
# runs). Do it in two phases.
#
# ── Phase 1 — stand up the instance ─────────────────────────────────────────
#
#   terraform apply -target=module.clawvisor
#
# The vm-docker module (spec 03) generates a correctly-formatted `cvat_`
# bootstrap token, delivers it to the server via its secret manager, and
# exposes it as the sensitive output `bootstrap_admin_token`. That token is
# short-lived (24h) and first-use-burns-it: it exists only to mint your real
# token.
#
# ── Phase 2 — mint a durable token, then apply everything ────────────────────
#
#   1. Point the provider at the bootstrap token FOR THIS ONE STEP ONLY, via
#      the environment (never in committed tfvars):
#
#        export CLAWVISOR_API_TOKEN="$(terraform output -raw \
#          -state=... bootstrap_admin_token)"
#        terraform apply -target=clawvisor_api_token.terraform
#
#   2. Read the minted token into your durable secret store and set
#      var.clawvisor_api_token from there for all future applies. Minting that
#      first scoped token BURNS the bootstrap token automatically — there is
#      nothing to remember to revoke.
#
#   3. Unset CLAWVISOR_API_TOKEN (or switch it to the durable token) and run a
#      full `terraform apply` for the rest of your configuration.

terraform {
  required_providers {
    clawvisor = {
      source = "clawvisor/clawvisor"
    }
  }
}

variable "clawvisor_endpoint" {
  type    = string
  default = "https://clawvisor.internal:25297"
}

# For all steady-state applies this is the durable instance-admin token minted
# below. During phase 2 step 1 it is left empty and CLAWVISOR_API_TOKEN carries
# the bootstrap token instead.
variable "clawvisor_api_token" {
  type      = string
  sensitive = true
  default   = ""
}

provider "clawvisor" {
  endpoint  = var.clawvisor_endpoint
  api_token = var.clawvisor_api_token # falls back to CLAWVISOR_API_TOKEN
}

# The durable, instance-admin credential Terraform uses after bootstrap. Its
# creation (with the provider temporarily authenticated by the bootstrap token)
# burns the bootstrap token.
resource "clawvisor_api_token" "terraform" {
  name  = "terraform"
  scope = "instance-admin"
}

output "terraform_api_token" {
  description = "Durable instance-admin token; store in your secret manager."
  value       = clawvisor_api_token.terraform.token
  sensitive   = true
}

# ── Everything else, applied in steady state once the durable token is set ───

resource "clawvisor_vault_entry" "anthropic" {
  service_id = "anthropic"
  value      = var.anthropic_api_key
}

variable "anthropic_api_key" {
  type      = string
  sensitive = true
  default   = ""
}

resource "clawvisor_agent" "ci" {
  name        = "github-actions"
  description = "CI runner service identity"
}

output "ci_agent_token" {
  value     = clawvisor_agent.ci.token
  sensitive = true
}
