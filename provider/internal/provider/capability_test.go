package provider

import (
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/provider/internal/client"

	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// TestRequireCapabilityGate proves the fail-fast gate: when the server does
// not report a required capability, requireCapability returns false and emits
// an actionable diagnostic naming the resource, capability, and endpoint. When
// the capability is present it returns true with no diagnostics.
func TestRequireCapabilityGate(t *testing.T) {
	t.Run("missing capability fails fast", func(t *testing.T) {
		pd := &providerData{
			features: &client.Features{}, // local_governance absent → false
			endpoint: "https://clawvisor.example:25297",
		}
		var diags diag.Diagnostics
		if requireCapability(pd, client.CapabilityLocalGovernance, "clawvisor_model_policy", &diags) {
			t.Fatal("expected requireCapability to return false")
		}
		if !diags.HasError() {
			t.Fatal("expected an error diagnostic")
		}
		detail := diags[0].Detail()
		for _, want := range []string{"clawvisor_model_policy", "local_governance", "https://clawvisor.example:25297"} {
			if !strings.Contains(detail, want) {
				t.Errorf("diagnostic detail missing %q:\n%s", want, detail)
			}
		}
	})

	t.Run("present capability passes", func(t *testing.T) {
		pd := &providerData{
			features: &client.Features{LocalGovernance: true},
			endpoint: "https://clawvisor.example:25297",
		}
		var diags diag.Diagnostics
		if !requireCapability(pd, client.CapabilityLocalGovernance, "clawvisor_model_policy", &diags) {
			t.Fatal("expected requireCapability to return true")
		}
		if diags.HasError() {
			t.Fatalf("expected no diagnostics, got: %v", diags)
		}
	})
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "x"); got != "x" {
		t.Errorf("firstNonEmpty = %q, want x", got)
	}
	if got := firstNonEmpty("a", "b"); got != "a" {
		t.Errorf("firstNonEmpty = %q, want a", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("firstNonEmpty = %q, want empty", got)
	}
}
