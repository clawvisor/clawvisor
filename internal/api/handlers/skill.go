package handlers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/mcpadapter"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// isMCPService reports whether the given base service ID corresponds to an
// MCPAdapter — used by the catalog renderer to apply MCP-specific rules
// (one-line summarization, per-server byte budget).
func (h *SkillHandler) isMCPService(serviceID string) bool {
	a, ok := h.adapterReg.Get(serviceID)
	if !ok {
		return false
	}
	_, ok = a.(*mcpadapter.MCPAdapter)
	return ok
}

// writeActionDetail serves the byte-exact original description of a single
// MCP tool. This is the "safety net" call site for an agent that read the
// compact catalog summary and needs full docs before invoking a tool.
//
// Output is NOT Markdown-wrapped — we serve the description verbatim so
// the agent gets exactly what the MCP server documented.
//
// Honors the same restriction filter as the overview/detail views: an
// action that's restricted (and therefore omitted from the catalog list)
// must not be readable via this endpoint either. Otherwise an agent could
// learn from full docs of an action it isn't allowed to call.
func (h *SkillHandler) writeActionDetail(buf *strings.Builder, ctx context.Context, serviceID, action string, entries []*catalogEntry, adapterMeta map[string]adapters.ServiceMetadata, userID string) {
	baseID, _ := parseServiceAliasSkill(serviceID)
	// Only allow this view for services the user actually has activated.
	var entry *catalogEntry
	for _, e := range entries {
		if e.baseID == baseID {
			entry = e
			break
		}
	}
	if entry == nil {
		buf.WriteString(fmt.Sprintf("Service %q is not activated for this user.\n", serviceID))
		return
	}
	if h.isRestricted(ctx, userID, entry, action) {
		buf.WriteString(fmt.Sprintf("Action %q is restricted on service %q.\n", action, serviceID))
		return
	}
	meta, ok := adapterMeta[baseID]
	if !ok {
		buf.WriteString(fmt.Sprintf("Service %q has no metadata.\n", serviceID))
		return
	}
	am, ok := meta.ActionMeta[action]
	if !ok {
		buf.WriteString(fmt.Sprintf("Action %q not found on service %q.\n", action, serviceID))
		return
	}
	if am.Description == "" {
		buf.WriteString(fmt.Sprintf("(no description provided for %s:%s)\n", serviceID, action))
		return
	}
	buf.WriteString(am.Description)
}

