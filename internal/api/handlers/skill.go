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
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// SkillHandler serves the dynamic skill catalog endpoint.
type SkillHandler struct {
	st         store.Store
	vault      vault.Vault
	adapterReg *adapters.Registry
	logger     *slog.Logger
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
	allAdapters := h.adapterReg.All()
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

	// Check for ?service= filter for detailed single-service view.
	serviceFilter := r.URL.Query().Get("service")

	var buf strings.Builder

	if serviceFilter != "" {
		h.writeServiceDetail(&buf, ctx, serviceFilter, sorted, adapterMeta, agent.UserID)
	} else {
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
		buf.WriteString("No services are currently activated.\n\n")
		buf.WriteString("To activate a service, direct the user to the Clawvisor dashboard.\n")
		return
	}

	buf.WriteString("_For detailed parameter docs, fetch `?service=<service_id>`._\n\n")

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

		for _, action := range entry.actions {
			if h.isRestricted(ctx, userID, entry, action) {
				continue
			}

			// Build the action line with compact param signature.
			if meta, ok := adapterMeta[entry.baseID]; ok {
				if am, ok := meta.ActionMeta[action]; ok {
					desc := am.Description
					if desc == "" {
						desc = am.DisplayName
					}
					paramSig := compactParamSig(am.Params)
					buf.WriteString(fmt.Sprintf("- **%s**%s \u2014 %s\n", action, paramSig, desc))
					continue
				}
			}
			buf.WriteString(fmt.Sprintf("- **%s** \u2014 (requires approval or task)\n", action))
		}
		buf.WriteString("\n")
	}
}

// writeServiceDetail renders the detailed view for a single service.
func (h *SkillHandler) writeServiceDetail(buf *strings.Builder, ctx context.Context, serviceID string, entries []*catalogEntry, adapterMeta map[string]adapters.ServiceMetadata, userID string) {
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
		restriction, _ := h.st.MatchRestriction(ctx, userID, alias, action)
		if restriction == nil && alias != entry.baseID {
			restriction, _ = h.st.MatchRestriction(ctx, userID, entry.baseID, action)
		}
		if restriction == nil {
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
