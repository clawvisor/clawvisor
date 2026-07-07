package provider

import (
	"context"
	"strings"

	"github.com/clawvisor/clawvisor/provider/internal/client"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// llmProviders is the set of upstream LLM providers a credential may target.
// These ids are the vault reserved namespace the generic clawvisor_vault_entry /
// clawvisor_vault_reference resources REJECT — this resource is the only way to
// set them.
var llmProviders = []string{"anthropic", "openai", "google"}

// llmReferenceBackends is the set of external-secret backends a reference may
// name (matches the server's resolver registry).
var llmReferenceBackends = []string{"aws-sm", "gcp-sm"}

// llmCredentialReferenceVerify is the fail-fast dry-run resolve applied on the
// reference path. It mirrors clawvisor_vault_reference's default (true): the
// server resolves the target once on apply and fails with an actionable error
// if it cannot be read, so a typo'd ARN / missing grant surfaces at apply time
// rather than silently at the first injection.
const llmCredentialReferenceVerify = true

type llmCredentialResource struct {
	pd *providerData
}

type llmCredentialModel struct {
	ID        types.String           `tfsdk:"id"`
	Provider  types.String           `tfsdk:"llm_provider"`
	AgentID   types.String           `tfsdk:"agent_id"`
	APIKey    types.String           `tfsdk:"api_key"`
	Reference *llmCredentialRefModel `tfsdk:"reference"`
}

type llmCredentialRefModel struct {
	Backend types.String `tfsdk:"backend"`
	RefID   types.String `tfsdk:"ref_id"`
	JSONKey types.String `tfsdk:"json_key"`
}

var (
	_ resource.Resource                   = (*llmCredentialResource)(nil)
	_ resource.ResourceWithConfigure      = (*llmCredentialResource)(nil)
	_ resource.ResourceWithImportState    = (*llmCredentialResource)(nil)
	_ resource.ResourceWithValidateConfig = (*llmCredentialResource)(nil)
)

// NewLLMCredentialResource is the resource factory.
func NewLLMCredentialResource() resource.Resource { return &llmCredentialResource{} }

func (r *llmCredentialResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_llm_credential"
}

func (r *llmCredentialResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if pd, ok := configure(req, resp); ok {
		r.pd = pd
	}
}

func (r *llmCredentialResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An upstream LLM provider API key (anthropic / openai / google) the proxy injects " +
			"server-side. This is the govern \"org provider key\": with an instance-admin token it is stored under " +
			"the shared instance scope and resolves as the fallback for every agent. Provider ids are a reserved vault " +
			"namespace, so `clawvisor_vault_entry` / `clawvisor_vault_reference` REJECT them — use this resource for " +
			"provider keys and those for generic service credentials (github, etc.).\n\n" +
			"Supply exactly one of `api_key` (push) or `reference` (point at a secret in your own store). In push mode " +
			"the key transits Terraform state — use an encrypted state backend and, where supported, write-only/" +
			"ephemeral inputs (PRD §7). In reference mode no secret ever enters state.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`<provider>` for an instance-shared key, or `<provider>:agent:<agent_id>` for an agent-scoped one.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"llm_provider": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The LLM provider: `anthropic`, `openai`, or `google`. (Named `llm_provider` rather " +
					"than `provider` because `provider` is a reserved Terraform meta-argument.) Changing it forces replacement.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"agent_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "When set, the credential is scoped to this agent (`agent:<id>:<provider>`), which the " +
					"forwarder prefers over the shared key for that agent. The agent must be owned by the token's principal " +
					"(a Terraform-managed `clawvisor_agent` is owned by the instance, so an instance-admin token can set its " +
					"agent-scoped key). When unset, the credential is instance-shared. Changing it forces replacement.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"api_key": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Push mode: the literal provider key (e.g. `sk-ant-…`, `sk-…`, `AIza…`). The value " +
					"transits Terraform state (Sensitive) — use an encrypted state backend and write-only/ephemeral inputs. " +
					"The server never returns it, so it stays authoritative from configuration and is null on import. " +
					"Mutually exclusive with `reference`.",
			},
			"reference": schema.SingleNestedAttribute{
				Optional: true,
				MarkdownDescription: "Reference mode: point at a secret in your own store (resolved to plaintext only at " +
					"injection time, never persisted). Mutually exclusive with `api_key`. The target must match the server's " +
					"`vault.reference_allowlist` and the instance identity must have read access.",
				Attributes: map[string]schema.Attribute{
					"backend": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "The external secret backend: `aws-sm` or `gcp-sm`.",
					},
					"ref_id": schema.StringAttribute{
						Required: true,
						MarkdownDescription: "The backend-specific locator: an ARN (`aws-sm`) or a full resource name " +
							"`projects/{p}/secrets/{s}` (`gcp-sm`; append `/versions/N` to pin, else `latest`). Must match the allowlist.",
					},
					"json_key": schema.StringAttribute{
						Optional:            true,
						MarkdownDescription: "When set, the fetched secret is parsed as JSON and this key extracted; when empty, the raw secret bytes are used.",
					},
				},
			},
		},
	}
}

