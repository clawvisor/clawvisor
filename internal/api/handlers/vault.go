package handlers

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/display"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

type VaultHandler struct {
	st         store.Store
	vault      vault.Vault
	adapterReg *adapters.Registry
}

func NewVaultHandler(st store.Store, v vault.Vault, adapterReg *adapters.Registry) *VaultHandler {
	return &VaultHandler{st: st, vault: v, adapterReg: adapterReg}
}

type VaultServiceBinding struct {
	ServiceID string `json:"service_id"`
	Alias     string `json:"alias,omitempty"`
	Name      string `json:"name"`
}

type VaultItem struct {
	ID                     string                      `json:"id"`
	Name                   string                      `json:"name"`
	Kind                   string                      `json:"kind"`
	Provider               string                      `json:"provider,omitempty"`
	Status                 string                      `json:"status"`
	ServiceBindings        []VaultServiceBinding       `json:"service_bindings,omitempty"`
	ActivePlaceholderCount int                         `json:"active_placeholder_count"`
	LastUsedAt             *time.Time                  `json:"last_used_at,omitempty"`
	Placeholders           []*store.RuntimePlaceholder `json:"placeholders,omitempty"`
}

func (h *VaultHandler) ListForUser(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	h.writeList(w, r, user.ID)
}

func (h *VaultHandler) ListForAgent(w http.ResponseWriter, r *http.Request) {
	agent := middleware.AgentFromContext(r.Context())
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	h.writeList(w, r, agent.UserID)
}

func (h *VaultHandler) GetForUser(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	itemID := strings.TrimSpace(r.PathValue("id"))
	if itemID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "vault item id is required")
		return
	}
	items, err := h.listItems(r, user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list vault items")
		return
	}
	for _, item := range items {
		if item.ID == itemID {
			placeholders, err := h.placeholdersForVaultItem(r.Context(), user.ID, itemID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list vault item placeholders")
				return
			}
			item.Placeholders = placeholders
			writeJSON(w, http.StatusOK, item)
			return
		}
	}
	writeError(w, http.StatusNotFound, "NOT_FOUND", "vault item not found")
}

func (h *VaultHandler) placeholdersForVaultItem(ctx context.Context, userID, itemID string) ([]*store.RuntimePlaceholder, error) {
	placeholders, err := h.st.ListRuntimePlaceholders(ctx, userID)
	if err != nil {
		return nil, err
	}
	var out []*store.RuntimePlaceholder
	for _, placeholder := range placeholders {
		if placeholder.VaultItemID == itemID || (placeholder.VaultItemID == "" && placeholder.ServiceID == itemID) {
			out = append(out, placeholder)
		}
	}
	if out == nil {
		out = []*store.RuntimePlaceholder{}
	}
	return out, nil
}

func (h *VaultHandler) writeList(w http.ResponseWriter, r *http.Request, userID string) {
	items, err := h.listItems(r, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list vault items")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": items,
		"total":   len(items),
	})
}

func (h *VaultHandler) listItems(r *http.Request, userID string) ([]VaultItem, error) {
	if h.vault == nil {
		return []VaultItem{}, nil
	}
	keys, err := h.vault.List(r.Context(), userID)
	if err != nil {
		return nil, err
	}
	metas, err := h.st.ListServiceMetas(r.Context(), userID)
	if err != nil {
		return nil, err
	}
	placeholders, err := h.st.ListRuntimePlaceholders(r.Context(), userID)
	if err != nil {
		return nil, err
	}

	counts := map[string]int{}
	lastUsed := map[string]*time.Time{}
	now := time.Now().UTC()
	for _, placeholder := range placeholders {
		if placeholder.RevokedAt != nil || (placeholder.ExpiresAt != nil && !placeholder.ExpiresAt.After(now)) {
			continue
		}
		counts[placeholder.ServiceID]++
		if placeholder.LastUsedAt != nil {
			if lastUsed[placeholder.ServiceID] == nil || placeholder.LastUsedAt.After(*lastUsed[placeholder.ServiceID]) {
				ts := *placeholder.LastUsedAt
				lastUsed[placeholder.ServiceID] = &ts
			}
		}
	}

	items := make([]VaultItem, 0, len(keys))
	for _, key := range keys {
		bindings := h.bindingsForVaultKey(r.Context(), userID, key, metas)
		items = append(items, VaultItem{
			ID:                     key,
			Name:                   vaultItemName(key, bindings),
			Kind:                   vaultItemKind(bindings),
			Provider:               providerFromVaultKey(key, bindings),
			Status:                 "active",
			ServiceBindings:        bindings,
			ActivePlaceholderCount: counts[key],
			LastUsedAt:             lastUsed[key],
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	return items, nil
}

func (h *VaultHandler) bindingsForVaultKey(ctx context.Context, userID, key string, metas []*store.ServiceMeta) []VaultServiceBinding {
	var bindings []VaultServiceBinding
	for _, meta := range metas {
		if h.adapterReg.VaultKeyWithAliasForUser(meta.ServiceID, meta.Alias, userID) != key {
			continue
		}
		bindings = append(bindings, VaultServiceBinding{
			ServiceID: meta.ServiceID,
			Alias:     omitDefaultAlias(meta.Alias),
			Name:      display.ServiceName(meta.ServiceID),
		})
	}
	if len(bindings) == 0 {
		if _, ok := h.adapterReg.GetForUser(ctx, key, userID); ok {
			bindings = append(bindings, VaultServiceBinding{
				ServiceID: key,
				Name:      display.ServiceName(key),
			})
		}
	}
	sort.Slice(bindings, func(i, j int) bool {
		if bindings[i].ServiceID == bindings[j].ServiceID {
			return bindings[i].Alias < bindings[j].Alias
		}
		return bindings[i].ServiceID < bindings[j].ServiceID
	})
	return bindings
}

func omitDefaultAlias(alias string) string {
	if alias == "" || alias == "default" {
		return ""
	}
	return alias
}

func vaultItemName(key string, bindings []VaultServiceBinding) string {
	if len(bindings) > 0 {
		return bindings[0].Name
	}
	return key
}

func vaultItemKind(bindings []VaultServiceBinding) string {
	if len(bindings) > 0 {
		return "connected_account"
	}
	return "secret"
}

func providerFromVaultKey(key string, bindings []VaultServiceBinding) string {
	if len(bindings) > 0 {
		key = bindings[0].ServiceID
	}
	key = strings.SplitN(key, ":", 2)[0]
	if before, _, ok := strings.Cut(key, "."); ok && before != "" {
		return before
	}
	return key
}
