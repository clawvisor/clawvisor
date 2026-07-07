# Plan-only render tests against mock providers (no credentials). Assert that
# the rendered compose/config carry the expected contract and that generated
# credentials have the shape spec 05 requires. The "no secret literal in the
# plan JSON" check is enforced separately by the CI script (terraform-ci.yml)
# on `terraform show -json`.

# Mock AWS only; `random` runs for real so generated values (bootstrap token,
# db password) are real and their shape can be asserted. Render blocks use
# command = apply so computed random values resolve.
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
  image               = "ghcr.io/clawvisor/clawvisor:v1.4.2"
  public_fqdn         = "clawvisor.example.com"
  acme_email          = "ops@example.com"
  agent_ingress_cidrs = ["203.0.113.0/24"]
  admin_ingress_cidrs = ["198.51.100.0/24"]
  otel_endpoint       = "otel.example.com:4317"
}

run "compose_uses_postgres_driver" {
  command = apply

  assert {
    condition     = can(regex("DATABASE_DRIVER: \"postgres\"", local.compose_rendered))
    error_message = "rendered compose must set DATABASE_DRIVER=postgres"
  }
}

run "config_carries_posture_and_proxy_lite" {
  command = apply

  variables {
    posture = "observe"
  }

  assert {
    condition     = can(regex("posture = \"observe\"", local.config_rendered))
    error_message = "rendered config must record the posture"
  }

  assert {
    condition     = can(regex("proxy_lite:\\s*\\n\\s*enabled: true", local.config_rendered))
    error_message = "rendered config must explicitly enable proxy_lite (writer-side flip, never Default())"
  }
}

run "config_wires_otel_when_endpoint_set" {
  command = apply

  assert {
    condition     = can(regex("endpoint: \"otel.example.com:4317\"", local.config_rendered))
    error_message = "otel_endpoint must be wired into the observability config"
  }
}

run "bootstrap_token_matches_spec05_format" {
  command = apply

  assert {
    condition     = can(regex("^cvat_[A-Za-z0-9_-]{43}$", local.bootstrap_token))
    error_message = "bootstrap token must match spec 05's ^cvat_[A-Za-z0-9_-]{43}$"
  }
}

run "image_ssm_parameter_holds_pinned_tag" {
  command = apply

  assert {
    condition     = aws_ssm_parameter.image.value == "ghcr.io/clawvisor/clawvisor:v1.4.2"
    error_message = "the SSM image parameter must hold the pinned image tag (the upgrade lever)"
  }
}

run "cloudinit_pins_and_verifies_docker_compose" {
  command = apply

  assert {
    condition     = can(regex("releases/download/v2.29.7/docker-compose-linux", local.user_data))
    error_message = "cloud-init must download a pinned docker compose release tag"
  }

  assert {
    condition     = !can(regex("releases/latest/download", local.user_data))
    error_message = "cloud-init must NOT pull docker compose from the floating latest release"
  }

  assert {
    condition     = can(regex("sha256sum -c", local.user_data))
    error_message = "cloud-init must checksum-verify the docker compose binary before install"
  }
}

run "deploy_script_gates_and_hardens_reconcile" {
  command = apply

  # Snapshot failure must abort, not continue (README snapshot-before-restart
  # guarantee).
  assert {
    condition     = can(regex("snapshot failed; aborting upgrade", local.deploy_rendered))
    error_message = "deploy script must abort the upgrade when the pre-upgrade snapshot fails"
  }

  assert {
    condition     = !can(regex("snapshot failed; continuing", local.deploy_rendered))
    error_message = "deploy script must not continue past a failed snapshot"
  }

  # pull/up is gated on change: the no-op else branch proves the guard wraps
  # the pull/up path (steady-state timer fires no longer restart the stack).
  assert {
    condition     = can(regex("already at .*nothing to do", local.deploy_rendered))
    error_message = "deploy script must gate pull/up on an image change (a no-op branch for steady state)"
  }
}

run "admin_url_output_matches_pr_body" {
  command = apply

  assert {
    condition     = output.admin_url == "https://clawvisor.example.com:8443"
    error_message = "admin_url output must expose the admin surface on the admin port (PR body: :8443)"
  }
}

run "accepts_kms_key_arn_for_cmk_refs" {
  command = apply

  variables {
    kms_key_arn = "arn:aws:kms:us-east-1:123456789012:key/abcd1234-ab12-cd34-ef56-abcdef123456"
  }
}
