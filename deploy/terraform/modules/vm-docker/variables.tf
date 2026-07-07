# Input variables for the vm-docker module.
#
# v1 is AWS-only. The `cloud` variable from earlier drafts is intentionally
# absent (PRD §13 / spec 03 "v1 scope"): there is one cloud, so there is no
# selector. `db` and `vault_backend` keep their variable form for forward
# compatibility but validate to the single v1-supported value each, so a
# `plan` that asks for a deferred option fails loudly instead of silently
# doing something else.

variable "name" {
  type        = string
  default     = "clawvisor"
  description = "Resource name prefix for every resource this module creates."

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,30}[a-z0-9]$", var.name))
    error_message = "name must be lowercase alphanumeric with hyphens, 2-32 chars, starting with a letter (used as an AWS resource name prefix)."
  }
}

variable "region" {
  type        = string
  description = "AWS region to deploy into, e.g. \"us-east-1\"."
}

variable "image" {
  type        = string
  description = "Full container image reference including an immutable tag, e.g. \"ghcr.io/clawvisor/clawvisor:v1.4.2\". The `latest` tag is rejected."

  validation {
    # A pinned tag is mandatory: `latest` makes upgrades non-deterministic and
    # makes rollback impossible. The upgrade mechanism keys entirely off this
    # tag changing (SSM parameter), so a floating tag defeats it.
    condition     = length(regexall(":latest$", var.image)) == 0 && length(regexall(":[^/]+$", var.image)) == 1
    error_message = "image must include an explicit non-latest tag (e.g. ghcr.io/clawvisor/clawvisor:v1.4.2); \"latest\" and untagged references are rejected."
  }
}

variable "posture" {
  type        = string
  default     = "observe"
  description = "Named security posture (PRD §4): observe | govern | contain. Rendered into the mounted config.yaml. NOTE: the concrete preset knobs come from spec 02; until it merges the module ships observe-only proxy-lite config and gates the govern/contain preset knobs behind a seam (see config.yaml.tftpl)."

  validation {
    condition     = contains(["observe", "govern", "contain"], var.posture)
    error_message = "posture must be one of: observe, govern, contain."
  }
}

variable "experimental_contain" {
  type        = bool
  default     = false
  description = "Gate for the experimental Contain posture (spec 09). posture = \"contain\" is refused at plan time unless this is true; when set the module writes experimental_contain: true into the rendered server config. Remove this variable (and the server-side config gate) in the release where CI runs the capability-parity lane required-blocking."

  # Cross-variable refusal (Terraform >= 1.9): contain requires the flag.
  validation {
    condition     = var.posture != "contain" || var.experimental_contain
    error_message = "posture = \"contain\" is experimental and must be opted into with experimental_contain = true until the capability-parity lane is required-blocking (spec 09)."
  }
}

variable "db" {
  type        = string
  default     = "managed"
  description = "Database mode. v1 supports only \"managed\" (RDS Postgres). \"container\" (postgres sidecar on a data volume) is deferred to v1.1."

  validation {
    condition     = var.db == "managed"
    error_message = "db must be \"managed\" in v1. The \"container\" sidecar option is deferred to v1.1 (spec 03 v1 scope)."
  }
}

variable "db_instance_class" {
  type        = string
  default     = "db.t4g.micro"
  description = "RDS instance class for the managed Postgres database."
}

variable "db_allocated_storage" {
  type        = number
  default     = 20
  description = "Allocated storage (GiB) for the managed Postgres database."
}

variable "vault_backend" {
  type        = string
  default     = "local"
  description = "Vault backend passed through to VAULT_BACKEND. v1 supports only \"local\" (AES-256-GCM in DB). The AWS Secrets Manager reference backend arrives with spec 10."

  validation {
    condition     = var.vault_backend == "local"
    error_message = "vault_backend must be \"local\" in v1. Reference backends (aws-sm) ship with spec 10; the \"gcp\" backend is not available in the AWS-only module."
  }
}

variable "reference_allowlist" {
  type        = list(string)
  default     = []
  description = "Operator allowlist of permitted vault-reference id prefixes (ARN / resource-name / path), wired to server config vault.reference_allowlist via the VAULT_REFERENCE_ALLOWLIST env (spec 10 confused-deputy control). A reference whose id does not begin with a listed prefix is rejected at create time. EMPTY (the default) disables reference creation entirely — fail-closed; the env is omitted so the server keeps an empty allowlist."

  # Entries are joined with commas into VAULT_REFERENCE_ALLOWLIST, so an empty
  # string, embedded whitespace, or an embedded comma would silently create a
  # bogus (or everything-matching) prefix. Reject all loudly at plan time.
  validation {
    condition = alltrue([
      for e in var.reference_allowlist : length(e) > 0 && !can(regex("[,\\s]", e))
    ])
    error_message = "reference_allowlist entries must be non-empty and contain no commas or whitespace (they are comma-joined into VAULT_REFERENCE_ALLOWLIST as reference-id prefixes)."
  }
}