// parseServiceAliasSkill mirrors parseServiceAlias used elsewhere — split
// "service:alias" → ("service", "alias"). Defined locally so this file
// doesn't reach across to other handlers.
func parseServiceAliasSkill(s string) (string, string) {
	if i := strings.Index(s, ":"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// LocalServiceProvider supplies local daemon services for the agent catalog.
// Implemented by the cloud layer; nil in self-hosted mode.
type LocalServiceProvider interface {
	ActiveLocalServices(ctx context.Context, userID string) ([]LocalCatalogService, error)
}

// LocalCatalogService describes a local daemon service for the agent catalog.
type LocalCatalogService struct {
	ServiceID   string
	DaemonName  string
	Name        string
	Description string
	Actions     []LocalCatalogAction
}

// LocalCatalogAction describes an action within a local service.
type LocalCatalogAction struct {
	ID          string
	Name        string
	Description string
	Params      []LocalCatalogParam
}

// LocalCatalogParam describes a parameter for a local action.
type LocalCatalogParam struct {
	Name        string
	Type        string
	Required    bool
	Description string
}

// SkillHandler serves the dynamic skill catalog endpoint.
type SkillHandler struct {
	st               store.Store
	vault            vault.Vault
	adapterReg       *adapters.Registry
	logger           *slog.Logger
	localSvcProvider LocalServiceProvider
}

// SetLocalServiceProvider configures the local daemon service provider.
func (h *SkillHandler) SetLocalServiceProvider(p LocalServiceProvider) {
	h.localSvcProvider = p
}

func NewSkillHandler(
	st store.Store,
	v vault.Vault,
	adapterReg *adapters.Registry,
	logger *slog.Logger,
) *SkillHandler {
	return &SkillHandler{
		st: st, vault: v, adapterReg: adapterReg, logger: logger,
	}
}

// catalogEntry groups all activated aliases for a single base service.
type catalogEntry struct {
	baseID  string   // adapter service ID, e.g. "google.gmail"
	aliases []string // activated display IDs (e.g. ["google.gmail", "google.gmail:personal"])
	actions []string
}

// Catalog returns a personalized markdown document listing only the agent's
// activated services (with restriction-filtered actions).
//
// GET /api/skill/catalog
// GET /api/skill/catalog?service=google.gmail   (detailed single-service view)
// Auth: agent bearer token
func (h *SkillHandler) Catalog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	agent := middleware.AgentFromContext(ctx)
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	// Determine which vault keys are activated for this user.
	activatedKeys, _ := h.vault.List(ctx, agent.UserID)
	keySet := make(map[string]bool, len(activatedKeys))
	for _, k := range activatedKeys {
		keySet[k] = true
	}

	// Load service metas for alias display.
	metas, _ := h.st.ListServiceMetas(ctx, agent.UserID)

	// Build the adapter metadata index for action descriptions.
	adapterMeta := map[string]adapters.ServiceMetadata{} // serviceID → metadata
	allAdapters := h.adapterReg.AllForUser(ctx, agent.UserID)
	for _, a := range allAdapters {
		if mp, ok := a.(adapters.MetadataProvider); ok {
			adapterMeta[a.ServiceID()] = mp.ServiceMetadata()
		}
	}

	// Collect activated services, grouped by base service ID.
	entries := map[string]*catalogEntry{} // baseID → entry
	for _, a := range allAdapters {
		if ac, ok := a.(adapters.AvailabilityChecker); ok && !ac.Available() {
			continue
		}
		baseID := a.ServiceID()
		credentialFree := a.ValidateCredential(nil) == nil
		if credentialFree {
			for _, m := range metas {
				if m.ServiceID == baseID {
					entries[baseID] = &catalogEntry{
						baseID: baseID, aliases: []string{baseID},
						actions: a.SupportedActions(),
					}
					break
				}
			}
			continue
		}

		shown := false
		for _, m := range metas {
			if m.ServiceID != baseID {
				continue
			}
			vKey := h.adapterReg.VaultKeyWithAlias(baseID, m.Alias)
			if !keySet[vKey] {
				continue
			}
			shown = true
			displayID := baseID
			if m.Alias != "default" {
				displayID = baseID + ":" + m.Alias
			}
			if e, ok := entries[baseID]; ok {
				e.aliases = append(e.aliases, displayID)
			} else {
				entries[baseID] = &catalogEntry{
					baseID: baseID, aliases: []string{displayID},
					actions: a.SupportedActions(),
				}
			}
		}

		if !shown {
			baseKey := h.adapterReg.VaultKey(baseID)
			usesSharedKey := baseKey != baseID
			if !usesSharedKey && keySet[baseKey] {
				entries[baseID] = &catalogEntry{
					baseID: baseID, aliases: []string{baseID},
					actions: a.SupportedActions(),
				}
			}
		}
	}

	// Sort entries by base ID.
	sorted := make([]*catalogEntry, 0, len(entries))
	for _, e := range entries {
		sort.Strings(e.aliases)
		sorted = append(sorted, e)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].baseID < sorted[j].baseID })

	// Check for ?service= filter for detailed single-service view, and
	// ?action= for a single tool's raw description (MCP catalog spec).
	serviceFilter := r.URL.Query().Get("service")
	actionFilter := r.URL.Query().Get("action")

	var buf strings.Builder

	switch {
	case serviceFilter != "" && actionFilter != "":
		// Detail endpoint: return the byte-exact raw description so the
		// agent can read full Markdown docs for one specific tool.
		h.writeActionDetail(&buf, ctx, serviceFilter, actionFilter, sorted, adapterMeta, agent.UserID)
	case serviceFilter != "":
		h.writeServiceDetail(&buf, ctx, serviceFilter, sorted, adapterMeta, agent.UserID)
	default:
		h.writeCatalogOverview(&buf, ctx, sorted, adapterMeta, agent.UserID)
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(buf.String()))
}

