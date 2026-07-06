# Per-service configuration document. config is opaque JSON stored verbatim;
# key ordering never causes a diff (semantic JSON equality).
resource "clawvisor_service_config" "github" {
  service_id = "github"
  config = jsonencode({
    base_url = "https://api.github.com"
    retries  = 3
  })
}
