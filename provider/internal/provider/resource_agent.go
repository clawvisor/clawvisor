package provider

import (
	"context"

	"github.com/clawvisor/clawvisor/provider/internal/client"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// tokenPlanModifier keeps the agent token stable in the plan (like
// UseStateForUnknown) EXCEPT when rotate_trigger changes, in which case the
// token is left unknown so the rotate in Update produces a value consistent
// with the plan. Plain UseStateForUnknown would pin the plan to the old token
// while Update rotates it — an "inconsistent result after apply" error.
type tokenPlanModifier struct{}

func (m tokenPlanModifier) Description(_ context.Context) string {
	return "Preserves the token unless rotate_trigger changes, in which case it is known after apply."
}

func (m tokenPlanModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (m tokenPlanModifier) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	// Create (no prior state) and known planned values need no adjustment.
	if req.StateValue.IsNull() || !req.PlanValue.IsUnknown() {
		return
	}
	var stateTrigger, planTrigger types.String
	req.State.GetAttribute(ctx, path.Root("rotate_trigger"), &stateTrigger)
	req.Plan.GetAttribute(ctx, path.Root("rotate_trigger"), &planTrigger)
	if !stateTrigger.Equal(planTrigger) {
		// rotate_trigger changed → token will be rotated; leave it unknown.
		return
	}
	resp.PlanValue = req.StateValue
}

type agentResource struct {
	pd *providerData
}

type agentModel struct {
	ID            types.String `tfsdk:"id"`
	Name          types.String `tfsdk:"name"`
	Description   types.String `tfsdk:"description"`
	RotateTrigger types.String `tfsdk:"rotate_trigger"`
	Token         types.String `tfsdk:"token"`
}

var (
	_ resource.Resource                = (*agentResource)(nil)
	_ resource.ResourceWithConfigure   = (*agentResource)(nil)
	_ resource.ResourceWithImportState = (*agentResource)(nil)
)

// NewAgentResource is the resource factory.
func NewAgentResource() resource.Resource { return &agentResource{} }

func (r *agentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_agent"
}

func (r *agentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if pd, ok := configure(req, resp); ok {
		r.pd = pd
	}
}

func (r *agentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A Clawvisor agent identity and its bearer token. Because the server " +
			"has no agent update endpoint, changing `name` or `description` forces replacement. " +
			"The sanctioned way to enroll a CI/service identity: declare a `clawvisor_agent` and " +
			"expose its `token` via a sensitive output.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Server-assigned agent id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Human-readable agent name. Changing it forces replacement (no update endpoint).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"description": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Optional description. Changing it forces replacement (no update endpoint).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"rotate_trigger": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Change this arbitrary value to rotate the agent token in place " +
					"(preserves the agent id and tasks). Any new value triggers a rotate on the next apply.",
			},
			"token": schema.StringAttribute{
				Computed:            true,
				Sensitive:           true,
				MarkdownDescription: "The agent bearer token (`cvis_`). Returned only at create/rotate and stored in state; treat state as sensitive.",
				PlanModifiers:       []planmodifier.String{tokenPlanModifier{}},
			},
		},
	}
}

func (r *agentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan agentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	agent, err := r.pd.client.CreateAgent(ctx, client.CreateAgentRequest{
		Name:        plan.Name.ValueString(),
		Description: plan.Description.ValueString(),
	})
	if err != nil {
		diagFromError("Creating clawvisor_agent", err, &resp.Diagnostics)
		return
	}

	plan.ID = types.StringValue(agent.ID)
	plan.Description = types.StringValue(agent.Description)
	plan.Token = types.StringValue(agent.Token)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *agentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state agentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	agent, err := r.pd.client.GetAgent(ctx, state.ID.ValueString())
	if err != nil {
		if client.NotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		diagFromError("Reading clawvisor_agent", err, &resp.Diagnostics)
		return
	}

	// Never touch token or rotate_trigger here: token is not returned by the
	// list endpoint (UseStateForUnknown keeps it), and rotate_trigger is a
	// client-only signal.
	state.Name = types.StringValue(agent.Name)
	state.Description = types.StringValue(agent.Description)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *agentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state agentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// name/description are RequiresReplace, so the only in-place change that
	// reaches Update is rotate_trigger — rotate the token.
	plan.ID = state.ID
	plan.Token = state.Token
	if !plan.RotateTrigger.Equal(state.RotateTrigger) {
		newToken, err := r.pd.client.RotateAgentToken(ctx, state.ID.ValueString())
		if err != nil {
			diagFromError("Rotating clawvisor_agent token", err, &resp.Diagnostics)
			return
		}
		plan.Token = types.StringValue(newToken)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *agentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state agentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.pd.client.DeleteAgent(ctx, state.ID.ValueString()); err != nil {
		if client.NotFound(err) {
			return
		}
		diagFromError("Deleting clawvisor_agent", err, &resp.Diagnostics)
	}
}

func (r *agentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, pathRoot("id"), req, resp)
}
