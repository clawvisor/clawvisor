package provider

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/clawvisor/clawvisor/provider/internal/client"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type serviceConfigResource struct {
	pd *providerData
}

type serviceConfigModel struct {
	ID        types.String         `tfsdk:"id"`
	ServiceID types.String         `tfsdk:"service_id"`
	Alias     types.String         `tfsdk:"alias"`
	Config    jsontypes.Normalized `tfsdk:"config"`
}

var (
	_ resource.Resource                = (*serviceConfigResource)(nil)
	_ resource.ResourceWithConfigure   = (*serviceConfigResource)(nil)
	_ resource.ResourceWithImportState = (*serviceConfigResource)(nil)
)

// NewServiceConfigResource is the resource factory.
func NewServiceConfigResource() resource.Resource { return &serviceConfigResource{} }

func (r *serviceConfigResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_service_config"
}

func (r *serviceConfigResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if pd, ok := configure(req, resp); ok {
		r.pd = pd
	}
}

func (r *serviceConfigResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Per-service configuration for a configurable adapter. `config` is an opaque " +
			"JSON document stored verbatim; key ordering never causes a diff (semantic JSON equality).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Composite id `service_id:alias`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"service_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The service/adapter id (e.g. `github`). Changing it forces replacement.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"alias": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("default"),
				MarkdownDescription: "Config alias (defaults to `default`). Changing it forces replacement.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"config": schema.StringAttribute{
				Required:            true,
				CustomType:          jsontypes.NormalizedType{},
				MarkdownDescription: "The service config as a JSON object string.",
			},
		},
	}
}

func serviceConfigID(serviceID, alias string) string { return serviceID + ":" + alias }

func (r *serviceConfigResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan serviceConfigModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	sc, err := r.pd.client.PutServiceConfig(ctx, plan.ServiceID.ValueString(), plan.Alias.ValueString(), json.RawMessage(plan.Config.ValueString()))
	if err != nil {
		diagFromError("Creating clawvisor_service_config", err, &resp.Diagnostics)
		return
	}
	plan.ID = types.StringValue(serviceConfigID(plan.ServiceID.ValueString(), plan.Alias.ValueString()))
	plan.Config = jsontypes.NewNormalizedValue(string(sc.Config))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *serviceConfigResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state serviceConfigModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	sc, err := r.pd.client.GetServiceConfig(ctx, state.ServiceID.ValueString(), state.Alias.ValueString())
	if err != nil {
		if client.NotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		diagFromError("Reading clawvisor_service_config", err, &resp.Diagnostics)
		return
	}
	state.ID = types.StringValue(serviceConfigID(state.ServiceID.ValueString(), state.Alias.ValueString()))
	state.Config = jsontypes.NewNormalizedValue(string(sc.Config))
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *serviceConfigResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan serviceConfigModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	sc, err := r.pd.client.PutServiceConfig(ctx, plan.ServiceID.ValueString(), plan.Alias.ValueString(), json.RawMessage(plan.Config.ValueString()))
	if err != nil {
		diagFromError("Updating clawvisor_service_config", err, &resp.Diagnostics)
		return
	}
	plan.ID = types.StringValue(serviceConfigID(plan.ServiceID.ValueString(), plan.Alias.ValueString()))
	plan.Config = jsontypes.NewNormalizedValue(string(sc.Config))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *serviceConfigResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state serviceConfigModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.pd.client.DeleteServiceConfig(ctx, state.ServiceID.ValueString(), state.Alias.ValueString()); err != nil {
		if client.NotFound(err) {
			return
		}
		diagFromError("Deleting clawvisor_service_config", err, &resp.Diagnostics)
	}
}

func (r *serviceConfigResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import id is `service_id:alias` (alias defaults to "default" if omitted).
	serviceID, alias, found := strings.Cut(req.ID, ":")
	if !found || alias == "" {
		alias = "default"
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), serviceConfigID(serviceID, alias))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("service_id"), serviceID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("alias"), alias)...)
}
