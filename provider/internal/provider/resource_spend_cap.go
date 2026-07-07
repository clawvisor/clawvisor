package provider

import (
	"context"

	"github.com/clawvisor/clawvisor/provider/internal/client"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type spendCapResource struct {
	pd *providerData
}

type spendCapModel struct {
	ID          types.String `tfsdk:"id"`
	Window      types.String `tfsdk:"window"`
	CapMicros   types.Int64  `tfsdk:"cap_micros"`
	Enforcement types.String `tfsdk:"enforcement"`
}

var (
	_ resource.Resource                = (*spendCapResource)(nil)
	_ resource.ResourceWithConfigure   = (*spendCapResource)(nil)
	_ resource.ResourceWithImportState = (*spendCapResource)(nil)
)

// NewSpendCapResource is the resource factory.
func NewSpendCapResource() resource.Resource { return &spendCapResource{} }

func (r *spendCapResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_spend_cap"
}

func (r *spendCapResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if pd, ok := configure(req, resp); ok {
		r.pd = pd
	}
}

func (r *spendCapResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A per-window spend cap. One cap per window; import with id equal to the window " +
			"(`daily` or `monthly`). Requires the `local_governance` capability (spec 06a) on OSS.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Equal to `window`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"window": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "`daily` or `monthly`. Changing it forces replacement.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"cap_micros": schema.Int64Attribute{
				Required:            true,
				MarkdownDescription: "Cap in micros (1e-6 USD). Must be positive.",
			},
			"enforcement": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("soft"),
				MarkdownDescription: "`soft` (warn at 80%/100%) or `hard` (block at 100%). Defaults to `soft`.",
			},
		},
	}
}

func (r *spendCapResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !requireCapability(r.pd, client.CapabilityLocalGovernance, "clawvisor_spend_cap", &resp.Diagnostics) {
		return
	}
	var plan spendCapModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, err := r.pd.client.PutSpendCap(ctx, plan.Window.ValueString(), plan.CapMicros.ValueInt64(), plan.Enforcement.ValueString()); err != nil {
		diagFromError("Creating clawvisor_spend_cap", err, &resp.Diagnostics)
		return
	}
	plan.ID = plan.Window
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *spendCapResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !requireCapability(r.pd, client.CapabilityLocalGovernance, "clawvisor_spend_cap", &resp.Diagnostics) {
		return
	}
	var state spendCapModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	sc, err := r.pd.client.GetSpendCap(ctx, state.Window.ValueString())
	if err != nil {
		if client.NotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		diagFromError("Reading clawvisor_spend_cap", err, &resp.Diagnostics)
		return
	}
	state.ID = types.StringValue(sc.Window)
	state.Window = types.StringValue(sc.Window)
	state.CapMicros = types.Int64Value(sc.CapMicros)
	state.Enforcement = types.StringValue(sc.Enforcement)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *spendCapResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan spendCapModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, err := r.pd.client.PutSpendCap(ctx, plan.Window.ValueString(), plan.CapMicros.ValueInt64(), plan.Enforcement.ValueString()); err != nil {
		diagFromError("Updating clawvisor_spend_cap", err, &resp.Diagnostics)
		return
	}
	plan.ID = plan.Window
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *spendCapResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state spendCapModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.pd.client.DeleteSpendCap(ctx, state.Window.ValueString()); err != nil {
		if client.NotFound(err) {
			return
		}
		diagFromError("Deleting clawvisor_spend_cap", err, &resp.Diagnostics)
	}
}

func (r *spendCapResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("window"), req.ID)...)
}
