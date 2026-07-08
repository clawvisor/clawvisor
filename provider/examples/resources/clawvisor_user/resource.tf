# One clawvisor_user per employee, keyed by email. Each exposes a computed,
# sensitive invite_url — deliver it over a secret manager / 1Password share,
# never Slack or a plain terraform output (it is a one-shot bearer credential).
# Destroying a user offboards them (invalidates their cvis_ tokens immediately;
# audit/cost history is retained). Taint to reissue a fresh invite_url.
variable "employees" {
  type = map(string) # email => role ("admin" | "member")
  default = {
    "alice@example.com" = "admin"
    "bob@example.com"   = "member"
  }
}

resource "clawvisor_user" "employee" {
  for_each = var.employees
  email    = each.key
  role     = each.value
}
