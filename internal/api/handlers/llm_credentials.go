package handlers

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// LLMCredentialsHandler manages the upstream API keys (sk-ant-…, sk-…)
// the lite-proxy injects into forwarded requests. The keys live in the
// vault under (user_id, "anthropic" | "openai").
type LLMCredentialsHandler struct {
	Store  store.Store
	Vault  vault.Vault
	Logger *slog.Logger
}

// NewLLMCredentialsHandler builds the handler with sensible defaults.
func NewLLMCredentialsHandler(st store.Store, v vault.Vault, logger *slog.Logger) *LLMCredentialsHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &LLMCredentialsHandler{Store: st, Vault: v, Logger: logger}
}

type setLLMCredentialBody struct {
	APIKey string `json:"api_key"`
}

// Set writes the upstream API key to the vault under (user_id, provider).
//
// PUT /api/runtime/llm-credentials/{provider}
//
//	body: {"api_key": "sk-ant-..."}
//	provider: "anthropic" | "openai"
func (h *LLMCredentialsHandler) Set(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user")
		return
	}
	provider := normalizeLLMProvider(r.PathValue("provider"))
	if provider == "" {
		writeJSONError(w, http.StatusBadRequest, "UNKNOWN_PROVIDER", "provider must be anthropic or openai")
		return
	}
	defer r.Body.Close()
	var body setLLMCredentialBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "MALFORMED_BODY", err.Error())
		return
	}
	apiKey := strings.TrimSpace(body.APIKey)
	if apiKey == "" {
		writeJSONError(w, http.StatusBadRequest, "MISSING_KEY", "api_key is required")
		return
	}
	if reason, ok := validateLLMAPIKey(provider, apiKey); !ok {
		writeJSONError(w, http.StatusBadRequest, "INVALID_KEY", reason)
		return
	}

	if err := h.Vault.Set(r.Context(), user.ID, provider, []byte(apiKey)); err != nil {
		h.Logger.WarnContext(r.Context(), "lite-proxy: vault set failed",
			"user_id", user.ID, "provider", provider, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "VAULT_ERROR", "could not store credential")
		return
	}

	if err := h.Store.UpsertServiceMeta(r.Context(), user.ID, provider, "default", time.Now().UTC()); err != nil {
		// Vault write succeeded; service-meta is for dashboard listing
		// only. Log + continue.
		h.Logger.WarnContext(r.Context(), "lite-proxy: service meta upsert failed",
			"user_id", user.ID, "provider", provider, "err", err.Error())
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"provider": provider,
		"status":   "stored",
	})
}

// Delete removes the upstream API key for (user_id, provider).
//
// DELETE /api/runtime/llm-credentials/{provider}
func (h *LLMCredentialsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user")
		return
	}
	provider := normalizeLLMProvider(r.PathValue("provider"))
	if provider == "" {
		writeJSONError(w, http.StatusBadRequest, "UNKNOWN_PROVIDER", "provider must be anthropic or openai")
		return
	}
	if err := h.Vault.Delete(r.Context(), user.ID, provider); err != nil && !errors.Is(err, vault.ErrNotFound) {
		h.Logger.WarnContext(r.Context(), "lite-proxy: vault delete failed",
			"user_id", user.ID, "provider", provider, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "VAULT_ERROR", "could not delete credential")
		return
	}
	if err := h.Store.DeleteServiceMeta(r.Context(), user.ID, provider, "default"); err != nil {
		h.Logger.WarnContext(r.Context(), "lite-proxy: service meta delete failed",
			"user_id", user.ID, "provider", provider, "err", err.Error())
	}
	w.WriteHeader(http.StatusNoContent)
}

// List returns which providers have an upstream credential stored. Does
// not return the credential itself.
//
// GET /api/runtime/llm-credentials
func (h *LLMCredentialsHandler) List(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user")
		return
	}
	type entry struct {
		Provider string `json:"provider"`
		Stored   bool   `json:"stored"`
	}
	out := make([]entry, 0, 2)
	for _, p := range []string{"anthropic", "openai"} {
		_, err := h.Vault.Get(r.Context(), user.ID, p)
		out = append(out, entry{Provider: p, Stored: err == nil})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"credentials": out})
}

func normalizeLLMProvider(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "anthropic":
		return "anthropic"
	case "openai":
		return "openai"
	}
	return ""
}

// validateLLMAPIKey applies provider-aware shape checks. Rejects empty,
// oversized, control-character-bearing, or wrong-prefix keys so a
// malformed value can't end up swapped into upstream auth headers later.
func validateLLMAPIKey(provider, key string) (reason string, ok bool) {
	if len(key) > 4096 {
		return "api_key exceeds 4 KiB", false
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return "api_key contains control characters", false
		}
	}
	if strings.Contains(strings.ToLower(key), "autovault") || strings.Contains(strings.ToLower(key), "clawvisor") {
		return "api_key contains a Clawvisor placeholder marker; did you paste the wrong value?", false
	}
	switch provider {
	case "anthropic":
		if !strings.HasPrefix(key, "sk-ant-") {
			return "anthropic api_key must start with sk-ant-", false
		}
	case "openai":
		if !(strings.HasPrefix(key, "sk-") || strings.HasPrefix(key, "sk-proj-")) {
			return "openai api_key must start with sk-", false
		}
	}
	return "", true
}
