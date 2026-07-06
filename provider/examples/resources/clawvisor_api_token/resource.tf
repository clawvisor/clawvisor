# A long-lived, scoped, revocable API token (spec 05). The token plaintext is
# returned only at create time and transits state — treat state as sensitive.
resource "clawvisor_api_token" "terraform" {
  name  = "terraform"
  scope = "instance-admin"
}

output "terraform_api_token" {
  value     = clawvisor_api_token.terraform.token # cvat_ token
  sensitive = true
}