variable "jwt_secret_ref" {
  type        = string
  default     = ""
  description = "ARN of an AWS Secrets Manager secret holding JWT_SECRET. Empty ⇒ the instance generates one at first boot and stores it in Secrets Manager. NEVER a literal secret value — literals would land in Terraform state."

  validation {
    condition     = var.jwt_secret_ref == "" || can(regex("^arn:aws[a-z-]*:secretsmanager:", var.jwt_secret_ref))
    error_message = "jwt_secret_ref must be empty or an AWS Secrets Manager ARN (arn:aws:secretsmanager:...). Do not pass a literal secret value."
  }
}

variable "vault_key_ref" {
  type        = string
  default     = ""
  description = "ARN of an AWS Secrets Manager secret holding VAULT_KEY. Same semantics as jwt_secret_ref; empty ⇒ generated on-instance."

  validation {
    condition     = var.vault_key_ref == "" || can(regex("^arn:aws[a-z-]*:secretsmanager:", var.vault_key_ref))
    error_message = "vault_key_ref must be empty or an AWS Secrets Manager ARN (arn:aws:secretsmanager:...). Do not pass a literal secret value."
  }
}

variable "otel_endpoint" {
  type        = string
  default     = ""
  description = "OTLP endpoint for observability export (spec 01), e.g. \"otel.example.com:4317\". Empty disables export."
}

variable "public_fqdn" {
  type        = string
  description = "DNS name Caddy serves and requests an ACME certificate for. A DNS A/AAAA record for this name MUST resolve to the instance public address (the server_ip output) before the ACME challenge runs, or certificate issuance fails. See README \"DNS for ACME\"."
}

variable "acme_email" {
  type        = string
  description = "Email used for Let's Encrypt/ACME account registration."

  validation {
    condition     = can(regex("^[^@ ]+@[^@ ]+\\.[^@ ]+$", var.acme_email))
    error_message = "acme_email must be a valid email address."
  }
}

variable "agent_ingress_cidrs" {
  type        = list(string)
  default     = []
  description = "CIDRs allowed to reach the agent LLM/proxy endpoint (/v1/*, /skill/install/*) on 443. Set to your VPN/office/CI egress ranges. Empty = deny all (the deploy is unreachable by agents until set — deliberate)."

  # Cross-variable validation requires Terraform >= 1.9 (see versions.tf).
  validation {
    condition = var.i_understand_public_exposure || length([
      for c in var.agent_ingress_cidrs : c if c == "0.0.0.0/0" || c == "::/0"
    ]) == 0
    error_message = "agent_ingress_cidrs contains a public CIDR (0.0.0.0/0 or ::/0). Internet-exposing the agent endpoint requires setting i_understand_public_exposure = true."
  }
}

variable "admin_ingress_cidrs" {
  type        = list(string)
  default     = []
  description = "CIDRs allowed to reach the admin dashboard + management API on the admin port. Keep tighter than agent_ingress_cidrs — VPN/allowlist only. Empty = deny all."

  # Cross-variable validation requires Terraform >= 1.9 (see versions.tf).
  validation {
    condition = var.i_understand_public_exposure || length([
      for c in var.admin_ingress_cidrs : c if c == "0.0.0.0/0" || c == "::/0"
    ]) == 0
    error_message = "admin_ingress_cidrs contains a public CIDR (0.0.0.0/0 or ::/0). Internet-exposing the admin surface requires setting i_understand_public_exposure = true (and is strongly discouraged for a security console)."
  }
}

variable "i_understand_public_exposure" {
  type        = bool
  default     = false
  description = "Must be explicitly true for the module to accept 0.0.0.0/0 (or ::/0) in either ingress list. Internet-exposing a security console is a conscious choice; otherwise plan fails validation."
}

variable "backup_retention_days" {
  type        = number
  default     = 7
  description = "Managed-DB backup retention (days). Also bounds how many upgrade snapshots are kept before pruning."

  validation {
    condition     = var.backup_retention_days >= 1 && var.backup_retention_days <= 35
    error_message = "backup_retention_days must be between 1 and 35 (RDS automated-backup limit)."
  }
}

variable "instance_type" {
  type        = string
  default     = "t3.small"
  description = "EC2 instance type for the application VM. Must be an x86_64 (Intel/AMD) family: the AMI is pinned to al2023 x86_64, so a Graviton (arm64) type would plan cleanly then fail at launch with an architecture mismatch."

  # The AMI filter is al2023-ami-*-x86_64 (aws.tf). Reject arm64/Graviton
  # families (a1.*, and any family whose suffix has a digit followed by 'g'
  # like t4g/m7g/c6gd/r7g/x2gd/g5g) so the arch mismatch is caught at plan.
  validation {
    condition     = !can(regex("(^a1\\.)|([0-9]+g[a-z]*\\.)", var.instance_type))
    error_message = "instance_type must be an x86_64 family; Graviton/arm64 types (a1.*, t4g.*, m7g.*, c6gd.*, r7g.*, g5g.*, …) are rejected because the AMI is pinned to al2023 x86_64."
  }
}

variable "max_users" {
  type        = number
  default     = 0
  description = "0 ⇒ unset (no cap, PRD §5). >0 sets MAX_USERS in the container env."
}

