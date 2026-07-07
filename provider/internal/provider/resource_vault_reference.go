package provider

import (
	"context"

	"github.com/clawvisor/clawvisor/provider/internal/client"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type vaultReferenceResource struct {
	pd *providerData
}

type vaultReferenceModel struct {
	ID        types.String `tfsdk:"id"`
	ServiceID types.String `tfsdk:"service_id"`
	Backend   types.String `tfsdk:"backend"`
	Reference types.String `tfsdk:"reference"`
	JSONKey   types.String `tfsdk:"json_key"`
	Verify    types.Bool   `tfsdk:"verify"`
}

var (
	_ resource.Resource                = (*vaultReferenceResource)(nil)
	_ resource.ResourceWithConfigure   = (*vaultReferenceResource)(nil)
	_ resource.ResourceWithImportState = (*vaultReferenceResource)(nil)
)

// NewVaultReferenceResource is the resource factory.
func NewVaultReferenceResource() resource.Resource { return &vaultReferenceResource{} }

func (r *vaultReferenceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_vault_reference"
}

func (r *vaultReferenceResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if pd, ok := configure(req, resp); ok {
		r.pd = pd
	}
}

func (r *vaultReferenceResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A vault entry that holds a REFERENCE to a secret in your own store (AWS " +
			"Secrets Manager, GCP Secret Manager), resolved to plaintext by Clawvisor at injection time " +
			"and never persisted. No secret value ever appears in Terraform state — only the reference. " +
			"Reference targets must match the server's `vault.reference_allowlist`, and resolution uses " +
			"the instance's ambient cloud identity; grant that identity read access to exactly the " +
			"referenced secrets.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Vault item id (equal to `service_id`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"service_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The vault item id (storage key) the reference is stored under. Changing it forces replacement.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"backend": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The external secret backend: `aws-sm`, `gcp-sm`, or `hashicorp` (hashicorp is reserved and not yet implemented).",
			},
			"reference": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The backend-specific locator: an ARN (aws-sm), a full resource name " +
					"`projects/{p}/secrets/{s}` (gcp-sm; append `/versions/N` to pin a version, else `latest`), " +
					"or a KV v2 path (hashicorp). Must match the server's reference allowlist.",
			},
			"json_key": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(""),
				MarkdownDescription: "When set, the fetched secret is parsed as JSON and this key extracted; when empty, the raw secret bytes are used.",
			},
			"verify": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "When true (default), the server dry-run resolves the reference on apply and fails with an actionable error if it cannot be read. Set false to skip the live check (e.g. when the instance identity is not yet granted access at apply time).",
			},
		},
	}
}

func (m vaultReferenceModel) input() client.VaultReferenceInput {
	return client.VaultReferenceInput{
		Backend: m.Backend.ValueString(),
		ID:      m.Reference.ValueString(),
		JSONKey: m.JSONKey.ValueString(),
	}
}

func (r *vaultReferenceResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan vaultReferenceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.pd.client.CreateVaultReference(ctx, plan.ServiceID.ValueString(), plan.input(), plan.Verify.ValueBool()); err != nil {
		diagFromError("Creating clawvisor_vault_reference", err, &resp.Diagnostics)
		return
	}
	plan.ID = plan.ServiceID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *vaultReferenceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state vaultReferenceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Existence check only. The server exposes reference metadata but never the
	// envelope details or the resolved value, so the configured backend/reference/
	// json_key stay authoritative and Read must not overwrite them.
	if _, err := r.pd.client.GetVaultEntry(ctx, state.ID.ValueString()); err != nil {
		if client.NotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		diagFromError("Reading clawvisor_vault_reference", err, &resp.Diagnostics)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *vaultReferenceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan vaultReferenceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// backend / reference / json_key change in place; service_id is RequiresReplace.
	if err := r.pd.client.UpdateVaultReference(ctx, plan.ServiceID.ValueString(), plan.input(), plan.Verify.ValueBool()); err != nil {
		diagFromError("Updating clawvisor_vault_reference", err, &resp.Diagnostics)
		return
	}
	plan.ID = plan.ServiceID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *vaultReferenceResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state vaultReferenceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.pd.client.DeleteVaultEntry(ctx, state.ID.ValueString()); err != nil {
		if client.NotFound(err) {
			return
		}
		diagFromError("Deleting clawvisor_vault_reference", err, &resp.Diagnostics)
	}
}

func (r *vaultReferenceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import by vault item id; sets id and service_id. backend/reference/json_key
	// must be supplied by config afterward (the server does not expose the
	// stored envelope through the read side).
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("service_id"), req.ID)...)
}
