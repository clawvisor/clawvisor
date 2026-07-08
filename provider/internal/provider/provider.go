// Package provider implements the terraform-plugin-framework provider for
// Clawvisor. It wraps the existing REST API (spec 06b): core resources work
// against both OSS (instance-scoped) and cloud (org-scoped via org_id);
// capability-gated resources fail fast when the server does not report the
// capability they need.
package provider

import (
	"context"
	"net/http"
	"os"

	"github.com/clawvisor/clawvisor/provider/internal/client"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// providerData is handed to every resource's Configure. It carries the REST
// client and the capabilities negotiated once at Configure time.
type providerData struct {
	client   *client.Client
	features *client.Features
	endpoint string
}

// clawvisorProvider is the provider implementation.
type clawvisorProvider struct {
	version string
	// httpClient is overridable for tests (unit tests point it at httptest).
	httpClient *http.Client
}

var _ provider.Provider = (*clawvisorProvider)(nil)

// New returns a provider.Provider factory for the given version string.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &clawvisorProvider{version: version}
	}
}

// NewWithHTTPClient is like New but injects an *http.Client (used by tests).
func NewWithHTTPClient(version string, hc *http.Client) func() provider.Provider {
	return func() provider.Provider {
		return &clawvisorProvider{version: version, httpClient: hc}
	}
}

func (p *clawvisorProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "clawvisor"
	resp.Version = p.version
}

type providerModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	APIToken types.String `tfsdk:"api_token"`
	OrgID    types.String `tfsdk:"org_id"`
}

func (p *clawvisorProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The Clawvisor provider configures a Clawvisor instance (OSS) or " +
			"organization (Cloud) declaratively over its REST API. Authenticate with a " +
			"long-lived `cvat_` API token (spec 05).",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Base URL of the Clawvisor server, e.g. " +
					"`https://clawvisor.internal:25297`. May also be set via the " +
					"`CLAWVISOR_ENDPOINT` environment variable.",
			},
			"api_token": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "A `cvat_` API token with the `instance-admin` scope. " +
					"May also be set via the `CLAWVISOR_API_TOKEN` environment variable. " +
					"Marked sensitive; it grants full instance configuration authority.",
			},
			"org_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Organization id (Clawvisor Cloud only). When set, " +
					"governance and org-scoped resources route to `/api/orgs/{org_id}/...`; " +
					"when omitted, resources use the instance-scoped OSS routes.",
			},
		},
	}
}

func (p *clawvisorProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	endpoint := firstNonEmpty(cfg.Endpoint.ValueString(), os.Getenv("CLAWVISOR_ENDPOINT"))
	token := firstNonEmpty(cfg.APIToken.ValueString(), os.Getenv("CLAWVISOR_API_TOKEN"))
	orgID := cfg.OrgID.ValueString()

	if endpoint == "" {
		resp.Diagnostics.AddAttributeError(
			pathRoot("endpoint"),
			"Missing Clawvisor endpoint",
			"Set the provider `endpoint` attribute or the CLAWVISOR_ENDPOINT environment variable "+
				"to the base URL of your Clawvisor server.",
		)
	}
	if token == "" {
		resp.Diagnostics.AddAttributeError(
			pathRoot("api_token"),
			"Missing Clawvisor API token",
			"Set the provider `api_token` attribute or the CLAWVISOR_API_TOKEN environment variable "+
				"to a cvat_ API token. On a fresh instance, the deploy module's bootstrap token "+
				"mints one via clawvisor_api_token (see the two-phase bootstrap example).",
		)
	}
	if resp.Diagnostics.HasError() {
		return
	}

	c := client.New(endpoint, token, orgID, p.httpClient)

	features, err := c.Features(ctx)
	if err != nil {
		if ae, ok := err.(*client.APIError); ok && (ae.StatusCode == http.StatusUnauthorized || ae.StatusCode == http.StatusForbidden) {
			resp.Diagnostics.AddError(
				"Clawvisor authentication failed",
				"GET /api/features on "+endpoint+" returned "+ae.Error()+". "+
					"Check that api_token is a valid, non-revoked cvat_ token with the instance-admin scope.",
			)
			return
		}
		resp.Diagnostics.AddError(
			"Cannot reach the Clawvisor server",
			"GET /api/features on "+endpoint+" failed: "+err.Error()+". "+
				"Check the endpoint URL and network connectivity.",
		)
		return
	}

	pd := &providerData{client: c, features: features, endpoint: endpoint}
	resp.ResourceData = pd
	resp.DataSourceData = pd
}

func (p *clawvisorProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewAgentResource,
		NewUserResource,
		NewServiceConfigResource,
		NewVaultEntryResource,
		NewVaultReferenceResource,
		NewLLMCredentialResource,
		NewAPITokenResource,
		NewModelPolicyResource,
		NewSpendCapResource,
		NewContentPolicyResource,
		NewTaskPolicyResource,
		NewSSOConnectionResource,
		NewOrgSettingsResource,
		NewOrgResource,
		NewOrgTokenResource,
	}
}

func (p *clawvisorProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	// v1 has no data sources (PRD §9.1 forbids telemetry/audit reads).
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
