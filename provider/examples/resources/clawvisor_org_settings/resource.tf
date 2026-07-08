# Per-org member self-service. Requires the provider's org_id to be set
# (Clawvisor Cloud / enterprise). Defaults to true — set false to centrally
# manage: members then cannot connect their own agents / activate services
# from the dashboard, and an org admin does it for them.
resource "clawvisor_org_settings" "acme" {
  member_self_service = false
}
