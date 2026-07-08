package provider

import (
	"context"

	"github.com/clawvisor/clawvisor/provider/internal/client"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type orgResource struct {
	pd *providerData
}

type orgModel struct {
	ID   types.String `tfsdk:"id"`
	Name types.String `tfsdk:"name"`
	Slug types.String `tfsdk:"slug"`
	Tier types.String `tfsdk:"tier"`
}

var (
	_ resource.Resource                = (*orgResource)(nil)
	_ resource.ResourceWithConfigure   = (*orgResource)(nil)
	_ resource.ResourceWithImportState = (*orgResource)(nil)
)

// NewOrgResource is the resource factory.
func NewOrgResource() resource.Resource { return &orgResource{} }

func (r *orgResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org"
}

func (r *orgResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if pd, ok := configure(req, resp); ok {
		r.pd = pd
	}
}

func (r *orgResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A Clawvisor organization, provisioned by an instance-admin (`cvat_`) token on a " +
			"self-hosted deployment. The org is created unowned + enterprise-tier — the `clawvisor_org_token` " +
			"minted next is its initial admin credential, and real users arrive later via SSO JIT. " +
			"Requires the instance-admin `/api/admin/orgs` surface (self-hosted mode); it does not exist on the " +
			"hosted SaaS or OSS. Configure the provider with a `cvat_` token and NO `org_id`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Server-assigned org id (e.g. `org_...`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Human-readable org name. The admin surface has no rename endpoint, so changing it forces replacement.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"slug": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "URL-safe org slug (unique). Changing it forces replacement.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"tier": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Org tier — always `enterprise` for an admin-provisioned org.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *orgResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !requireCapability(r.pd, client.CapabilityTeams, "clawvisor_org", &resp.Diagnostics) {
		return
	}
	var plan orgModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	org, err := r.pd.client.CreateOrg(ctx, client.CreateOrgRequest{
		Name: plan.Name.ValueString(),
		Slug: plan.Slug.ValueString(),
	})
	if err != nil {
		diagFromError("Creating clawvisor_org", err, &resp.Diagnostics)
		return
	}
	plan.ID = types.StringValue(org.ID)
	plan.Tier = types.StringValue(org.Tier)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *orgResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !requireCapability(r.pd, client.CapabilityTeams, "clawvisor_org", &resp.Diagnostics) {
		return
	}
	var state orgModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	org, err := r.pd.client.GetOrg(ctx, state.ID.ValueString())
	if err != nil {
		if client.NotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		diagFromError("Reading clawvisor_org", err, &resp.Diagnostics)
		return
	}
	state.Name = types.StringValue(org.Name)
	state.Slug = types.StringValue(org.Slug)
	state.Tier = types.StringValue(org.Tier)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update never runs: every attribute is Computed or RequiresReplace. It is a
// no-op copy to satisfy the interface.
func (r *orgResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan orgModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *orgResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state orgModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.pd.client.DeleteOrg(ctx, state.ID.ValueString()); err != nil {
		if client.NotFound(err) {
			return
		}
		diagFromError("Deleting clawvisor_org", err, &resp.Diagnostics)
	}
}

func (r *orgResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, pathRoot("id"), req, resp)
}
