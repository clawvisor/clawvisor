package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/provider/internal/client"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// TestVaultReferenceResource_RequiresSecretVaultCapability proves every CRUD
// path fails fast when the server does not report the secret_vault capability.
// The gate runs before the request is read, so zero-value requests suffice: if
// the gate were missing, Create/Read/Update/Delete would instead try to reach
// the client and produce a different (or no) diagnostic.
func TestVaultReferenceResource_RequiresSecretVaultCapability(t *testing.T) {
	r := &vaultReferenceResource{pd: &providerData{
		features: &client.Features{}, // secret_vault absent → gate must fire
		endpoint: "https://clawvisor.example:25297",
	}}
	ctx := context.Background()

	assertGate := func(t *testing.T, diags diag.Diagnostics) {
		t.Helper()
		if !diags.HasError() {
			t.Fatal("expected a capability gate error diagnostic")
		}
		detail := diags[0].Detail()
		for _, want := range []string{"clawvisor_vault_reference", "secret_vault", "https://clawvisor.example:25297"} {
			if !strings.Contains(detail, want) {
				t.Errorf("gate diagnostic missing %q:\n%s", want, detail)
			}
		}
	}

	t.Run("create", func(t *testing.T) {
		var resp resource.CreateResponse
		r.Create(ctx, resource.CreateRequest{}, &resp)
		assertGate(t, resp.Diagnostics)
	})
	t.Run("read", func(t *testing.T) {
		var resp resource.ReadResponse
		r.Read(ctx, resource.ReadRequest{}, &resp)
		assertGate(t, resp.Diagnostics)
	})
	t.Run("update", func(t *testing.T) {
		var resp resource.UpdateResponse
		r.Update(ctx, resource.UpdateRequest{}, &resp)
		assertGate(t, resp.Diagnostics)
	})
	t.Run("delete", func(t *testing.T) {
		var resp resource.DeleteResponse
		r.Delete(ctx, resource.DeleteRequest{}, &resp)
		assertGate(t, resp.Diagnostics)
	})
}
