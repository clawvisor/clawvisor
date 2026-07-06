# `vm-docker` — single-VM Docker Clawvisor on AWS

One `terraform apply` stands up a TLS-serving, Docker-based Clawvisor on a
single EC2 instance backed by managed RDS Postgres, in a named security
posture, with a seeded bootstrap admin API token as a sensitive output.

**v1 is AWS-only.** GCP, the container-DB option, and the reference-vault
backends are deferred (PRD §13). There is no `cloud` variable.

## Usage

```hcl
module "clawvisor" {
  source = "github.com/clawvisor/clawvisor//deploy/terraform/modules/vm-docker"

  name        = "clawvisor"
  region      = "us-east-1"
  image       = "ghcr.io/clawvisor/clawvisor:v1.4.2" # pinned tag, never :latest
  posture     = "observe"
  public_fqdn = "clawvisor.example.com"
  acme_email  = "ops@example.com"

  # Deny-by-default. Set these to reach the deploy at all.
  agent_ingress_cidrs = ["203.0.113.0/24"] # laptops/CI egress → agent endpoint (443)
  admin_ingress_cidrs = ["198.51.100.10/32"] # VPN only → admin surface (8443)
}
```

The monorepo path (`//deploy/terraform/modules/vm-docker`) is the v1
distribution. A publish-only registry mirror is deferred to v1.1 (PRD §6).

## Inputs

| Name | Type | Default | Notes |
|---|---|---|---|
| `name` | string | `"clawvisor"` | resource name prefix |
| `region` | string | — (required) | AWS region |
| `image` | string | — (required) | full image ref incl. tag; `:latest`/untagged rejected |
| `posture` | string | `"observe"` | `observe`/`govern`/`contain`; rendered into config.yaml (preset knobs land with spec 02) |
| `db` | string | `"managed"` | v1 accepts only `"managed"` (RDS Postgres); `"container"` is v1.1 |
| `db_instance_class` | string | `"db.t4g.micro"` | RDS instance class |
| `db_allocated_storage` | number | `20` | RDS storage (GiB) |
| `vault_backend` | string | `"local"` | v1 accepts only `"local"`; reference backends ship with spec 10 |
| `jwt_secret_ref` | string | `""` | AWS Secrets Manager ARN; empty ⇒ generated on-instance. Never a literal |
| `vault_key_ref` | string | `""` | same semantics as `jwt_secret_ref` |
| `otel_endpoint` | string | `""` | OTLP endpoint; empty disables export (spec 01) |
| `public_fqdn` | string | — (required) | DNS name Caddy serves + ACME; must resolve to `server_ip` before first boot |
| `acme_email` | string | — (required) | Let's Encrypt registration |
| `agent_ingress_cidrs` | list(string) | `[]` | CIDRs allowed to the agent endpoint (443). Empty = deny all |
| `admin_ingress_cidrs` | list(string) | `[]` | CIDRs allowed to the admin surface (8443). Empty = deny all |
| `i_understand_public_exposure` | bool | `false` | must be `true` to accept `0.0.0.0/0`/`::/0` in either list |
| `backup_retention_days` | number | `7` | RDS retention + upgrade-snapshot pruning window |
| `instance_type` | string | `"t3.small"` | EC2 instance type |
| `max_users` | number | `0` | `0` ⇒ unset; >0 sets `MAX_USERS` |
| `extra_env` | map(string) | `{}` | merged last into compose env; no secret literals |
| `vpc_id`/`subnet_id`/`db_subnet_ids` | — | default VPC | optional placement overrides |
| `key_name` | string | `""` | optional break-glass SSH key; SSH is admin-CIDR-scoped, never world-open |

## Outputs

| Name | Sensitive | Value |
|---|---|---|
| `server_url` | no | `https://<public_fqdn>` |
| `bootstrap_admin_token` | **yes** | single-use `cvat_` instance-admin token (spec 05) |
| `install_commands` | no | per-harness `curl … | sh` one-liners |
| `provider_block` | no | ready-to-paste `provider "clawvisor"` block (attr is `api_token`) |
| `instance_id` | no | EC2 instance id |
| `server_ip` | no | public IP the DNS record must point at |
| `db_endpoint` | **yes** | connection string (contains the DB password) |
| `image_ssm_parameter` | no | SSM parameter that drives upgrades/rollback |

## Network exposure — deny by default

