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

type apiTokenResource struct {
	pd *providerData
}

type apiTokenModel struct {
	ID        types.String `tfsdk:"id"`
	Name      types.String `tfsdk:"name"`
	Scope     types.String `tfsdk:"scope"`
	ExpiresAt types.String `tfsdk:"expires_at"`
	Token     types.String `tfsdk:"token"`
}

var (
	_ resource.Resource                = (*apiTokenResource)(nil)
	_ resource.ResourceWithConfigure   = (*apiTokenResource)(nil)
	_ resource.ResourceWithImportState = (*apiTokenResource)(nil)
)

// NewAPITokenResource is the resource factory.
func NewAPITokenResource() resource.Resource { return &apiTokenResource{} }

func (r *apiTokenResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_api_token"
}

func (r *apiTokenResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if pd, ok := configure(req, resp); ok {
		r.pd = pd
	}
}

func (r *apiTokenResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A long-lived, scoped, revocable Clawvisor API token (spec 05). The token " +
			"is a single-scope credential; there is no server-side update, so any change forces replacement.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Server-assigned token id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Human-readable token name. Changing it forces replacement.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"scope": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("instance-admin"),
				MarkdownDescription: "Token scope. One of `instance-admin`, `config-write`, `config-read` (05-lite issues only `instance-admin`). Changing it forces replacement.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"expires_at": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Optional RFC3339 expiry. Changing it forces replacement.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"token": schema.StringAttribute{
				Computed:            true,
				Sensitive:           true,
				MarkdownDescription: "The `cvat_` token plaintext, returned only at create time and stored in state; treat state as sensitive.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *apiTokenResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !requireCapability(r.pd, client.CapabilityAPITokens, "clawvisor_api_token", &resp.Diagnostics) {
		return
	}
	var plan apiTokenModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tok, err := r.pd.client.CreateToken(ctx, client.CreateTokenRequest{
		Name:      plan.Name.ValueString(),
		Scope:     plan.Scope.ValueString(),
		ExpiresAt: plan.ExpiresAt.ValueString(),
	})
	if err != nil {
		diagFromError("Creating clawvisor_api_token", err, &resp.Diagnostics)
		return
	}

	plan.ID = types.StringValue(tok.ID)
	plan.Scope = types.StringValue(tok.Scope)
	plan.Token = types.StringValue(tok.Token)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *apiTokenResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !requireCapability(r.pd, client.CapabilityAPITokens, "clawvisor_api_token", &resp.Diagnostics) {
		return
	}
	var state apiTokenModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tok, err := r.pd.client.GetToken(ctx, state.ID.ValueString())
	if err != nil {
		if client.NotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		diagFromError("Reading clawvisor_api_token", err, &resp.Diagnostics)
		return
	}
	// name/scope are drift-detectable; expires_at is not read back (immutable
	// server-side and RFC3339 formatting would perma-diff). token is create-only.
	state.Name = types.StringValue(tok.Name)
	state.Scope = types.StringValue(tok.Scope)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update never runs: every mutable attribute is RequiresReplace. It is
// implemented as a no-op copy to satisfy the interface.
func (r *apiTokenResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan apiTokenModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *apiTokenResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state apiTokenModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.pd.client.DeleteToken(ctx, state.ID.ValueString()); err != nil {
		if client.NotFound(err) {
			return
		}
		diagFromError("Deleting clawvisor_api_token", err, &resp.Diagnostics)
	}
}

func (r *apiTokenResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, pathRoot("id"), req, resp)
}
