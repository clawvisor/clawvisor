# A Clawvisor agent identity and its bearer token. The sanctioned way to enroll
# a CI/service identity: declare the agent and expose its token via a sensitive
# output. Changing name/description forces replacement (no server update
# endpoint); change rotate_trigger to rotate the token in place.
resource "clawvisor_agent" "ci" {
  name        = "github-actions"
  description = "CI runner service identity"
}

output "ci_agent_token" {
  value     = clawvisor_agent.ci.token # cvis_ agent token
  sensitive = true
}
