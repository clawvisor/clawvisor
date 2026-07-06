# AWS resources for the single-VM Clawvisor deploy.

# --- Placement (default VPC when not supplied) -----------------------------
data "aws_vpc" "default" {
  count   = var.vpc_id == "" ? 1 : 0
  default = true
}

data "aws_subnets" "default" {
  count = var.vpc_id == "" ? 1 : 0
  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.default[0].id]
  }
}

locals {
  vpc_id        = var.vpc_id != "" ? var.vpc_id : data.aws_vpc.default[0].id
  db_subnet_ids = length(var.db_subnet_ids) > 0 ? var.db_subnet_ids : data.aws_subnets.default[0].ids
  instance_subnet = var.subnet_id != "" ? var.subnet_id : (
    length(var.db_subnet_ids) > 0 ? var.db_subnet_ids[0] : data.aws_subnets.default[0].ids[0]
  )
}

data "aws_ami" "al2023" {
  most_recent = true
  owners      = ["amazon"]
  filter {
    name   = "name"
    values = ["al2023-ami-*-x86_64"]
  }
  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

# --- Two-zone security group -----------------------------------------------
# Deny-by-default: an ingress block renders only when its CIDR list is
# non-empty. 443 = agent endpoint (agent_ingress_cidrs); admin_port = admin
# surface (admin_ingress_cidrs). The i_understand_public_exposure gate on the
# variables is what allows 0.0.0.0/0 into either list at all.
resource "aws_security_group" "app" {
  name        = "${var.name}-app"
  description = "Clawvisor app: agent endpoint (443) + admin surface (${local.admin_port})"
  vpc_id      = local.vpc_id

  dynamic "ingress" {
    for_each = length(var.agent_ingress_cidrs) > 0 ? [1] : []
    content {
      description = "agent endpoint (LLM/proxy + installer)"
      from_port   = 443
      to_port     = 443
      protocol    = "tcp"
      cidr_blocks = var.agent_ingress_cidrs
    }
  }

  # HTTP is for the ACME challenge / HTTPS redirect only; scoped to the agent
  # zone. See README "DNS for ACME" for the Let's Encrypt reachability caveat
  # under deny-by-default.
  dynamic "ingress" {
    for_each = length(var.agent_ingress_cidrs) > 0 ? [1] : []
    content {
      description = "ACME HTTP-01 / HTTPS redirect"
      from_port   = 80
      to_port     = 80
      protocol    = "tcp"
      cidr_blocks = var.agent_ingress_cidrs
    }
  }

  dynamic "ingress" {
    for_each = length(var.admin_ingress_cidrs) > 0 ? [1] : []
    content {
      description = "admin dashboard + management API"
      from_port   = local.admin_port
      to_port     = local.admin_port
      protocol    = "tcp"
      cidr_blocks = var.admin_ingress_cidrs
    }
  }

  # Break-glass SSH, admin-CIDR-scoped, only if a key pair is supplied. Never
  # world-open. Routine ops (upgrade) use SSM, not SSH.
  dynamic "ingress" {
    for_each = var.key_name != "" && length(var.admin_ingress_cidrs) > 0 ? [1] : []
    content {
      description = "break-glass SSH (admin CIDRs only)"
      from_port   = 22
      to_port     = 22
      protocol    = "tcp"
      cidr_blocks = var.admin_ingress_cidrs
    }
  }

  egress {
    description = "all egress"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = { Name = "${var.name}-app", "clawvisor:managed" = "true" }
}

resource "aws_security_group" "db" {
  name        = "${var.name}-db"
  description = "Clawvisor managed Postgres: 5432 from the app instance only"
  vpc_id      = local.vpc_id

  ingress {
    description     = "postgres from app"
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [aws_security_group.app.id]
  }

  tags = { Name = "${var.name}-db", "clawvisor:managed" = "true" }
}

# --- IAM: instance profile -------------------------------------------------
data "aws_iam_policy_document" "assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "instance" {
  name               = "${var.name}-instance"
  assume_role_policy = data.aws_iam_policy_document.assume.json
  tags               = { "clawvisor:managed" = "true" }
}

# Session Manager + ssm:GetParameter for the upgrade poll.
resource "aws_iam_role_policy_attachment" "ssm_core" {
  role       = aws_iam_role.instance.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

data "aws_iam_policy_document" "instance" {
  # Read the secrets the container needs at boot.
  statement {
    sid       = "ReadSecrets"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = local.read_secret_arns
  }
  # Generated-mode: write JWT/VAULT on first boot (only the module-owned
  # secrets, never operator-supplied refs).
  dynamic "statement" {
    for_each = length(local.generated_secret_arns) > 0 ? [1] : []
    content {
      sid       = "PutGeneratedSecrets"
      actions   = ["secretsmanager:PutSecretValue"]
      resources = local.generated_secret_arns
    }
  }
  # The image parameter the deploy timer polls.
  statement {
    sid       = "ReadImageParam"
    actions   = ["ssm:GetParameter"]
    resources = [aws_ssm_parameter.image.arn]
  }
  # Snapshot-before-upgrade + prune.
  statement {
    sid = "SnapshotDB"
    actions = [
      "rds:CreateDBSnapshot",
      "rds:DescribeDBSnapshots",
      "rds:DeleteDBSnapshot",
    ]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "instance" {
  name   = "${var.name}-instance"
  role   = aws_iam_role.instance.id
  policy = data.aws_iam_policy_document.instance.json
}

resource "aws_iam_instance_profile" "instance" {
  name = "${var.name}-instance"
  role = aws_iam_role.instance.name
}

locals {
  generated_secret_arns = compact([
    local.jwt_generate ? aws_secretsmanager_secret.jwt[0].arn : "",
    local.vault_key_generate ? aws_secretsmanager_secret.vault_key[0].arn : "",
  ])
}

# --- SSM parameter: desired image tag (the upgrade lever) ------------------
# `terraform apply` with a new `image` only updates this parameter; the
# on-instance timer notices and redeploys. No instance replacement, no SSH.
resource "aws_ssm_parameter" "image" {
  name  = "/clawvisor/${var.name}/image"
  type  = "String"
  value = var.image
  tags  = { "clawvisor:managed" = "true" }
}

# --- Managed Postgres ------------------------------------------------------
resource "aws_db_subnet_group" "this" {
  name       = "${var.name}-db"
  subnet_ids = local.db_subnet_ids
  tags       = { "clawvisor:managed" = "true" }
}

resource "aws_db_instance" "this" {
  identifier              = "${var.name}-db"
  engine                  = "postgres"
  engine_version          = "16"
  instance_class          = var.db_instance_class
  allocated_storage       = var.db_allocated_storage
  db_name                 = local.db_name
  username                = local.db_user
  password                = random_password.db.result
  db_subnet_group_name    = aws_db_subnet_group.this.name
  vpc_security_group_ids  = [aws_security_group.db.id]
  backup_retention_period = var.backup_retention_days
  storage_encrypted       = true
  # v1: single-VM product accepts brief downtime and destroy-deletes the DB.
  # Take a manual snapshot before `terraform destroy` (README runbook).
  skip_final_snapshot = true
  apply_immediately   = true
  tags                = { "clawvisor:managed" = "true" }
}

# --- Application VM --------------------------------------------------------
resource "aws_instance" "app" {
  ami                    = data.aws_ami.al2023.id
  instance_type          = var.instance_type
  subnet_id              = local.instance_subnet
  vpc_security_group_ids = [aws_security_group.app.id]
  iam_instance_profile   = aws_iam_instance_profile.instance.name
  key_name               = var.key_name != "" ? var.key_name : null
  user_data              = local.user_data

  # An image bump changes only the SSM parameter (and thus user_data, since
  # the rendered deploy script references the parameter name — but NOT its
  # value). Never replace the instance on user_data change: the upgrade path
  # is the on-instance timer, not a fresh boot.
  user_data_replace_on_change = false

  root_block_device {
    volume_size = 30
    encrypted   = true
  }

  tags = { Name = var.name, "clawvisor:managed" = "true" }
}
