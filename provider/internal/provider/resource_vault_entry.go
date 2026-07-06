package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/clawvisor/clawvisor/provider/internal/client"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// vaultValueHashKey is the private-state key under which we store a SHA-256 of
// the last written secret value. The server never echoes the value, so this
// hash is the only drift signal available (used for documentation/forensics;
// value drift cannot be detected via Read because the server is write-only).
const vaultValueHashKey = "value_sha256"

type vaultEntryResource struct {
	pd *providerData
}

type vaultEntryModel struct {
	ID        types.String `tfsdk:"id"`
	ServiceID types.String `tfsdk:"service_id"`
	Value     types.String `tfsdk:"value"`
	UserID    types.String `tfsdk:"user_id"`
}

var (
	_ resource.Resource                = (*vaultEntryResource)(nil)
	_ resource.ResourceWithConfigure   = (*vaultEntryResource)(nil)
	_ resource.ResourceWithImportState = (*vaultEntryResource)(nil)
)

// NewVaultEntryResource is the resource factory.
func NewVaultEntryResource() resource.Resource { return &vaultEntryResource{} }

func (r *vaultEntryResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_vault_entry"
}

func (r *vaultEntryResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if pd, ok := configure(req, resp); ok {
		r.pd = pd
	}
}

func (r *vaultEntryResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A secret stored in the Clawvisor vault (push mode). The value transits " +
			"Terraform state — use an encrypted state backend and, where your Terraform version supports " +
			"it, ephemeral/write-only inputs. The server never returns the value, so value drift is not " +
			"detected on read (a SHA-256 is kept in private state for forensics).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Vault item id (equal to `service_id`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"service_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The vault item id (storage key). Changing it forces replacement.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"value": schema.StringAttribute{
				Required:            true,
				Sensitive:           true,
				MarkdownDescription: "The secret value to store. Write-only server-side; kept in state as configured.",
			},
			"user_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Reserved: owning user id. On OSS, token-authenticated writes are pinned " +
					"to the instance system user, so this attribute is informational in v1. Changing it forces replacement.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
		},
	}
}

func hashValue(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// valueHashJSON returns the SHA-256 of v as a JSON-encoded string. The
// framework requires private-state values to be valid JSON, so the raw hex
// digest is wrapped as a JSON string literal.
func valueHashJSON(v string) []byte {
	b, _ := json.Marshal(hashValue(v))
	return b
}

func (r *vaultEntryResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan vaultEntryModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.pd.client.CreateVaultEntry(ctx, plan.ServiceID.ValueString(), plan.Value.ValueString()); err != nil {
		diagFromError("Creating clawvisor_vault_entry", err, &resp.Diagnostics)
		return
	}

	plan.ID = plan.ServiceID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, vaultValueHashKey, valueHashJSON(plan.Value.ValueString()))...)
}

func (r *vaultEntryResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state vaultEntryModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Existence check only. The value is never fetched (write-only server-side),
	// so Read must not write it — that keeps the configured value authoritative
	// and avoids echoing the secret through Read.
	if _, err := r.pd.client.GetVaultEntry(ctx, state.ID.ValueString()); err != nil {
		if client.NotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		diagFromError("Reading clawvisor_vault_entry", err, &resp.Diagnostics)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *vaultEntryResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan vaultEntryModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Only value can change in place (service_id/user_id are RequiresReplace).
	if err := r.pd.client.UpdateVaultEntry(ctx, plan.ServiceID.ValueString(), plan.Value.ValueString()); err != nil {
		diagFromError("Updating clawvisor_vault_entry", err, &resp.Diagnostics)
		return
	}
	plan.ID = plan.ServiceID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, vaultValueHashKey, valueHashJSON(plan.Value.ValueString()))...)
}

func (r *vaultEntryResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state vaultEntryModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.pd.client.DeleteVaultEntry(ctx, state.ID.ValueString()); err != nil {
		if client.NotFound(err) {
			return
		}
		diagFromError("Deleting clawvisor_vault_entry", err, &resp.Diagnostics)
	}
}

func (r *vaultEntryResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import by vault item id; sets id and service_id. value cannot be
	// recovered (write-only) and must be supplied by config afterward.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("service_id"), req.ID)...)
}
