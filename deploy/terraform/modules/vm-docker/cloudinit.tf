# Renders the compose / Caddyfile / config.yaml / deploy script templates and
# assembles them into the cloud-init user_data. Each rendered artifact is
# base64-embedded (cloud-init `encoding: b64`) to avoid YAML-in-YAML escaping.

locals {
  secrets_env_file = "/etc/clawvisor/secrets.env"

  compose_rendered = templatefile("${path.module}/templates/docker-compose.yaml.tftpl", {
    app_env          = local.app_env
    secrets_env_file = local.secrets_env_file
    admin_port       = local.admin_port
    vault_backend    = var.vault_backend
  })

  caddyfile_rendered = templatefile("${path.module}/templates/Caddyfile.tftpl", {
    public_fqdn = var.public_fqdn
    acme_email  = var.acme_email
    admin_port  = local.admin_port
  })

  config_rendered = templatefile("${path.module}/templates/config.yaml.tftpl", {
    public_fqdn   = var.public_fqdn
    otel_endpoint = var.otel_endpoint
    posture       = var.posture
  })

  deploy_rendered = templatefile("${path.module}/templates/deploy.sh.tftpl", {
    region                = var.region
    name                  = var.name
    image_ssm_param       = aws_ssm_parameter.image.name
    db_instance_id        = aws_db_instance.this.identifier
    backup_retention_days = var.backup_retention_days
    db_url_secret_arn     = aws_secretsmanager_secret.db_url.arn
    jwt_secret_arn        = local.jwt_secret_arn
    vault_key_arn         = local.vault_key_arn
    bootstrap_secret_arn  = aws_secretsmanager_secret.bootstrap.arn
    jwt_generate          = local.jwt_generate
    vault_key_generate    = local.vault_key_generate
  })

  user_data = templatefile("${path.module}/templates/cloud-init.yaml.tftpl", {
    compose_b64   = base64encode(local.compose_rendered)
    caddyfile_b64 = base64encode(local.caddyfile_rendered)
    config_b64    = base64encode(local.config_rendered)
    deploy_b64    = base64encode(local.deploy_rendered)
  })
}