A security/governance console must not default to internet-open. Two trust
zones, both deny-by-default, gated at the security-group (port) level because
security groups are L3/L4 and cannot path-route:

- **Agent endpoint (443, `agent_ingress_cidrs`):** the LLM/proxy + installer
  surface (`/v1/*`, `/skill/install/*`). Caddy serves only those paths here.
- **Admin surface (8443, `admin_ingress_cidrs`):** dashboard + management API.
  Keep tighter — VPN/allowlist only.

Empty list = the port is not opened at all. A `0.0.0.0/0` or `::/0` entry in
either list fails `plan` unless `i_understand_public_exposure = true`.

### DNS for ACME

Caddy obtains a Let's Encrypt certificate for `public_fqdn`. That requires:

1. The `public_fqdn` DNS A/AAAA record resolving to the `server_ip` output.
2. Let's Encrypt being able to reach the instance for the challenge.

Two-step apply: run `terraform apply` once, read `server_ip`, create the DNS
record, then let Caddy issue (it retries automatically). Under strict
deny-by-default ingress, Let's Encrypt's validators cannot reach 443/80 unless
you either (a) temporarily widen `agent_ingress_cidrs` during issuance, or
(b) switch Caddy to the DNS-01 challenge (a v1.1 module option). Document your
choice in the calling config.

## Secrets & state (PRD §7 — hard requirements)

- `jwt_secret_ref`/`vault_key_ref` are **references**, never literals. When
  empty, the value is generated **on the instance** at first boot and written
  to Secrets Manager — the literal never enters Terraform state or the plan.
- `bootstrap_admin_token` and `db_endpoint` are `sensitive = true`. The
  bootstrap token is module-generated (only the module can guarantee spec 05's
  `cvat_ + 43 base64url` shape) and therefore transits state; its 24h expiry
  and burn-on-first-use bound the exposure.
- **Use an encrypted state backend.** Pipe secrets straight to storage:
  `terraform output -raw bootstrap_admin_token | your-secret-store put …`.
  Do not paste them.

## Bootstrap token (spec 05 contract)

The module generates `cvat_` + 43 base64url chars, writes it to the
`${name}-bootstrap-token` Secrets Manager entry, and mounts it into the
container as `CLAWVISOR_BOOTSTRAP_TOKEN`. On first boot the server (spec 05, ≥
the release that consumes this env var) mints a 24h, burn-on-first-use
instance-admin token whose secret equals this value, provided no non-revoked
instance-admin token exists yet. After first use the token is dead; the
Secrets Manager entry is then safe to delete.

> `bootstrap_admin_token` requires **server ≥ the release that consumes
> `CLAWVISOR_BOOTSTRAP_TOKEN`** (spec 05). Against older servers the output is
> populated but inert.

## Upgrade & rollback (honest, no remote-exec / no SSH)

`terraform apply` with a new `image` updates only the SSM parameter
`/clawvisor/<name>/image`. An on-instance **systemd timer**
(`clawvisor-deploy.timer`) polls it every ~2 minutes and, on change:

1. takes an **RDS snapshot** (`<name>-preupgrade-<ts>`) — a new image may run
   migrations,
2. prunes pre-upgrade snapshots older than `backup_retention_days`,
3. `docker compose pull && up -d` onto the new tag.

No instance replacement, no `user_data` re-run, no SSH from the Terraform
runner. Brief downtime during restart is accepted and expected (single VM).

**Rollback:** set `image` back to the previous tag and `apply` (the timer
redeploys), then, if a migration ran, restore the pre-upgrade snapshot:

```
aws rds restore-db-instance-from-db-snapshot \
  --db-instance-identifier clawvisor-db-restored \
  --db-snapshot-identifier clawvisor-preupgrade-<ts>
# repoint DATABASE_URL / swap the instance per your runbook
```

Restore is a documented manual step in v1, not automated.

## Testing

- `terraform test` (in this directory) runs the validation + render lanes with
  **no cloud credentials** (mocked AWS provider, real `random`).
- `../../examples/local-docker` renders the compose/Caddyfile/config from these
  same templates and boots them with `docker compose` as a VM stand-in.
- The keyed real-AWS lane (`.github/workflows/terraform-ci.yml`, manual) does a
  full apply → SSM-param upgrade → snapshot-exists → destroy.

## Out of scope (v1)

No Kubernetes/Helm, no multi-VM/HA, no container-DB, no GCP, no reference-vault
backends. See PRD §13/§14.
