package handlers

import (
	"context"
	"errors"
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
	StorageKey             string                      `json:"-"`
	Name                   string                      `json:"name"`
	Kind                   string                      `json:"kind"`
	Provider               string                      `json:"provider,omitempty"`
	Scope                  string                      `json:"scope,omitempty"`
	Status                 string                      `json:"status"`
	Metadata               map[string]string           `json:"metadata,omitempty"`
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
	if strings.HasPrefix(r.URL.Path, "/control/") {
		h.writeControlList(w, r, agent.UserID)
		return
	}
	h.writeList(w, r, agent.UserID)
}

func (h *VaultHandler) CreateForUser(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	if h.vault == nil {
		writeError(w, http.StatusConflict, "VAULT_DISABLED", "vault is not configured")
		return
	}
	var body struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	itemID := strings.TrimSpace(body.ID)
	if itemID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "vault item id is required")
		return
	}
	if !validManualVaultItemID(itemID) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "vault item id may only contain letters, numbers, dots, underscores, dashes, and colons")
		return
	}
	if strings.TrimSpace(body.Value) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "vault item value is required")
		return
	}
	if h.isReservedVaultItemID(r.Context(), user.ID, itemID) {
		writeError(w, http.StatusConflict, "RESERVED_VAULT_ITEM", "vault item id is reserved for a connected account or provider credential")
		return
	}
	if _, err := h.vault.Get(r.Context(), user.ID, itemID); err == nil {
		writeError(w, http.StatusConflict, "VAULT_ITEM_EXISTS", "vault item already exists")
		return
	} else if err != nil && !errors.Is(err, vault.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not verify vault item")
		return
	}
	if err := h.vault.Set(r.Context(), user.ID, itemID, []byte(body.Value)); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create vault item")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "created", "id": itemID})
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
			placeholders, err := h.placeholdersForVaultItem(r.Context(), user.ID, item)
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

