# The singleton model allow/deny policy. Models are provider-qualified ids.
# Requires the local_governance capability (spec 06a) on OSS.
resource "clawvisor_model_policy" "default" {
  mode = "allow"
  models = [
    "anthropic/claude-3-5-sonnet",
    "anthropic/claude-3-5-haiku",
  ]
}
