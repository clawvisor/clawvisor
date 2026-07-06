# Variable-validation tests. No credentials: every run block uses
# command = plan and expects the plan to FAIL at variable validation, so no
# provider calls happen. A minimal valid variable set is supplied per block;
# the one bad variable under test triggers the expected failure.

# Mock AWS only (no credentials). `random` is a local provider and runs for
# real. mock_data feeds the data sources so plan reaches variable validation
# instead of erroring on empty default-VPC lookups.
mock_provider "aws" {
  mock_data "aws_vpc" {
    defaults = { id = "vpc-test" }
  }
  mock_data "aws_subnets" {
    defaults = { ids = ["subnet-a", "subnet-b"] }
  }
  mock_data "aws_ami" {
    defaults = { id = "ami-test" }
  }
  mock_data "aws_iam_policy_document" {
    defaults = { json = "{}" }
  }
}

variables {
  region              = "us-east-1"
  public_fqdn         = "clawvisor.example.com"
  acme_email          = "ops@example.com"
  agent_ingress_cidrs = ["10.0.0.0/8"]
  admin_ingress_cidrs = ["10.0.0.0/8"]
}

run "rejects_latest_image" {
  command = plan

  variables {
    image = "ghcr.io/clawvisor/clawvisor:latest"
  }

  expect_failures = [var.image]
}

run "rejects_untagged_image" {
  command = plan

  variables {
    image = "ghcr.io/clawvisor/clawvisor"
  }

  expect_failures = [var.image]
}

run "accepts_pinned_image" {
  command = plan

  variables {
    image = "ghcr.io/clawvisor/clawvisor:v1.4.2"
  }
}

run "rejects_vault_backend_gcp" {
  command = plan

  variables {
    image         = "ghcr.io/clawvisor/clawvisor:v1.4.2"
    vault_backend = "gcp"
  }

  expect_failures = [var.vault_backend]
}

run "rejects_db_container" {
  command = plan

  variables {
    image = "ghcr.io/clawvisor/clawvisor:v1.4.2"
    db    = "container"
  }

  expect_failures = [var.db]
}

run "rejects_invalid_posture" {
  command = plan

  variables {
    image   = "ghcr.io/clawvisor/clawvisor:v1.4.2"
    posture = "lockdown"
  }

  expect_failures = [var.posture]
}

run "rejects_contain_without_experimental_flag" {
  command = plan

  variables {
    image   = "ghcr.io/clawvisor/clawvisor:v1.4.2"
    posture = "contain"
  }

  expect_failures = [var.experimental_contain]
}

run "accepts_contain_with_experimental_flag" {
  command = plan

  variables {
    image                = "ghcr.io/clawvisor/clawvisor:v1.4.2"
    posture              = "contain"
    experimental_contain = true
  }
}

run "rejects_public_agent_cidr_without_ack" {
  command = plan

  variables {
    image                        = "ghcr.io/clawvisor/clawvisor:v1.4.2"
    agent_ingress_cidrs          = ["0.0.0.0/0"]
    i_understand_public_exposure = false
  }

  expect_failures = [var.agent_ingress_cidrs]
}

run "rejects_public_admin_cidr_without_ack" {
  command = plan

  variables {
    image                        = "ghcr.io/clawvisor/clawvisor:v1.4.2"
    admin_ingress_cidrs          = ["0.0.0.0/0"]
    i_understand_public_exposure = false
  }

  expect_failures = [var.admin_ingress_cidrs]
}

run "accepts_public_cidr_with_ack" {
  command = plan

  variables {
    image                        = "ghcr.io/clawvisor/clawvisor:v1.4.2"
    agent_ingress_cidrs          = ["0.0.0.0/0"]
    i_understand_public_exposure = true
  }
}