// writeCatalogOverview renders the compact multi-service catalog.
func (h *SkillHandler) writeCatalogOverview(buf *strings.Builder, ctx context.Context, entries []*catalogEntry, adapterMeta map[string]adapters.ServiceMetadata, userID string) {
	buf.WriteString("# Your Clawvisor Service Catalog\n\n")

	if len(entries) == 0 {
		buf.WriteString("No cloud services are currently activated.\n\n")
		buf.WriteString("To activate a service, direct the user to the Clawvisor dashboard.\n\n")
	} else {
		buf.WriteString("_For detailed parameter docs, fetch `?service=<service_id>`._\n\n")

		// Pick a real aliased example from the user's catalog so the note
		// reflects their actual services rather than a hardcoded placeholder.
		// If no entry uses an account suffix, the note is not relevant — skip it.
		var aliasedExample, baseExample string
		for _, entry := range entries {
			for _, a := range entry.aliases {
				if strings.Contains(a, ":") {
					aliasedExample = a
					baseExample = entry.baseID
					break
				}
			}
			if aliasedExample != "" {
				break
			}
		}
		if aliasedExample != "" {
			buf.WriteString(fmt.Sprintf("**Important:** When invoking any service, use the full `service:account` identifier (e.g. `%s`) as the `service` value in requests. Using just the service name (e.g. `%s`) will fail if every entry has an account suffix.\n\n", aliasedExample, baseExample))
		}
	}

	for _, entry := range entries {
		buf.WriteString(fmt.Sprintf("## %s\n", entry.baseID))
		if meta, ok := adapterMeta[entry.baseID]; ok && meta.Description != "" {
			buf.WriteString(fmt.Sprintf("_%s_\n", meta.Description))
		}

		// Show aliases if more than one, or if the single alias differs from baseID.
		if len(entry.aliases) > 1 || (len(entry.aliases) == 1 && entry.aliases[0] != entry.baseID) {
			buf.WriteString(fmt.Sprintf("Accounts: %s\n", strings.Join(entry.aliases, ", ")))
		}
		buf.WriteString("\n")

		isMCP := h.isMCPService(entry.baseID)
		renderSection := func(maxChars int) string {
			var section strings.Builder
			for _, action := range entry.actions {
				if h.isRestricted(ctx, userID, entry, action) {
					continue
				}
				meta, ok := adapterMeta[entry.baseID]
				if !ok {
					section.WriteString(fmt.Sprintf("- **%s** \u2014 (requires approval or task)\n", action))
					continue
				}
				am, ok := meta.ActionMeta[action]
				if !ok {
					section.WriteString(fmt.Sprintf("- **%s** \u2014 (requires approval or task)\n", action))
					continue
				}
				desc := am.Description
				if isMCP {
					// Raw MCP description can be multi-paragraph Markdown
					// with fenced code, HTML, headers. Reduce to one line.
					desc = mcpadapter.OneLineSummary(desc, maxChars)
				}
				if desc == "" {
					desc = am.DisplayName
				}
				paramSig := compactParamSig(am.Params)
				section.WriteString(fmt.Sprintf("- **%s**%s \u2014 %s\n", action, paramSig, desc))
			}
			return section.String()
		}

		// For MCP services, enforce the per-server byte budget by squeezing
		// the summary cap until it fits. Non-MCP services use whatever
		// curated descriptions their YAML already provides.
		if isMCP {
			buf.WriteString(mcpadapter.FitToBudget(
				mcpadapter.PerServerByteBudget,
				mcpadapter.DefaultSummaryMaxChars,
				renderSection,
			))
		} else {
			buf.WriteString(renderSection(0))
		}
		buf.WriteString("\n")
	}

	// Append local daemon services if provider is configured.
	if h.localSvcProvider != nil {
		localServices, err := h.localSvcProvider.ActiveLocalServices(ctx, userID)
		if err != nil {
			h.logger.Warn("failed to fetch local services for catalog", "err", err)
		} else if len(localServices) > 0 {
			buf.WriteString("---\n\n")
			buf.WriteString("# Local Services\n\n")
			buf.WriteString("_These services run on your locally paired daemon(s). Each service can only be enabled on one daemon at a time._\n\n")

			for _, svc := range localServices {
				buf.WriteString(fmt.Sprintf("## %s\n", svc.Name))
				if svc.Description != "" {
					buf.WriteString(fmt.Sprintf("_%s_\n", svc.Description))
				}
				buf.WriteString(fmt.Sprintf("Service: `local.%s` (via %s)\n\n", svc.ServiceID, svc.DaemonName))

				for _, action := range svc.Actions {
					paramSig := compactLocalParamSig(action.Params)
					desc := action.Description
					if desc == "" {
						desc = action.Name
					}
					buf.WriteString(fmt.Sprintf("- **%s**%s \u2014 %s\n", action.ID, paramSig, desc))
				}
				buf.WriteString("\n")
			}
		}
	}
}

