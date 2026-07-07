# Minimal AWS deploy of Clawvisor via the vm-docker module.
#
# Prereqs (see the module README):
#   - AWS credentials for the target account/region.
#   - A DNS zone you can create an A record in for var.public_fqdn.
#
# Two-step ACME: apply once to learn the instance IP (server_ip output),
# create the DNS A record pointing at it, then re-apply / let the cert issue.

terraform {
  required_version = ">= 1.9.0"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = var.region
}

variable "region" {
  type    = string
  default = "us-east-1"
}

variable "public_fqdn" {
  type = string
}

variable "acme_email" {
  type = string
}

variable "office_cidrs" {
  type        = list(string)
  description = "Your VPN/office/CI egress CIDRs. Both zones default to deny-all."
  default     = []
}

module "clawvisor" {
  source = "../../modules/vm-docker"

  name        = "clawvisor"
  region      = var.region
  image       = "ghcr.io/clawvisor/clawvisor:v1.4.2"
  posture     = "observe"
  public_fqdn = var.public_fqdn
  acme_email  = var.acme_email

  # Deny-by-default: set these to reach the deploy at all.
  agent_ingress_cidrs = var.office_cidrs
  admin_ingress_cidrs = var.office_cidrs
}

output "server_url" {
  value = module.clawvisor.server_url
}

output "server_ip" {
  value = module.clawvisor.server_ip
}

output "instance_id" {
  value = module.clawvisor.instance_id
}

output "install_commands" {
  value = module.clawvisor.install_commands
}

output "bootstrap_admin_token" {
  value     = module.clawvisor.bootstrap_admin_token
  sensitive = true
}
