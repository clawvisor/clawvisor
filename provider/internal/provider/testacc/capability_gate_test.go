package testacc

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccProvider_capabilityGate proves a capability-gated resource fails fast
// with an actionable error (and no state pollution) when the target server
// does not report the capability. Against the hermetic OSS testapp, the
// governance resources are gated on `local_governance`, which OSS does not
// report until spec 06a lands.
func TestAccProvider_capabilityGate(t *testing.T) {
	if hasLocalGovernance {
		t.Skip("server reports local_governance; the fail-fast gate is not exercised on this build")
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories,
		Steps: []resource.TestStep{
			{
				Config: `resource "clawvisor_model_policy" "test" {
  mode   = "deny"
  models = ["anthropic/claude-3-opus"]
}`,
				ExpectError: regexp.MustCompile(`requires the "local_governance" capability`),
			},
		},
	})
}