// writeLocalServiceDetail renders the detailed view for a single local service.
func (h *SkillHandler) writeLocalServiceDetail(buf *strings.Builder, ctx context.Context, serviceID, userID string) {
	if h.localSvcProvider == nil {
		buf.WriteString(fmt.Sprintf("Service %q is not available in this deployment.\n", serviceID))
		return
	}
	localServices, err := h.localSvcProvider.ActiveLocalServices(ctx, userID)
	if err != nil {
		buf.WriteString(fmt.Sprintf("Unable to load local service %q.\n", serviceID))
		return
	}
	svcID := strings.TrimPrefix(serviceID, "local.")
	var found *LocalCatalogService
	for i := range localServices {
		if localServices[i].ServiceID == svcID {
			found = &localServices[i]
			break
		}
	}
	if found == nil {
		buf.WriteString(fmt.Sprintf("Local service %q is not enabled or does not exist.\n", serviceID))
		return
	}

	buf.WriteString(fmt.Sprintf("# %s\n", found.Name))
	if found.Description != "" {
		buf.WriteString(fmt.Sprintf("_%s_\n", found.Description))
	}
	buf.WriteString(fmt.Sprintf("Service: `local.%s` (via %s)\n\n", found.ServiceID, found.DaemonName))
	buf.WriteString("_This service runs on a locally paired daemon._\n\n")

	for _, action := range found.Actions {
		desc := action.Description
		if desc == "" {
			desc = action.Name
		}
		buf.WriteString(fmt.Sprintf("## %s\n", action.ID))
		buf.WriteString(fmt.Sprintf("%s\n", desc))
		if len(action.Params) > 0 {
			buf.WriteString("Parameters:\n")
			for _, p := range action.Params {
				reqTag := "optional"
				if p.Required {
					reqTag = "**required**"
				}
				extras := []string{p.Type, reqTag}
				buf.WriteString(fmt.Sprintf("- `%s` (%s)\n", p.Name, strings.Join(extras, ", ")))
				if p.Description != "" {
					buf.WriteString(fmt.Sprintf("  %s\n", p.Description))
				}
			}
		}
		buf.WriteString("\n")
	}
}

