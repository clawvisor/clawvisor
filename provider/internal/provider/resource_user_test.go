package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/provider/internal/client"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// TestUserResourceCapabilityGate proves every clawvisor_user CRUD entry point
// gates on the user_management capability: when the server does not report it,
// each method fails fast with the standard actionable diagnostic (naming the
// resource and capability) instead of proceeding to a raw API call. The gate
// runs before the request body is touched, so a zero request is sufficient.
func TestUserResourceCapabilityGate(t *testing.T) {
	ctx := context.Background()
	r := &userResource{pd: &providerData{
		features: &client.Features{}, // user_management absent → false
		endpoint: "https://clawvisor.example:25297",
	}}

	assertGated := func(t *testing.T, diags diag.Diagnostics) {
		t.Helper()
		if !diags.HasError() {
			t.Fatal("expected an error diagnostic from the capability gate")
		}
		detail := diags[0].Detail()
		for _, want := range []string{"clawvisor_user", client.CapabilityUserManagement} {
			if !strings.Contains(detail, want) {
				t.Errorf("diagnostic detail missing %q:\n%s", want, detail)
			}
		}
	}

	t.Run("create", func(t *testing.T) {
		var resp resource.CreateResponse
		r.Create(ctx, resource.CreateRequest{}, &resp)
		assertGated(t, resp.Diagnostics)
	})
	t.Run("read", func(t *testing.T) {
		var resp resource.ReadResponse
		r.Read(ctx, resource.ReadRequest{}, &resp)
		assertGated(t, resp.Diagnostics)
	})
	t.Run("update", func(t *testing.T) {
		var resp resource.UpdateResponse
		r.Update(ctx, resource.UpdateRequest{}, &resp)
		assertGated(t, resp.Diagnostics)
	})
	t.Run("delete", func(t *testing.T) {
		var resp resource.DeleteResponse
		r.Delete(ctx, resource.DeleteRequest{}, &resp)
		assertGated(t, resp.Diagnostics)
	})
}
