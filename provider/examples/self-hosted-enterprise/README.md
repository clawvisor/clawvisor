# Self-hosted enterprise: bootstrap an org entirely in Terraform

This example shows the full self-hosted flow: an instance-admin `cvat_` token
creates an org and mints a `cvot_` org token, and that `cvot_` then configures
the org's governance (and SSO). Requires a `self_hosted` deployment with an
enterprise license (`CLAWVISOR_DEPLOYMENT_MODE=self_hosted` + `CLAWVISOR_LICENSE_FILE`).

## Two-stage apply (required)

The `clawvisor.org` provider authenticates with the `cvot_` minted by
`clawvisor_org_token.terraform` — i.e. a **provider configuration that depends on
a resource created in the same configuration**. Terraform cannot configure that
provider until the token exists, so bootstrap the org + token first:

```sh
# Stage 1 — cvat_ creates the org and mints the cvot_:
terraform apply -target=clawvisor_org.acme -target=clawvisor_org_token.terraform

# Stage 2 — the cvot_ configures the org:
terraform apply
```

This is a Terraform core constraint (provider config from resource output), not a
Clawvisor limitation. The two-stage `apply` is needed whenever the `cvot_` is
(re)created: on the first apply, and again any time `clawvisor_org_token` is
replaced — all its inputs are `ForceNew` — or its write-once `token` is lost from
state. A steady-state `apply` that doesn't recreate the token needs only one pass.