// writeServiceDetail renders the detailed view for a single service.
func (h *SkillHandler) writeServiceDetail(buf *strings.Builder, ctx context.Context, serviceID string, entries []*catalogEntry, adapterMeta map[string]adapters.ServiceMetadata, userID string) {
	// Check if this is a local service request.
	if strings.HasPrefix(serviceID, "local.") {
		h.writeLocalServiceDetail(buf, ctx, serviceID, userID)
		return
	}

	// Find the matching entry.
	var entry *catalogEntry
	for _, e := range entries {
		if e.baseID == serviceID {
			entry = e
			break
		}
	}
	if entry == nil {
		buf.WriteString(fmt.Sprintf("Service %q is not activated or does not exist.\n", serviceID))
		return
	}

	buf.WriteString(fmt.Sprintf("# %s\n", entry.baseID))
	if meta, ok := adapterMeta[entry.baseID]; ok && meta.Description != "" {
		buf.WriteString(fmt.Sprintf("_%s_\n", meta.Description))
	}
	if len(entry.aliases) > 1 || (len(entry.aliases) == 1 && entry.aliases[0] != entry.baseID) {
		buf.WriteString(fmt.Sprintf("Accounts: %s\n", strings.Join(entry.aliases, ", ")))
	}
	buf.WriteString("\n")

	for _, action := range entry.actions {
		if h.isRestricted(ctx, userID, entry, action) {
			continue
		}

		if meta, ok := adapterMeta[entry.baseID]; ok {
			if am, ok := meta.ActionMeta[action]; ok {
				desc := am.Description
				if desc == "" {
					desc = am.DisplayName
				}
				buf.WriteString(fmt.Sprintf("## %s\n", action))
				buf.WriteString(fmt.Sprintf("%s\n", desc))
				buf.WriteString(fmt.Sprintf("Category: %s, Sensitivity: %s\n", am.Category, am.Sensitivity))
				if len(am.Params) > 0 {
					buf.WriteString("Parameters:\n")
					for _, p := range am.Params {
						reqTag := "optional"
						if p.Required {
							reqTag = "**required**"
						}
						extras := []string{p.Type, reqTag}
						if p.Default != nil {
							extras = append(extras, fmt.Sprintf("default: %v", p.Default))
						}
						if p.Min != nil {
							extras = append(extras, fmt.Sprintf("min: %d", *p.Min))
						}
						if p.Max != nil {
							extras = append(extras, fmt.Sprintf("max: %d", *p.Max))
						}
						buf.WriteString(fmt.Sprintf("- `%s` (%s)\n", p.Name, strings.Join(extras, ", ")))
					}
				}
				buf.WriteString("\n")
				continue
			}
		}
		buf.WriteString(fmt.Sprintf("## %s\n(requires approval or task)\n\n", action))
	}
}

// isRestricted returns true if the action is restricted on all aliases of the entry.
func (h *SkillHandler) isRestricted(ctx context.Context, userID string, entry *catalogEntry, action string) bool {
	for _, alias := range entry.aliases {
		rule, _ := matchServicePolicyRule(ctx, h.st, userID, alias, action)
		if rule == nil && alias != entry.baseID {
			rule, _ = matchServicePolicyRule(ctx, h.st, userID, entry.baseID, action)
		}
		restriction, _ := h.st.MatchRestriction(ctx, userID, alias, action)
		if restriction == nil && alias != entry.baseID {
			restriction, _ = h.st.MatchRestriction(ctx, userID, entry.baseID, action)
		}
		if rule == nil && restriction == nil {
			return false
		}
	}
	return true
}

// compactParamSig builds a compact inline parameter signature like "(to, subject, body?, in_reply_to?)".
func compactParamSig(params []adapters.ParamMeta) string {
	if len(params) == 0 {
		return ""
	}
	parts := make([]string, len(params))
	for i, p := range params {
		if p.Required {
			parts[i] = p.Name
		} else {
			parts[i] = p.Name + "?"
		}
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// compactLocalParamSig builds a compact inline parameter signature for local service actions.
func compactLocalParamSig(params []LocalCatalogParam) string {
	if len(params) == 0 {
		return ""
	}
	parts := make([]string, len(params))
	for i, p := range params {
		if p.Required {
			parts[i] = p.Name
		} else {
			parts[i] = p.Name + "?"
		}
	}
	return "(" + strings.Join(parts, ", ") + ")"
}
