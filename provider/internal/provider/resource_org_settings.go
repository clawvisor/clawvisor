package provider

import (
	"context"

	"github.com/clawvisor/clawvisor/provider/internal/client"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type orgSettingsResource struct {
	pd *providerData
}

type orgSettingsModel struct {
	ID                types.String `tfsdk:"id"`
	MemberSelfService types.Bool   `tfsdk:"member_self_service"`
}

var (
	_ resource.Resource                = (*orgSettingsResource)(nil)
	_ resource.ResourceWithConfigure   = (*orgSettingsResource)(nil)
	_ resource.ResourceWithImportState = (*orgSettingsResource)(nil)
)

// NewOrgSettingsResource is the resource factory.
func NewOrgSettingsResource() resource.Resource { return &orgSettingsResource{} }

func (r *orgSettingsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_settings"
}

func (r *orgSettingsResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if pd, ok := configure(req, resp); ok {
		r.pd = pd
	}
}

func (r *orgSettingsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Org-admin-configurable settings for a single org (a singleton per org; `id` is the " +
			"org id). Org-scoped — set the provider `org_id`. Available on Clawvisor Cloud / enterprise orgs; the " +
			"settings API is not served by an OSS-only deployment. Unlike SSO this is a basic org setting, not an " +
			"enterprise-gated one — any org admin can set it.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The org id (settings are a singleton per org).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"member_self_service": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether non-admin members may run the personal connect flows (connect agents, " +
					"activate services). Defaults to `true`; set `false` to centrally manage — members then get " +
					"403 `SELF_SERVICE_DISABLED` on those routes and the dashboard hides the flows.",
			},
		},
	}
}

// requireOrg errors when the provider isn't org-scoped: org settings have no
// instance-scoped equivalent.
func (r *orgSettingsResource) requireOrg(diags *diag.Diagnostics) (string, bool) {
	orgID := r.pd.client.Scope.OrgID
	if orgID == "" {
		diags.AddError("clawvisor_org_settings requires an org-scoped provider",
			"Set `org_id` on the clawvisor provider — org settings are only available on Clawvisor Cloud / enterprise orgs.")
		return "", false
	}
	return orgID, true
}

func (r *orgSettingsResource) put(ctx context.Context, orgID string, m *orgSettingsModel, diags *diag.Diagnostics, op string) {
	out, err := r.pd.client.PutOrgSettings(ctx, client.OrgSettings{MemberSelfService: m.MemberSelfService.ValueBool()})
	if err != nil {
		diagFromError(op+" clawvisor_org_settings", err, diags)
		return
	}
	m.ID = types.StringValue(orgID)
	m.MemberSelfService = types.BoolValue(out.MemberSelfService)
}

func (r *orgSettingsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	orgID, ok := r.requireOrg(&resp.Diagnostics)
	if !ok {
		return
	}
	var plan orgSettingsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.put(ctx, orgID, &plan, &resp.Diagnostics, "Creating")
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *orgSettingsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	orgID, ok := r.requireOrg(&resp.Diagnostics)
	if !ok {
		return
	}
	var state orgSettingsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.pd.client.GetOrgSettings(ctx)
	if err != nil {
		if client.NotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		diagFromError("Reading clawvisor_org_settings", err, &resp.Diagnostics)
		return
	}
	state.ID = types.StringValue(orgID)
	state.MemberSelfService = types.BoolValue(out.MemberSelfService)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *orgSettingsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	orgID, ok := r.requireOrg(&resp.Diagnostics)
	if !ok {
		return
	}
	var plan orgSettingsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.put(ctx, orgID, &plan, &resp.Diagnostics, "Updating")
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete resets member self-service to the default (true) — settings are a
// per-org singleton with no "absent" state; removing the resource just
// restores the default.
func (r *orgSettingsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if _, ok := r.requireOrg(&resp.Diagnostics); !ok {
		return
	}
	if _, err := r.pd.client.PutOrgSettings(ctx, client.OrgSettings{MemberSelfService: true}); err != nil {
		if client.NotFound(err) {
			return
		}
		diagFromError("Deleting clawvisor_org_settings", err, &resp.Diagnostics)
	}
}

func (r *orgSettingsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, pathRoot("id"), req, resp)
}
