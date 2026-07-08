# local-docker — the deterministic lane's stand-in for a VM (spec 03 Tests).
#
# Reuses ONLY the module's compose + Caddyfile + config templates (no cloud
# resources) and renders them to disk so `docker compose` can boot the exact
# rendered artifacts. A local postgres overlay + a generated secrets file
# replace the RDS/Secrets-Manager plumbing that the real module provides on a
# VM. Run `./run.sh` to boot and assert /health returns 200.
#
# This proves the rendering logic and the compose/Caddyfile contract without
# any AWS credentials.

terraform {
  required_version = ">= 1.9.0"
  required_providers {
    local = {
      source  = "hashicorp/local"
      version = "~> 2.5"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

variable "public_fqdn" {
  type    = string
  default = "localhost"
}

variable "image" {
  type    = string
  default = "clawvisor:local"
}

locals {
  module_dir = "${path.module}/../../modules/vm-docker"
  out_dir    = "${path.module}/rendered"

  compose = templatefile("${local.module_dir}/templates/docker-compose.yaml.tftpl", {
    app_env          = { AUTH_MODE = "password" }
    secrets_env_file = "./secrets.env"
    admin_port       = 8443
    vault_backend    = "local"
  })

  caddyfile = templatefile("${local.module_dir}/templates/Caddyfile.tftpl", {
    public_fqdn = var.public_fqdn
    acme_email  = "dev@example.com"
    admin_port  = 8443
  })

  config = templatefile("${local.module_dir}/templates/config.yaml.tftpl", {
    public_fqdn          = var.public_fqdn
    otel_endpoint        = ""
    posture              = "observe"
    experimental_contain = false
  })
}

# A local, module-format bootstrap token so the rendered secrets file matches
# what the VM path produces (cvat_ + 43 base64url chars).
resource "random_password" "bootstrap" {
  length           = 43
  special          = true
  override_special = "_-"
  min_special      = 0
}

resource "random_password" "jwt" {
  length  = 48
  special = false
}

resource "random_bytes" "vault_key" {
  length = 32
}

resource "local_file" "compose" {
  filename = "${local.out_dir}/docker-compose.yaml"
  content  = local.compose
}

resource "local_file" "caddyfile" {
  filename = "${local.out_dir}/Caddyfile"
  content  = local.caddyfile
}

resource "local_file" "config" {
  filename = "${local.out_dir}/config.yaml"
  content  = local.config
}

# Local overlay: a postgres sidecar (the VM path uses managed RDS) plus a
# bind mount of the rendered config into the app container.
resource "local_file" "override" {
  filename = "${local.out_dir}/docker-compose.override.yaml"
  content  = <<-EOT
    services:
      postgres:
        image: postgres:16-alpine
        environment:
          POSTGRES_USER: clawvisor
          POSTGRES_PASSWORD: clawvisor
          POSTGRES_DB: clawvisor
        healthcheck:
          test: ["CMD-SHELL", "pg_isready -U clawvisor"]
          interval: 5s
          timeout: 5s
          retries: 10
      app:
        depends_on:
          postgres:
            condition: service_healthy
        ports:
          - "25297:25297"
        volumes:
          - ./config.yaml:/etc/clawvisor/config.yaml:ro
      caddy:
        # Local runs hit the app directly; skip TLS/ACME in the deterministic
        # target by not starting caddy (the compose+Caddyfile still render and
        # `docker compose config` still validates them).
        profiles: ["tls"]
  EOT
}

resource "local_sensitive_file" "secrets" {
  filename = "${local.out_dir}/secrets.env"
  content  = <<-EOT
    DATABASE_URL=postgres://clawvisor:clawvisor@postgres:5432/clawvisor
    JWT_SECRET=${random_password.jwt.result}
    VAULT_KEY=${random_bytes.vault_key.base64}
    CLAWVISOR_BOOTSTRAP_TOKEN=cvat_${random_password.bootstrap.result}
  EOT
}

resource "local_file" "compose_env" {
  filename = "${local.out_dir}/compose.env"
  content  = "clawvisor_image=${var.image}\n"
}
