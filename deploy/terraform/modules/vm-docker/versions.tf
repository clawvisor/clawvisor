# Provider and Terraform version constraints for the vm-docker module.
#
# v1 is AWS-only (PRD §13, spec 03 "v1 scope" section). GCP is v1.1 — the
# `google` provider and all GCP resources were intentionally dropped, so the
# module carries a single-cloud interface with no `cloud` selector variable.
terraform {
  required_version = ">= 1.9.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}
