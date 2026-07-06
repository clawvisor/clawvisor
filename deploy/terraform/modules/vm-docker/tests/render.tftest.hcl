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

run "admin_url_is_management_endpoint_on_8443" {
  command = apply

  assert {
    condition     = output.admin_url == "https://clawvisor.example.com:8443"
    error_message = "admin_url must be the management endpoint on the admin port (8443)"
  }

  assert {
    condition     = output.server_url == "https://clawvisor.example.com"
    error_message = "server_url must remain the agent endpoint on 443 (unchanged semantics)"
  }

  assert {
    condition     = can(regex("endpoint  = \"https://clawvisor.example.com:8443\"", output.provider_block))
    error_message = "provider_block endpoint must equal admin_url (8443) — no drift from the management endpoint"
  }
}

run "install_commands_match_installer_target_table" {
  command = apply

  # Source of truth: internal/api/handlers/installer.go installerTargets.
  # claude-code/codex are shell-canonical (.sh, pipe to sh).
  assert {
    condition     = output.install_commands["claude-code"] == "curl -fsSL https://clawvisor.example.com/skill/install/claude-code.sh | sh"
    error_message = "claude-code must be the .sh pipe-to-sh one-liner"
  }

  assert {
    condition     = output.install_commands["codex"] == "curl -fsSL https://clawvisor.example.com/skill/install/codex.sh | sh"
    error_message = "codex must be the .sh pipe-to-sh one-liner"
  }

  # hermes/openclaw are markdown-canonical (.md); .sh 404s, so no pipe-to-sh.
  assert {
    condition     = output.install_commands["hermes"] == "assisted install — open this URL: https://clawvisor.example.com/skill/install/hermes.md"
    error_message = "hermes must be the assisted-install .md URL, not a pipe-to-sh (hermes.sh 404s)"
  }

  assert {
    condition     = output.install_commands["openclaw"] == "assisted install — open this URL: https://clawvisor.example.com/skill/install/openclaw.md"
    error_message = "openclaw must be the assisted-install .md URL, not a pipe-to-sh (openclaw.sh 404s)"
  }

  assert {
    condition     = !can(regex("hermes.sh|openclaw.sh", join(" ", values(output.install_commands))))
    error_message = "no install command may reference the non-existent hermes.sh/openclaw.sh shell installers"
  }
}

run "reference_allowlist_empty_by_default_fails_closed" {
  command = apply

  assert {
    condition     = !can(regex("VAULT_REFERENCE_ALLOWLIST", local.compose_rendered))
    error_message = "an empty reference_allowlist must omit VAULT_REFERENCE_ALLOWLIST so the server fails closed"
  }
}

run "reference_allowlist_populated_renders_csv" {
  command = apply

  variables {
    reference_allowlist = [
      "arn:aws:secretsmanager:us-east-1:123456789012:secret:clawvisor/",
      "arn:aws:secretsmanager:us-east-1:123456789012:secret:shared/",
    ]
  }

  assert {
    condition     = can(regex("VAULT_REFERENCE_ALLOWLIST: \"arn:aws:secretsmanager:us-east-1:123456789012:secret:clawvisor/,arn:aws:secretsmanager:us-east-1:123456789012:secret:shared/\"", local.compose_rendered))
    error_message = "a populated reference_allowlist must render as the comma-joined VAULT_REFERENCE_ALLOWLIST env"
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

run "config_carries_contain_gate_when_enabled" {
  command = apply

  variables {
    posture              = "contain"
    experimental_contain = true
  }

  assert {
    condition     = can(regex("posture: \"contain\"", local.config_rendered))
    error_message = "rendered config must carry the active posture key for contain"
  }

  assert {
    condition     = can(regex("experimental_contain: true", local.config_rendered))
    error_message = "rendered config must set experimental_contain: true so the server accepts posture: contain"
  }
}

run "config_omits_contain_gate_by_default" {
  command = apply

  variables {
    posture = "observe"
  }

  assert {
    condition     = !can(regex("experimental_contain: true", local.config_rendered))
    error_message = "non-contain deploys must not emit the experimental_contain gate"
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
