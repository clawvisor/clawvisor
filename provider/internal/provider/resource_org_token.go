package provider

import (
	"context"

	"github.com/clawvisor/clawvisor/provider/internal/client"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type orgTokenResource struct {
	pd *providerData
}

type orgTokenModel struct {
	ID            types.String `tfsdk:"id"`
	OrgID         types.String `tfsdk:"org_id"`
	Name          types.String `tfsdk:"name"`
	ExpiresInDays types.Int64  `tfsdk:"expires_in_days"`
	TokenPrefix   types.String `tfsdk:"token_prefix"`
	ExpiresAt     types.String `tfsdk:"expires_at"`
	Token         types.String `tfsdk:"token"`
}

var (
	_ resource.Resource              = (*orgTokenResource)(nil)
	_ resource.ResourceWithConfigure = (*orgTokenResource)(nil)
)

// NewOrgTokenResource is the resource factory.
func NewOrgTokenResource() resource.Resource { return &orgTokenResource{} }

func (r *orgTokenResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_token"
}

func (r *orgTokenResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if pd, ok := configure(req, resp); ok {
		r.pd = pd
	}
}

func (r *orgTokenResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A `cvot_` org-scoped admin token, minted for an org by an instance-admin (`cvat_`) " +
			"token via the self-hosted `/api/admin/orgs/{id}/tokens` surface. Use it to configure the org from " +
			"Terraform (SSO, governance) without a user login. The plaintext is returned only at create time. " +
			"Configure the minting provider with a `cvat_` token and NO `org_id`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Server-assigned token id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"org_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The org this token administers (e.g. `clawvisor_org.acme.id`). Changing it forces replacement.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Human-readable token name. Changing it forces replacement.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"expires_in_days": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Optional lifetime in days from creation. Omit for a non-expiring token. Changing it forces replacement.",
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			"token_prefix": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Display prefix of the token (identifies it without revealing the secret).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"expires_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "RFC3339 expiry, or empty for a non-expiring token.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"token": schema.StringAttribute{
				Computed:            true,
				Sensitive:           true,
				MarkdownDescription: "The `cvot_` token plaintext, returned only at create time and stored in state; treat state as sensitive.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *orgTokenResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !requireCapability(r.pd, client.CapabilityTeams, "clawvisor_org_token", &resp.Diagnostics) {
		return
	}
	var plan orgTokenModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := client.CreateOrgTokenRequest{Name: plan.Name.ValueString()}
	if !plan.ExpiresInDays.IsNull() && !plan.ExpiresInDays.IsUnknown() {
		d := int(plan.ExpiresInDays.ValueInt64())
		body.ExpiresInDays = &d
	}
	tok, err := r.pd.client.CreateOrgTokenAdmin(ctx, plan.OrgID.ValueString(), body)
	if err != nil {
		diagFromError("Creating clawvisor_org_token", err, &resp.Diagnostics)
		return
	}
	plan.ID = types.StringValue(tok.ID)
	plan.TokenPrefix = types.StringValue(tok.TokenPrefix)
	plan.ExpiresAt = types.StringValue(tok.ExpiresAt)
	plan.Token = types.StringValue(tok.Token)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *orgTokenResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !requireCapability(r.pd, client.CapabilityTeams, "clawvisor_org_token", &resp.Diagnostics) {
		return
	}
	var state orgTokenModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// There is no single-item GET; confirm the token still exists (unrevoked) in
	// the org's list. The plaintext is create-only and stays in state.
	toks, err := r.pd.client.ListOrgTokens(ctx, state.OrgID.ValueString())
	if err != nil {
		if client.NotFound(err) { // the org itself is gone
			resp.State.RemoveResource(ctx)
			return
		}
		diagFromError("Reading clawvisor_org_token", err, &resp.Diagnostics)
		return
	}
	for _, t := range toks {
		if t.ID == state.ID.ValueString() {
			state.Name = types.StringValue(t.Name)
			state.TokenPrefix = types.StringValue(t.TokenPrefix)
			state.ExpiresAt = types.StringValue(t.ExpiresAt)
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
	}
	// Not found (revoked or deleted) → gone.
	resp.State.RemoveResource(ctx)
}

// Update never runs: every settable attribute is RequiresReplace. No-op copy.
func (r *orgTokenResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan orgTokenModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *orgTokenResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state orgTokenModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.pd.client.RevokeOrgToken(ctx, state.OrgID.ValueString(), state.ID.ValueString()); err != nil {
		if client.NotFound(err) {
			return
		}
		diagFromError("Deleting clawvisor_org_token", err, &resp.Diagnostics)
	}
}
