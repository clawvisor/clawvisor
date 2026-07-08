package provider

import (
	"context"

	"github.com/clawvisor/clawvisor/provider/internal/client"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// ssoKinds / ssoRoles are the accepted enums (mirrors the server).
var (
	ssoKinds = []string{"saml", "oidc"}
	ssoRoles = []string{"member", "admin"} // "owner" is deliberately rejected
)

type ssoConnectionResource struct {
	pd *providerData
}

type ssoConnectionModel struct {
	ID                 types.String `tfsdk:"id"`
	Kind               types.String `tfsdk:"kind"`
	SAMLEntityID       types.String `tfsdk:"saml_entity_id"`
	SAMLSSOURL         types.String `tfsdk:"saml_sso_url"`
	SAMLCertificatePEM types.String `tfsdk:"saml_certificate_pem"`
	OIDCIssuer         types.String `tfsdk:"oidc_issuer"`
	OIDCClientID       types.String `tfsdk:"oidc_client_id"`
	OIDCClientSecret   types.String `tfsdk:"oidc_client_secret"`
	JITProvision       types.Bool   `tfsdk:"jit_provision"`
	DefaultRole        types.String `tfsdk:"default_role"`
	EmailDomain        types.String `tfsdk:"email_domain"`
	SSOTeamAttribute   types.String `tfsdk:"sso_team_attribute"`
	Enabled            types.Bool   `tfsdk:"enabled"`
}

var (
	_ resource.Resource                   = (*ssoConnectionResource)(nil)
	_ resource.ResourceWithConfigure      = (*ssoConnectionResource)(nil)
	_ resource.ResourceWithImportState    = (*ssoConnectionResource)(nil)
	_ resource.ResourceWithValidateConfig = (*ssoConnectionResource)(nil)
)

// NewSSOConnectionResource is the resource factory.
func NewSSOConnectionResource() resource.Resource { return &ssoConnectionResource{} }

func (r *ssoConnectionResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_sso_connection"
}

func (r *ssoConnectionResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if pd, ok := configure(req, resp); ok {
		r.pd = pd
	}
}

func (r *ssoConnectionResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A per-org single sign-on connection (SAML or OIDC) — e.g. an Okta app. " +
			"This is a **Clawvisor Cloud / enterprise** resource: it requires the `sso` capability, so it errors " +
			"on an OSS-only deployment, and requires the provider's `org_id` to be set. It is a singleton — one " +
			"connection per org (the resource id is the org id).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The org id this connection belongs to (matches the provider's `org_id`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"kind": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "`oidc` or `saml`. Determines which of the `oidc_*` / `saml_*` attributes are required.",
			},
			"email_domain": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The IdP's verified email domain (e.g. `acme.com`) used to route sign-ins to this connection.",
			},
			"saml_entity_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "SAML IdP entity id. Required when `kind = \"saml\"`.",
			},
			"saml_sso_url": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "SAML IdP SSO (redirect) URL. Required when `kind = \"saml\"`.",
			},
			"saml_certificate_pem": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "SAML IdP signing certificate, PEM-encoded. Required when `kind = \"saml\"`.",
			},
			"oidc_issuer": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "OIDC issuer URL (e.g. `https://acme.okta.com`). Required when `kind = \"oidc\"`.",
			},
			"oidc_client_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "OIDC client id. Required when `kind = \"oidc\"`.",
			},
			"oidc_client_secret": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "OIDC client secret. Required when `kind = \"oidc\"`. Write-only: the value transits " +
					"Terraform state (use an encrypted state backend / write-only inputs), the server stores it encrypted " +
					"and never returns it, so it stays authoritative from configuration and is not refreshed on read.",
			},
			"jit_provision": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
				MarkdownDescription: "Just-in-time provision users on first SSO sign-in. Defaults to `false`.",
			},
			"default_role": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("member"),
				MarkdownDescription: "Role granted to JIT-provisioned users: `member` (default) or `admin`. `owner` is rejected.",
			},
			"sso_team_attribute": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "SAML/OIDC assertion attribute mapped to team membership (e.g. `groups`). Optional.",
			},
			"enabled": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Whether the connection is active. Defaults to `true`.",
			},
		},
	}
}

// ValidateConfig enforces the kind enum, the SAML-vs-OIDC required-field rules,
// and the owner-role rejection at plan time (mirrors the server so typos fail
// fast rather than at apply).
func (r *ssoConnectionResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg ssoConnectionModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	kind := cfg.Kind.ValueString()
	if !cfg.Kind.IsUnknown() && !cfg.Kind.IsNull() && !oneOf(kind, ssoKinds) {
		resp.Diagnostics.AddAttributeError(path.Root("kind"), "Invalid kind", "kind must be \"oidc\" or \"saml\".")
	}
	set := func(v types.String) bool { return !v.IsNull() && !v.IsUnknown() && v.ValueString() != "" }
	switch kind {
	case "oidc":
		if !set(cfg.OIDCIssuer) || !set(cfg.OIDCClientID) || !set(cfg.OIDCClientSecret) {
			resp.Diagnostics.AddError("Incomplete OIDC connection",
				"kind=\"oidc\" requires oidc_issuer, oidc_client_id, and oidc_client_secret.")
		}
		if set(cfg.SAMLEntityID) || set(cfg.SAMLSSOURL) || set(cfg.SAMLCertificatePEM) {
			resp.Diagnostics.AddError("Mixed SSO configuration", "kind=\"oidc\" must not set any saml_* attribute.")
		}
	case "saml":
		if !set(cfg.SAMLEntityID) || !set(cfg.SAMLSSOURL) || !set(cfg.SAMLCertificatePEM) {
			resp.Diagnostics.AddError("Incomplete SAML connection",
				"kind=\"saml\" requires saml_entity_id, saml_sso_url, and saml_certificate_pem.")
		}
		if set(cfg.OIDCIssuer) || set(cfg.OIDCClientID) || set(cfg.OIDCClientSecret) {
			resp.Diagnostics.AddError("Mixed SSO configuration", "kind=\"saml\" must not set any oidc_* attribute.")
		}
	}
	if role := cfg.DefaultRole.ValueString(); set(cfg.DefaultRole) && !oneOf(role, ssoRoles) {
		resp.Diagnostics.AddAttributeError(path.Root("default_role"), "Invalid default_role",
			"default_role must be \"member\" or \"admin\"; \"owner\" is not allowed for SSO JIT provisioning.")
	}
}

