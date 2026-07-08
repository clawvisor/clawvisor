package testacc

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccClawvisorSSOConnection_capabilityGate proves the enterprise-only
// clawvisor_sso_connection resource fails fast with an actionable error against
// a server that does not report the `sso` capability. The hermetic OSS testapp
// never reports it, so this exercises the gate in CI — the whole point of
// housing the resource in the OSS provider but disabling it outside Cloud.
func TestAccClawvisorSSOConnection_capabilityGate(t *testing.T) {
	if hasSSO {
		t.Skip("server reports the 'sso' capability; the fail-fast gate is not exercised on this build")
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_sso_connection" "test" {
  kind               = "oidc"
  email_domain       = "acme.com"
  oidc_issuer        = "https://acme.okta.com"
  oidc_client_id     = "0oaExample"
  oidc_client_secret = "example-secret"
}`,
				ExpectError: regexp.MustCompile(`requires the "sso" capability`),
			},
		},
	})
}

// TestAccClawvisorSSOConnection_validation covers the plan-time ValidateConfig
// rules (kind enum, SAML/OIDC field consistency, owner rejection). These fire
// before any server round-trip, so they run without the sso capability.
func TestAccClawvisorSSOConnection_validation(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name: "owner default_role rejected",
			config: `resource "clawvisor_sso_connection" "t" {
  kind               = "oidc"
  email_domain       = "acme.com"
  oidc_issuer        = "https://a.okta.com"
  oidc_client_id     = "c"
  oidc_client_secret = "s"
  default_role       = "owner"
}`,
			wantErr: `default_role must be`,
		},
		{
			name: "oidc missing fields",
			config: `resource "clawvisor_sso_connection" "t" {
  kind         = "oidc"
  email_domain = "acme.com"
}`,
			wantErr: `requires oidc_issuer, oidc_client_id, and oidc_client_secret`,
		},
		{
			name: "mixed saml + oidc",
			config: `resource "clawvisor_sso_connection" "t" {
  kind               = "oidc"
  email_domain       = "acme.com"
  oidc_issuer        = "https://a.okta.com"
  oidc_client_id     = "c"
  oidc_client_secret = "s"
  saml_entity_id     = "urn:x"
}`,
			wantErr: `must not set any saml_\* attribute`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: protoV6Factories,
				Steps: []resource.TestStep{
					{Config: tc.config, ExpectError: regexp.MustCompile(tc.wantErr)},
				},
			})
		})
	}
}

// TestAccClawvisorSSOConnection_oidcRoundTrip is a full apply against a Cloud
// deployment; it skips unless the server reports the sso capability AND the
// provider is org-scoped (CLAWVISOR_ORG_ID). Kept for Cloud CI / manual runs.
func TestAccClawvisorSSOConnection_oidcRoundTrip(t *testing.T) {
	if !hasSSO {
		t.Skip("server does not report the 'sso' capability (Cloud/enterprise only)")
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_sso_connection" "test" {
  kind               = "oidc"
  email_domain       = "acme.com"
  oidc_issuer        = "https://acme.okta.com"
  oidc_client_id     = "0oaExample"
  oidc_client_secret = "example-secret"
  jit_provision      = true
  default_role       = "member"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("clawvisor_sso_connection.test", "id"),
					resource.TestCheckResourceAttr("clawvisor_sso_connection.test", "kind", "oidc"),
					resource.TestCheckResourceAttr("clawvisor_sso_connection.test", "enabled", "true"),
				),
			},
		},
	})
}