variable "extra_env" {
  type        = map(string)
  default     = {}
  description = "Escape hatch: extra environment variables merged LAST into the compose app env. Do not put secret literals here — they land in Terraform state."

  # extra_env is merged LAST into the compose environment: map, which outranks
  # the Secrets-Manager env_file. Rejecting the reserved secret keys here stops
  # a caller from shadowing the vaulted DATABASE_URL/JWT_SECRET/VAULT_KEY/
  # bootstrap token with a plaintext value that would land in state/user_data
  # (PRD §7 hard rules).
  validation {
    condition = length([
      for k in keys(var.extra_env) : k
      if contains(["DATABASE_URL", "JWT_SECRET", "VAULT_KEY", "CLAWVISOR_BOOTSTRAP_TOKEN"], k)
    ]) == 0
    error_message = "extra_env must not set a reserved secret key (DATABASE_URL, JWT_SECRET, VAULT_KEY, CLAWVISOR_BOOTSTRAP_TOKEN); those travel via the Secrets-Manager env_file, never the compose environment map."
  }
}

# --- Placement -------------------------------------------------------------
# Optional. When empty the module uses the account's default VPC and its
# subnets, so examples/aws-minimal needs only region + fqdn + email + CIDRs.

variable "vpc_id" {
  type        = string
  default     = ""
  description = "VPC to deploy into. Empty ⇒ the account's default VPC."

  # The default-VPC subnet auto-discovery (data.aws_subnets.default) only runs
  # when vpc_id is empty. Supplying a non-default vpc_id without subnets would
  # index an absent data source (crash at plan), so require explicit subnet_id
  # and >= 2 db_subnet_ids alongside a custom vpc_id.
  # Cross-variable validation requires Terraform >= 1.9 (see versions.tf).
  validation {
    condition     = var.vpc_id == "" || (var.subnet_id != "" && length(var.db_subnet_ids) >= 2)
    error_message = "When vpc_id is set you must also set subnet_id and db_subnet_ids (>= 2 AZs); the module only auto-discovers subnets for the account's default VPC."
  }
}

variable "subnet_id" {
  type        = string
  default     = ""
  description = "Subnet for the EC2 instance. Empty ⇒ the first default-VPC subnet."
}

variable "db_subnet_ids" {
  type        = list(string)
  default     = []
  description = "Subnets for the RDS subnet group (needs >= 2 AZs). Empty ⇒ all default-VPC subnets."

  # The list flows straight to aws_db_subnet_group; a single subnet fails at
  # apply with DBSubnetGroupDoesNotCoverEnoughAZs. Catch it at plan.
  validation {
    condition     = length(var.db_subnet_ids) == 0 || length(var.db_subnet_ids) >= 2
    error_message = "db_subnet_ids must be empty (use the default-VPC subnets) or list at least 2 subnets in different AZs (RDS DB subnet groups require >= 2 AZs)."
  }
}

variable "key_name" {
  type        = string
  default     = ""
  description = "Optional EC2 key pair name for break-glass SSH. The module does NOT open port 22 to the world; SSH, if the key is set, is reachable only from admin_ingress_cidrs. Routine ops (upgrade) use SSM, not SSH."
}

# --- Secret decryption -----------------------------------------------------

variable "kms_key_arn" {
  type        = string
  default     = ""
  description = "Optional KMS CMK ARN that encrypts operator-supplied jwt_secret_ref/vault_key_ref secrets. When set, the instance role is granted kms:Decrypt on it so the boot-time GetSecretValue can decrypt CMK-encrypted refs. Leave empty when the refs use the AWS-managed key (aws/secretsmanager), which the instance role can already decrypt."

  validation {
    condition     = var.kms_key_arn == "" || can(regex("^arn:aws[a-z-]*:kms:", var.kms_key_arn))
    error_message = "kms_key_arn must be empty or an AWS KMS key ARN (arn:aws:kms:...)."
  }
}

# --- Docker Compose plugin (pinned + checksum-verified in cloud-init) -------

variable "docker_compose_version" {
  type        = string
  default     = "v2.29.7"
  description = "Pinned docker compose v2 plugin release tag downloaded in the privileged cloud-init bootstrap. Must have a matching entry in docker_compose_sha256; the download is checksum-verified before install. Bumping this without updating docker_compose_sha256 fails the on-instance verification."
}

variable "docker_compose_sha256" {
  type = map(string)
  default = {
    x86_64  = "383ce6698cd5d5bbf958d2c8489ed75094e34a77d340404d9f32c4ae9e12baf0"
    aarch64 = "6e9fbd5daa20dca5d7d89145081ae8155d68ef2928b497d9f85b54fe0f9dbb2c"
  }
  description = "SHA256 of the docker-compose-linux-<arch> binary for docker_compose_version, keyed by `uname -m` arch. Verified with `sha256sum -c` before chmod+install so a compromised/renamed GitHub asset cannot land in the privileged bootstrap."

  validation {
    condition     = can(var.docker_compose_sha256["x86_64"]) && can(var.docker_compose_sha256["aarch64"])
    error_message = "docker_compose_sha256 must provide checksums for both x86_64 and aarch64."
  }
}
