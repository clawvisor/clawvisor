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

type taskPolicyResource struct {
	pd *providerData
}

type taskPolicyModel struct {
	ID       types.String `tfsdk:"id"`
	Guidance types.String `tfsdk:"guidance"`
}

var (
	_ resource.Resource                = (*taskPolicyResource)(nil)
	_ resource.ResourceWithConfigure   = (*taskPolicyResource)(nil)
	_ resource.ResourceWithImportState = (*taskPolicyResource)(nil)
)

// NewTaskPolicyResource is the resource factory.
func NewTaskPolicyResource() resource.Resource { return &taskPolicyResource{} }

func (r *taskPolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_task_policy"
}

func (r *taskPolicyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if pd, ok := configure(req, resp); ok {
		r.pd = pd
	}
}

func (r *taskPolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The singleton task-guidance policy. Import with id `task_policy`. Requires the " +
			"`local_governance` capability (spec 06a) on OSS.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Always `task_policy` (singleton).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"guidance": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Free-text task guidance applied instance/org-wide.",
			},
		},
	}
}

func (r *taskPolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !requireCapability(r.pd, client.CapabilityLocalGovernance, "clawvisor_task_policy", &resp.Diagnostics) {
		return
	}
	var plan taskPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, err := r.pd.client.PutTaskPolicy(ctx, client.TaskPolicy{Guidance: plan.Guidance.ValueString()}); err != nil {
		diagFromError("Creating clawvisor_task_policy", err, &resp.Diagnostics)
		return
	}
	plan.ID = types.StringValue("task_policy")
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *taskPolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !requireCapability(r.pd, client.CapabilityLocalGovernance, "clawvisor_task_policy", &resp.Diagnostics) {
		return
	}
	var state taskPolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	tp, err := r.pd.client.GetTaskPolicy(ctx)
	if err != nil {
		if client.NotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		diagFromError("Reading clawvisor_task_policy", err, &resp.Diagnostics)
		return
	}
	state.ID = types.StringValue("task_policy")
	state.Guidance = types.StringValue(tp.Guidance)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *taskPolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan taskPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, err := r.pd.client.PutTaskPolicy(ctx, client.TaskPolicy{Guidance: plan.Guidance.ValueString()}); err != nil {
		diagFromError("Updating clawvisor_task_policy", err, &resp.Diagnostics)
		return
	}
	plan.ID = types.StringValue("task_policy")
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *taskPolicyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if err := r.pd.client.DeleteTaskPolicy(ctx); err != nil {
		if client.NotFound(err) {
			return
		}
		diagFromError("Deleting clawvisor_task_policy", err, &resp.Diagnostics)
	}
}

func (r *taskPolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, pathRoot("id"), req, resp)
}