func (m ssoConnectionModel) toClient() client.SSOConnection {
	return client.SSOConnection{
		Kind:               m.Kind.ValueString(),
		SAMLEntityID:       m.SAMLEntityID.ValueString(),
		SAMLSSOURL:         m.SAMLSSOURL.ValueString(),
		SAMLCertificatePEM: m.SAMLCertificatePEM.ValueString(),
		OIDCIssuer:         m.OIDCIssuer.ValueString(),
		OIDCClientID:       m.OIDCClientID.ValueString(),
		OIDCClientSecret:   m.OIDCClientSecret.ValueString(),
		JITProvision:       m.JITProvision.ValueBool(),
		DefaultRole:        m.DefaultRole.ValueString(),
		EmailDomain:        m.EmailDomain.ValueString(),
		SSOTeamAttribute:   m.SSOTeamAttribute.ValueString(),
		Enabled:            m.Enabled.ValueBool(),
	}
}

// orgID returns the provider's configured org id, erroring if unset — the SSO
// connection is org-scoped and has no instance-level equivalent.
func (r *ssoConnectionResource) orgID(diags *diag.Diagnostics) (string, bool) {
	id := r.pd.client.Scope.OrgID
	if id == "" {
		diags.AddError("org_id is required",
			"clawvisor_sso_connection is an org-scoped enterprise resource — set org_id on the provider block.")
		return "", false
	}
	return id, true
}

func (r *ssoConnectionResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !requireCapability(r.pd, client.CapabilitySSO, "clawvisor_sso_connection", &resp.Diagnostics) {
		return
	}
	orgID, ok := r.orgID(&resp.Diagnostics)
	if !ok {
		return
	}
	var plan ssoConnectionModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.pd.client.PutSSOConnection(ctx, plan.toClient()); err != nil {
		diagFromError("Creating clawvisor_sso_connection", err, &resp.Diagnostics)
		return
	}
	plan.ID = types.StringValue(orgID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *ssoConnectionResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !requireCapability(r.pd, client.CapabilitySSO, "clawvisor_sso_connection", &resp.Diagnostics) {
		return
	}
	var state ssoConnectionModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	conn, err := r.pd.client.GetSSOConnection(ctx)
	if err != nil {
		if client.NotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		diagFromError("Reading clawvisor_sso_connection", err, &resp.Diagnostics)
		return
	}
	if conn == nil {
		// Server returns 200 + null body when no connection is configured.
		resp.State.RemoveResource(ctx)
		return
	}
	// Refresh from the server, but keep oidc_client_secret from prior state —
	// it is write-only (never returned), so config stays authoritative.
	state.Kind = types.StringValue(conn.Kind)
	state.SAMLEntityID = optString(conn.SAMLEntityID)
	state.SAMLSSOURL = optString(conn.SAMLSSOURL)
	state.SAMLCertificatePEM = optString(conn.SAMLCertificatePEM)
	state.OIDCIssuer = optString(conn.OIDCIssuer)
	state.OIDCClientID = optString(conn.OIDCClientID)
	state.JITProvision = types.BoolValue(conn.JITProvision)
	if conn.DefaultRole != "" {
		state.DefaultRole = types.StringValue(conn.DefaultRole)
	}
	state.EmailDomain = types.StringValue(conn.EmailDomain)
	state.SSOTeamAttribute = optString(conn.SSOTeamAttribute)
	state.Enabled = types.BoolValue(conn.Enabled)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *ssoConnectionResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !requireCapability(r.pd, client.CapabilitySSO, "clawvisor_sso_connection", &resp.Diagnostics) {
		return
	}
	var plan ssoConnectionModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.pd.client.PutSSOConnection(ctx, plan.toClient()); err != nil {
		diagFromError("Updating clawvisor_sso_connection", err, &resp.Diagnostics)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *ssoConnectionResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if err := r.pd.client.DeleteSSOConnection(ctx); err != nil {
		if client.NotFound(err) {
			return
		}
		diagFromError("Deleting clawvisor_sso_connection", err, &resp.Diagnostics)
	}
}

func (r *ssoConnectionResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, pathRoot("id"), req, resp)
}

// optString maps an empty string to null so optional attributes don't churn.
func optString(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}