// ValidateConfig enforces the provider/backend enums and the api_key XOR
// reference rule at plan time. The validators module is not a dependency, so
// these are hand-rolled rather than declared.
func (r *llmCredentialResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg llmCredentialModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !cfg.Provider.IsUnknown() && !cfg.Provider.IsNull() && !oneOf(cfg.Provider.ValueString(), llmProviders) {
		resp.Diagnostics.AddAttributeError(path.Root("llm_provider"),
			"Invalid provider", "provider must be one of anthropic, openai, or google.")
	}

	hasKey := !cfg.APIKey.IsNull() && !cfg.APIKey.IsUnknown()
	hasRef := cfg.Reference != nil
	if hasKey == hasRef {
		resp.Diagnostics.AddError("Invalid clawvisor_llm_credential configuration",
			"Exactly one of api_key or reference must be set.")
	}

	if hasRef {
		b := cfg.Reference.Backend
		if !b.IsUnknown() && !b.IsNull() && !oneOf(b.ValueString(), llmReferenceBackends) {
			resp.Diagnostics.AddAttributeError(path.Root("reference").AtName("backend"),
				"Invalid reference backend", "reference.backend must be one of aws-sm or gcp-sm.")
		}
	}
}

func oneOf(v string, set []string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

// llmCredentialID composes the computed id / import handle.
func llmCredentialID(provider, agentID string) string {
	if agentID != "" {
		return provider + ":agent:" + agentID
	}
	return provider
}

// write stores the credential per the configured mode (push or reference).
func (r *llmCredentialResource) write(ctx context.Context, plan llmCredentialModel) error {
	provider := plan.Provider.ValueString()
	agentID := plan.AgentID.ValueString()
	if plan.Reference != nil {
		ref := client.LLMCredentialInput{
			Backend: plan.Reference.Backend.ValueString(),
			RefID:   plan.Reference.RefID.ValueString(),
			JSONKey: plan.Reference.JSONKey.ValueString(),
		}
		return r.pd.client.SetLLMCredentialReference(ctx, provider, agentID, ref, llmCredentialReferenceVerify)
	}
	return r.pd.client.SetLLMCredential(ctx, provider, agentID, plan.APIKey.ValueString())
}

func (r *llmCredentialResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !requireCapability(r.pd, client.CapabilitySecretVault, "clawvisor_llm_credential", &resp.Diagnostics) {
		return
	}
	var plan llmCredentialModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.write(ctx, plan); err != nil {
		diagFromError("Creating clawvisor_llm_credential", err, &resp.Diagnostics)
		return
	}
	plan.ID = types.StringValue(llmCredentialID(plan.Provider.ValueString(), plan.AgentID.ValueString()))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *llmCredentialResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !requireCapability(r.pd, client.CapabilitySecretVault, "clawvisor_llm_credential", &resp.Diagnostics) {
		return
	}
	var state llmCredentialModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Existence check only. The GET never returns the secret value, so api_key /
	// reference stay authoritative from config/state and Read must not rewrite
	// them. If the entry is gone out-of-band (including its agent), drop it from
	// state so the next apply recreates it.
	exists, err := r.pd.client.LLMCredentialExists(ctx, state.Provider.ValueString(), state.AgentID.ValueString())
	if err != nil {
		if client.NotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		diagFromError("Reading clawvisor_llm_credential", err, &resp.Diagnostics)
		return
	}
	if !exists {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *llmCredentialResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !requireCapability(r.pd, client.CapabilitySecretVault, "clawvisor_llm_credential", &resp.Diagnostics) {
		return
	}
	var plan llmCredentialModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// provider / agent_id are RequiresReplace; only api_key / reference change
	// in place, and PUT overwrites either kind.
	if err := r.write(ctx, plan); err != nil {
		diagFromError("Updating clawvisor_llm_credential", err, &resp.Diagnostics)
		return
	}
	plan.ID = types.StringValue(llmCredentialID(plan.Provider.ValueString(), plan.AgentID.ValueString()))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *llmCredentialResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !requireCapability(r.pd, client.CapabilitySecretVault, "clawvisor_llm_credential", &resp.Diagnostics) {
		return
	}
	var state llmCredentialModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.pd.client.DeleteLLMCredential(ctx, state.Provider.ValueString(), state.AgentID.ValueString()); err != nil {
		if client.NotFound(err) {
			return
		}
		diagFromError("Deleting clawvisor_llm_credential", err, &resp.Diagnostics)
	}
}

// ImportState parses `<provider>` or `<provider>:agent:<id>`. api_key /
// reference cannot be recovered (the server is write-only) and are null on
// import; supply them in configuration afterward.
func (r *llmCredentialResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id := strings.TrimSpace(req.ID)
	provider := id
	agentID := ""
	if p, a, ok := strings.Cut(id, ":agent:"); ok {
		provider, agentID = p, a
	}
	if provider == "" || !oneOf(provider, llmProviders) {
		resp.Diagnostics.AddError("Invalid import id",
			"Import id must be \"<provider>\" or \"<provider>:agent:<agent_id>\" where provider is anthropic, openai, or google.")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), llmCredentialID(provider, agentID))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("llm_provider"), provider)...)
	if agentID != "" {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("agent_id"), agentID)...)
	}
}
