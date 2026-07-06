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

type contentPolicyResource struct {
	pd *providerData
}

type contentPolicyModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	Pattern      types.String `tfsdk:"pattern"`
	PatternKind  types.String `tfsdk:"pattern_kind"`
	Action       types.String `tfsdk:"action"`
	BlockMessage types.String `tfsdk:"block_message"`
	Enabled      types.Bool   `tfsdk:"enabled"`
}

var (
	_ resource.Resource                = (*contentPolicyResource)(nil)
	_ resource.ResourceWithConfigure   = (*contentPolicyResource)(nil)
	_ resource.ResourceWithImportState = (*contentPolicyResource)(nil)
)

// NewContentPolicyResource is the resource factory.
func NewContentPolicyResource() resource.Resource { return &contentPolicyResource{} }

func (r *contentPolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_content_policy"
}

func (r *contentPolicyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if pd, ok := configure(req, resp); ok {
		r.pd = pd
	}
}

func (r *contentPolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A content-scanning rule. `block` rules stop the request with `block_message`; " +
			"`flag` rules accumulate the rule name for observation. Requires the `local_governance` " +
			"capability (spec 06a) on OSS.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Server-assigned policy id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Human-readable rule name (surfaced in flagged lists).",
			},
			"pattern": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The match pattern. Regex patterns must compile and be <=256 chars.",
			},
			"pattern_kind": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "`regex` or `keyword` (case-insensitive substring).",
			},
			"action": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "`block` or `flag`.",
			},
			"block_message": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(""),
				MarkdownDescription: "Admin-authored message returned to the caller on a block. Defaults to empty.",
			},
			"enabled": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Whether the rule is active. Defaults to `true`.",
			},
		},
	}
}

func (m contentPolicyModel) toClient() client.ContentPolicy {
	return client.ContentPolicy{
		ID:           m.ID.ValueString(),
		Name:         m.Name.ValueString(),
		Pattern:      m.Pattern.ValueString(),
		PatternKind:  m.PatternKind.ValueString(),
		Action:       m.Action.ValueString(),
		BlockMessage: m.BlockMessage.ValueString(),
		Enabled:      m.Enabled.ValueBool(),
	}
}

func (r *contentPolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !requireCapability(r.pd, client.CapabilityLocalGovernance, "clawvisor_content_policy", &resp.Diagnostics) {
		return
	}
	var plan contentPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	cp, err := r.pd.client.CreateContentPolicy(ctx, plan.toClient())
	if err != nil {
		diagFromError("Creating clawvisor_content_policy", err, &resp.Diagnostics)
		return
	}
	plan.ID = types.StringValue(cp.ID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *contentPolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !requireCapability(r.pd, client.CapabilityLocalGovernance, "clawvisor_content_policy", &resp.Diagnostics) {
		return
	}
	var state contentPolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	cp, err := r.pd.client.GetContentPolicy(ctx, state.ID.ValueString())
	if err != nil {
		if client.NotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		diagFromError("Reading clawvisor_content_policy", err, &resp.Diagnostics)
		return
	}
	state.Name = types.StringValue(cp.Name)
	state.Pattern = types.StringValue(cp.Pattern)
	state.PatternKind = types.StringValue(cp.PatternKind)
	state.Action = types.StringValue(cp.Action)
	state.BlockMessage = types.StringValue(cp.BlockMessage)
	state.Enabled = types.BoolValue(cp.Enabled)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *contentPolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan contentPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, err := r.pd.client.UpdateContentPolicy(ctx, plan.ID.ValueString(), plan.toClient()); err != nil {
		diagFromError("Updating clawvisor_content_policy", err, &resp.Diagnostics)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *contentPolicyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state contentPolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.pd.client.DeleteContentPolicy(ctx, state.ID.ValueString()); err != nil {
		if client.NotFound(err) {
			return
		}
		diagFromError("Deleting clawvisor_content_policy", err, &resp.Diagnostics)
	}
}

func (r *contentPolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, pathRoot("id"), req, resp)
}