func (h *VaultHandler) UpdateForUser(w http.ResponseWriter, r *http.Request) {
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
	var body struct {
		Value string `json:"value"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Value) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "vault item value is required")
		return
	}
	item, ok, err := h.findItem(r, user.ID, itemID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list vault items")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "vault item not found")
		return
	}
	if item.Kind == "connected_account" {
		writeError(w, http.StatusConflict, "CONNECTED_ACCOUNT", "connected accounts must be updated through the service connection")
		return
	}
	if err := h.vault.Set(r.Context(), user.ID, vaultItemStorageKey(item), []byte(body.Value)); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not update vault item")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated", "id": item.ID})
}

func (h *VaultHandler) DeleteForUser(w http.ResponseWriter, r *http.Request) {
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
	item, ok, err := h.findItem(r, user.ID, itemID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list vault items")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "vault item not found")
		return
	}
	if item.Kind == "connected_account" {
		writeError(w, http.StatusConflict, "CONNECTED_ACCOUNT", "connected accounts must be disconnected through the service connection")
		return
	}
	if err := h.vault.Delete(r.Context(), user.ID, vaultItemStorageKey(item)); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not delete vault item")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "id": item.ID})
}

func (h *VaultHandler) findItem(r *http.Request, userID, itemID string) (VaultItem, bool, error) {
	items, err := h.listItems(r, userID)
	if err != nil {
		return VaultItem{}, false, err
	}
	for _, item := range items {
		if item.ID == itemID {
			return item, true, nil
		}
	}
	return VaultItem{}, false, nil
}

func validManualVaultItemID(itemID string) bool {
	if strings.Contains(itemID, "..") {
		return false
	}
	for _, r := range itemID {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-', r == ':':
		default:
			return false
		}
	}
	return true
}

func (h *VaultHandler) isReservedVaultItemID(ctx context.Context, userID, itemID string) bool {
	if _, _, ok := parseAgentScopedLLMKey(itemID); ok {
		return true
	}
	if llmProviderFromVaultKey(itemID) != "" {
		return true
	}
	if h.adapterReg == nil {
		return false
	}
	serviceID, _ := splitServiceScopedVaultItemID(itemID)
	if _, ok := h.adapterReg.GetForUser(ctx, serviceID, userID); ok {
		return true
	}
	return false
}

func (h *VaultHandler) placeholdersForVaultItem(ctx context.Context, userID string, item VaultItem) ([]*store.RuntimePlaceholder, error) {
	placeholders, err := h.st.ListRuntimePlaceholders(ctx, userID)
	if err != nil {
		return nil, err
	}
	var out []*store.RuntimePlaceholder
	for _, placeholder := range placeholders {
		if placeholderMatchesVaultItem(placeholder, item.ID, vaultItemStorageKey(item), item.ServiceBindings) {
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

type controlVaultItem struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Kind     string `json:"kind"`
	Provider string `json:"provider,omitempty"`
}

func (h *VaultHandler) writeControlList(w http.ResponseWriter, r *http.Request, userID string) {
	items, err := h.listItems(r, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list vault items")
		return
	}
	out := make([]controlVaultItem, 0, len(items))
	for _, item := range items {
		out = append(out, controlVaultItem{
			ID:       item.ID,
			Label:    item.Name,
			Kind:     item.Kind,
			Provider: item.Provider,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":        out,
		"total":        len(out),
		"instructions": "Use one of these id values as required_credentials_json[].vault_item_id when creating a task. This response is intentionally compact; do not pipe or shell-filter it.",
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

	activePlaceholders := make([]*store.RuntimePlaceholder, 0, len(placeholders))
	now := time.Now().UTC()
	for _, placeholder := range placeholders {
		if placeholder.RevokedAt != nil || (placeholder.ExpiresAt != nil && !placeholder.ExpiresAt.After(now)) {
			continue
		}
		activePlaceholders = append(activePlaceholders, placeholder)
	}

	items := make([]VaultItem, 0, len(keys))
	for _, key := range keys {
		if _, _, ok := parseAgentScopedLLMKey(key); ok {
			continue
		}
		if item, ok := llmCredentialVaultItem(key); ok {
			item.ActivePlaceholderCount, item.LastUsedAt = vaultItemPlaceholderStats(activePlaceholders, item.ID, vaultItemStorageKey(item), nil)
			items = append(items, item)
			continue
		}
		bindings := h.bindingsForVaultKey(r.Context(), userID, key, metas)
		if len(bindings) > 0 {
			for _, binding := range bindings {
				itemID := connectedVaultItemID(binding)
				activeCount, lastUsed := vaultItemPlaceholderStats(activePlaceholders, itemID, key, []VaultServiceBinding{binding})
				items = append(items, VaultItem{
					ID:                     itemID,
					StorageKey:             key,
					Name:                   binding.Name,
					Kind:                   "connected_account",
					Provider:               providerFromVaultKey(binding.ServiceID, []VaultServiceBinding{binding}),
					Status:                 "active",
					ServiceBindings:        []VaultServiceBinding{binding},
					ActivePlaceholderCount: activeCount,
					LastUsedAt:             lastUsed,
				})
			}
			continue
		}
		activeCount, lastUsed := vaultItemPlaceholderStats(activePlaceholders, key, key, nil)
		items = append(items, VaultItem{
			ID:                     key,
			StorageKey:             key,
			Name:                   vaultItemName(key, bindings),
			Kind:                   vaultItemKind(bindings),
			Provider:               providerFromVaultKey(key, bindings),
			Status:                 "active",
			ServiceBindings:        bindings,
			ActivePlaceholderCount: activeCount,
			LastUsedAt:             lastUsed,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	return items, nil
}

func llmCredentialVaultItem(key string) (VaultItem, bool) {
	if provider := llmProviderFromVaultKey(key); provider != "" {
		return VaultItem{
			ID:         "llm:" + provider + ":user",
			StorageKey: provider,
			Name:       display.ServiceName(provider) + " API key",
			Kind:       "llm_provider_key",
			Provider:   provider,
			Scope:      "user",
			Status:     "active",
		}, true
	}
	return VaultItem{}, false
}

func connectedVaultItemID(binding VaultServiceBinding) string {
	if binding.Alias == "" {
		return binding.ServiceID
	}
	return binding.ServiceID + ":" + binding.Alias
}

func llmProviderFromVaultKey(key string) string {
	switch key {
	case "anthropic", "openai":
		return key
	default:
		return ""
	}
}

func parseAgentScopedLLMKey(key string) (agentID, provider string, ok bool) {
	parts := strings.Split(key, ":")
	if len(parts) != 3 || parts[0] != "agent" {
		return "", "", false
	}
	switch parts[2] {
	case "anthropic", "openai":
		return parts[1], parts[2], true
	default:
		return "", "", false
	}
}

func vaultStorageKeyForItemID(itemID string) string {
	itemID = strings.TrimSpace(itemID)
	parts := strings.Split(itemID, ":")
	if len(parts) == 3 && parts[0] == "llm" && parts[2] == "user" && llmProviderFromVaultKey(parts[1]) != "" {
		return parts[1]
	}
	if len(parts) == 4 && parts[0] == "llm" && parts[2] == "agent" && llmProviderFromVaultKey(parts[1]) != "" && parts[3] != "" {
		return "agent:" + parts[3] + ":" + parts[1]
	}
	return itemID
}

func vaultStorageKeyForItemIDForUser(ctx context.Context, reg *adapters.Registry, userID, itemID string) string {
	itemID = strings.TrimSpace(itemID)
	if reg == nil || itemID == "" {
		return vaultStorageKeyForItemID(itemID)
	}
	serviceID, alias := splitServiceScopedVaultItemID(itemID)
	if serviceID == "" {
		return vaultStorageKeyForItemID(itemID)
	}
	if _, ok := reg.GetForUser(ctx, serviceID, userID); !ok {
		return vaultStorageKeyForItemID(itemID)
	}
	return reg.VaultKeyWithAliasForUser(serviceID, alias, userID)
}

func splitServiceScopedVaultItemID(itemID string) (serviceID, alias string) {
	before, after, ok := strings.Cut(itemID, ":")
	if !ok {
		return itemID, ""
	}
	if before == "" || after == "" {
		return itemID, ""
	}
	return before, after
}

func vaultItemStorageKey(item VaultItem) string {
	if item.StorageKey != "" {
		return item.StorageKey
	}
	return vaultStorageKeyForItemID(item.ID)
}

func vaultItemPlaceholderStats(placeholders []*store.RuntimePlaceholder, itemID, storageKey string, bindings []VaultServiceBinding) (int, *time.Time) {
	count := 0
	var lastUsed *time.Time
	for _, placeholder := range placeholders {
		if !placeholderMatchesVaultItem(placeholder, itemID, storageKey, bindings) {
			continue
		}
		count++
		if placeholder.LastUsedAt != nil && (lastUsed == nil || placeholder.LastUsedAt.After(*lastUsed)) {
			ts := *placeholder.LastUsedAt
			lastUsed = &ts
		}
	}
	return count, lastUsed
}

func placeholderMatchesVaultItem(placeholder *store.RuntimePlaceholder, itemID, storageKey string, bindings []VaultServiceBinding) bool {
	if placeholder == nil || itemID == "" {
		return false
	}
	if storageKey == "" {
		storageKey = vaultStorageKeyForItemID(itemID)
	}
	if placeholder.VaultItemID == itemID {
		return true
	}
	if len(bindings) == 0 && placeholder.VaultItemID == storageKey {
		return true
	}
	if placeholder.VaultItemID == "" && placeholder.ServiceID == itemID {
		return true
	}
	if len(bindings) == 0 && placeholder.VaultItemID == "" && placeholder.ServiceID == storageKey {
		return true
	}
	for _, binding := range bindings {
		if placeholder.ServiceID == binding.ServiceID {
			return true
		}
		if binding.Alias != "" && placeholder.ServiceID == binding.ServiceID+":"+binding.Alias {
			return true
		}
	}
	return false
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
