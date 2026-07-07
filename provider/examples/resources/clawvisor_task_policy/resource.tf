# The singleton task-guidance policy applied instance/org-wide.
# Requires the local_governance capability (spec 06a) on OSS.
resource "clawvisor_task_policy" "default" {
  guidance = "Prefer read-only tools; ask before mutating production."
}
