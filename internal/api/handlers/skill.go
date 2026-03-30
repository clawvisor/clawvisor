package handlers

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/internal/api/middleware"
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

// Catalog returns a personalized markdown document listing only the agent's
// activated services (with restriction-filtered actions).
//
// GET /api/skill/catalog
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

	type catalogService struct {
		id      string // display ID, e.g. "google.gmail" or "google.gmail:personal"
		baseID  string // adapter service ID, e.g. "google.gmail"
		actions []string
	}

	// Build the adapter metadata index for action descriptions.
	adapterMeta := map[string]adapters.ServiceMetadata{} // serviceID → metadata
	allAdapters := h.adapterReg.All()
	for _, a := range allAdapters {
		if mp, ok := a.(adapters.MetadataProvider); ok {
			adapterMeta[a.ServiceID()] = mp.ServiceMetadata()
		}
	}

	// Only collect activated services.
	var services []catalogService
	for _, a := range allAdapters {
		credentialFree := a.ValidateCredential(nil) == nil
		if credentialFree {
			// Credential-free services (e.g. iMessage) require explicit
			// activation via service_meta, same as the gateway check.
			for _, m := range metas {
				if m.ServiceID == a.ServiceID() {
					services = append(services, catalogService{
						id: a.ServiceID(), baseID: a.ServiceID(),
						actions: a.SupportedActions(),
					})
					break
				}
			}
			continue
		}

		// Show each activated alias as a separate catalog entry.
		shown := false
		for _, m := range metas {
			if m.ServiceID != a.ServiceID() {
				continue
			}
			vKey := h.adapterReg.VaultKeyWithAlias(a.ServiceID(), m.Alias)
			if !keySet[vKey] {
				continue
			}
			shown = true
			displayID := a.ServiceID()
			if m.Alias != "default" {
				displayID = a.ServiceID() + ":" + m.Alias
			}
			services = append(services, catalogService{
				id: displayID, baseID: a.ServiceID(),
				actions: a.SupportedActions(),
			})
		}

		// Fallback: check base vault key (handles pre-existing activations
		// that may not have a service_meta row). Skip when the service uses
		// a shared vault key (e.g. all google.* services share "google").
		if !shown {
			baseKey := h.adapterReg.VaultKey(a.ServiceID())
			usesSharedKey := baseKey != a.ServiceID()
			if !usesSharedKey && keySet[baseKey] {
				services = append(services, catalogService{
					id: a.ServiceID(), baseID: a.ServiceID(),
					actions: a.SupportedActions(),
				})
			}
		}
	}
	sort.Slice(services, func(i, j int) bool { return services[i].id < services[j].id })

	var buf strings.Builder
	buf.WriteString("# Your Clawvisor Service Catalog\n\n")

	if len(services) == 0 {
		buf.WriteString("No services are currently activated.\n\n")
		buf.WriteString("To activate a service, direct the user to the Clawvisor dashboard.\n")
	} else {
		for _, svc := range services {
			// Service heading — include description if available.
			buf.WriteString(fmt.Sprintf("## %s\n", svc.id))
			if meta, ok := adapterMeta[svc.baseID]; ok && meta.Description != "" {
				buf.WriteString(fmt.Sprintf("_%s_\n", meta.Description))
			}
			buf.WriteString("\n")

			for _, action := range svc.actions {
				// If a restriction matches this action (exact or base service), skip it.
				restriction, _ := h.st.MatchRestriction(ctx, agent.UserID, svc.id, action)
				if restriction == nil && svc.id != svc.baseID {
					restriction, _ = h.st.MatchRestriction(ctx, agent.UserID, svc.baseID, action)
				}
				if restriction != nil {
					continue
				}

				// All non-restricted actions require approval unless covered by a task.
				label := " (requires approval or task)"

				// Read action metadata from MetadataProvider.
				if meta, ok := adapterMeta[svc.baseID]; ok {
					if am, ok := meta.ActionMeta[action]; ok {
						desc := am.Description
						if desc == "" {
							desc = am.DisplayName
						}
						buf.WriteString(fmt.Sprintf("**%s**%s — %s\n", action, label, desc))
						buf.WriteString(fmt.Sprintf("  Category: %s, Sensitivity: %s\n\n", am.Category, am.Sensitivity))
						continue
					}
				}
				buf.WriteString(fmt.Sprintf("**%s**%s\n\n", action, label))
			}
		}
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(buf.String()))
}
