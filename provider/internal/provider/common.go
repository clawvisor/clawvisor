package provider

import (
	"fmt"

	"github.com/clawvisor/clawvisor/provider/internal/client"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

func pathRoot(name string) path.Path { return path.Root(name) }

// configure extracts *providerData from a resource ConfigureRequest. It
// returns (nil, false) when ProviderData is nil (Terraform calls Configure
// before provider Configure during some phases) so the caller can return
// early without erroring.
func configure(req resource.ConfigureRequest, resp *resource.ConfigureResponse) (*providerData, bool) {
	if req.ProviderData == nil {
		return nil, false
	}
	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data type",
			fmt.Sprintf("Expected *providerData, got %T. This is a provider bug.", req.ProviderData),
		)
		return nil, false
	}
	return pd, true
}

// requireCapability adds a fail-fast diagnostic when the server does not
// report the capability the resource needs, and returns false. The message is
// actionable (names the resource, capability, endpoint, and the fix).
func requireCapability(pd *providerData, capability, resourceType string, diags *diag.Diagnostics) bool {
	if pd.features.Has(capability) {
		return true
	}
	diags.AddError(
		"Server capability not available",
		fmt.Sprintf(
			"%s requires the %q capability, which the Clawvisor server at %s does not report. %s",
			resourceType, capability, pd.endpoint, capabilityHint(capability),
		),
	)
	return false
}

func capabilityHint(capability string) string {
	switch capability {
	case client.CapabilityLocalGovernance:
		return "This resource needs local governance (spec 06a) or a Clawvisor Cloud deployment. " +
			"On OSS, upgrade to a build that reports the `local_governance` capability."
	case client.CapabilityUserManagement:
		return "This resource needs the flat-team/user management feature (spec 04) or Clawvisor Cloud."
	case client.CapabilitySecretVault:
		return "This resource needs the secret vault feature (spec 10). " +
			"On OSS, upgrade to a build that reports the `secret_vault` capability."
	case client.CapabilityTeams, client.CapabilitySSO, client.CapabilityMultiTenant:
		return "This resource needs a Clawvisor Cloud or in-VPC deployment."
	default:
		return "This capability is not enabled on the target server."
	}
}

// diagFromError maps a client error to a framework error diagnostic with an
// actionable message. 404s are handled by resources directly (state removal),
// so this is for the other cases.
func diagFromError(op string, err error, diags *diag.Diagnostics) {
	if ae, ok := err.(*client.APIError); ok {
		switch ae.StatusCode {
		case 401, 403:
			diags.AddError(op+" failed: authorization error",
				ae.Error()+"\nEnsure the provider api_token is valid and carries the instance-admin scope.")
			return
		case 409, 422:
			diags.AddError(op+" failed", ae.Error())
			return
		}
		diags.AddError(op+" failed", ae.Error())
		return
	}
	diags.AddError(op+" failed", err.Error())
}
