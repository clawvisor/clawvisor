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

type modelPolicyResource struct {
	pd *providerData
}

type modelPolicyModel struct {
	ID     types.String `tfsdk:"id"`
	Mode   types.String `tfsdk:"mode"`
	Models types.List   `tfsdk:"models"`
}

var (
	_ resource.Resource                = (*modelPolicyResource)(nil)
	_ resource.ResourceWithConfigure   = (*modelPolicyResource)(nil)
	_ resource.ResourceWithImportState = (*modelPolicyResource)(nil)
)

// NewModelPolicyResource is the resource factory.
func NewModelPolicyResource() resource.Resource { return &modelPolicyResource{} }

func (r *modelPolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_model_policy"
}

func (r *modelPolicyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if pd, ok := configure(req, resp); ok {
		r.pd = pd
	}
}

func (r *modelPolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The singleton model allow/deny policy for the instance (OSS) or org (Cloud). " +
			"Import with id `model_policy`. Requires the `local_governance` capability (spec 06a) on OSS.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Always `model_policy` (singleton).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"mode": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "`allow` (only listed models permitted) or `deny` (listed models blocked).",
			},
			"models": schema.ListAttribute{
				Required:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Provider-qualified model ids, e.g. `anthropic/claude-3-5-sonnet`.",
			},
		},
	}
}

func (r *modelPolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !requireCapability(r.pd, client.CapabilityLocalGovernance, "clawvisor_model_policy", &resp.Diagnostics) {
		return
	}
	var plan modelPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var models []string
	resp.Diagnostics.Append(plan.Models.ElementsAs(ctx, &models, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, err := r.pd.client.PutModelPolicy(ctx, client.ModelPolicy{Mode: plan.Mode.ValueString(), Models: models}); err != nil {
		diagFromError("Creating clawvisor_model_policy", err, &resp.Diagnostics)
		return
	}
	plan.ID = types.StringValue("model_policy")
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *modelPolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !requireCapability(r.pd, client.CapabilityLocalGovernance, "clawvisor_model_policy", &resp.Diagnostics) {
		return
	}
	var state modelPolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	mp, err := r.pd.client.GetModelPolicy(ctx)
	if err != nil {
		if client.NotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		diagFromError("Reading clawvisor_model_policy", err, &resp.Diagnostics)
		return
	}
	models, d := types.ListValueFrom(ctx, types.StringType, mp.Models)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.ID = types.StringValue("model_policy")
	state.Mode = types.StringValue(mp.Mode)
	state.Models = models
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *modelPolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan modelPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var models []string
	resp.Diagnostics.Append(plan.Models.ElementsAs(ctx, &models, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, err := r.pd.client.PutModelPolicy(ctx, client.ModelPolicy{Mode: plan.Mode.ValueString(), Models: models}); err != nil {
		diagFromError("Updating clawvisor_model_policy", err, &resp.Diagnostics)
		return
	}
	plan.ID = types.StringValue("model_policy")
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *modelPolicyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if err := r.pd.client.DeleteModelPolicy(ctx); err != nil {
		if client.NotFound(err) {
			return
		}
		diagFromError("Deleting clawvisor_model_policy", err, &resp.Diagnostics)
	}
}

func (r *modelPolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, pathRoot("id"), req, resp)
}
