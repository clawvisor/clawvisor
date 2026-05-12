package handlers

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
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

// Set writes the upstream API key to the vault under (user_id, provider) —
// or, when ?agent_id=<id> is present, under the agent-scoped service ID
// (`agent:<id>:<provider>`) so the lite-proxy can prefer it for that agent.
//
// PUT /api/runtime/llm-credentials/{provider}
// PUT /api/runtime/llm-credentials/{provider}?agent_id=<id>
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
	serviceID, agentID, errResp := h.resolveServiceID(r, user.ID, provider)
	if errResp != nil {
		errResp(w)
		return
	}
	defer r.Body.Close()
	// Cap the body before JSON-parsing — api_key max length is 4 KiB per
	// validateLLMAPIKey, but the JSON envelope is unbounded otherwise.
	// 16 KiB is plenty of slack for the wrapper + future fields without
	// allowing pathological payloads to allocate memory.
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
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

	if err := h.Vault.Set(r.Context(), user.ID, serviceID, []byte(apiKey)); err != nil {
		h.Logger.WarnContext(r.Context(), "lite-proxy: vault set failed",
			"user_id", user.ID, "service_id", serviceID, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "VAULT_ERROR", "could not store credential")
		return
	}

	if err := h.Store.UpsertServiceMeta(r.Context(), user.ID, serviceID, "default", time.Now().UTC()); err != nil {
		// Vault write succeeded; service-meta is for dashboard listing
		// only. Log + continue.
		h.Logger.WarnContext(r.Context(), "lite-proxy: service meta upsert failed",
			"user_id", user.ID, "service_id", serviceID, "err", err.Error())
	}

	resp := map[string]string{
		"provider":   provider,
		"service_id": serviceID,
		"status":     "stored",
	}
	if agentID != "" {
		resp["agent_id"] = agentID
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// Delete removes the upstream API key for (user_id, provider) — or, when
// ?agent_id=<id> is present, the agent-scoped credential.
//
// DELETE /api/runtime/llm-credentials/{provider}
// DELETE /api/runtime/llm-credentials/{provider}?agent_id=<id>
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
	serviceID, _, errResp := h.resolveServiceID(r, user.ID, provider)
	if errResp != nil {
		errResp(w)
		return
	}
	if err := h.Vault.Delete(r.Context(), user.ID, serviceID); err != nil && !errors.Is(err, vault.ErrNotFound) {
		h.Logger.WarnContext(r.Context(), "lite-proxy: vault delete failed",
			"user_id", user.ID, "service_id", serviceID, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "VAULT_ERROR", "could not delete credential")
		return
	}
	if err := h.Store.DeleteServiceMeta(r.Context(), user.ID, serviceID, "default"); err != nil {
		h.Logger.WarnContext(r.Context(), "lite-proxy: service meta delete failed",
			"user_id", user.ID, "service_id", serviceID, "err", err.Error())
	}
	w.WriteHeader(http.StatusNoContent)
}

// List returns which providers have an upstream credential stored. Does
// not return the credential itself. When ?agent_id=<id> is present, the
// per-agent storage status is reported alongside the user-scoped status
// — the lite-proxy prefers the agent-scoped key when both exist.
//
// GET /api/runtime/llm-credentials
// GET /api/runtime/llm-credentials?agent_id=<id>
func (h *LLMCredentialsHandler) List(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing user")
		return
	}
	type entry struct {
		Provider     string `json:"provider"`
		Stored       bool   `json:"stored"`
		AgentStored  bool   `json:"agent_stored,omitempty"`
		AgentID      string `json:"agent_id,omitempty"`
	}
	agentID := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	if agentID != "" {
		if err := h.verifyAgentOwnership(r, user.ID, agentID); err != nil {
			err(w)
			return
		}
	}
	out := make([]entry, 0, 2)
	for _, p := range []string{"anthropic", "openai"} {
		e := entry{Provider: p}
		// Differentiate vault.ErrNotFound (legitimately not stored) from
		// other errors (backend down, permissions, etc.). The latter should
		// surface as a 500 rather than misreporting stored=false to the UI.
		_, err := h.Vault.Get(r.Context(), user.ID, p)
		switch {
		case err == nil:
			e.Stored = true
		case errors.Is(err, vault.ErrNotFound):
			// Not stored; leave e.Stored=false.
		default:
			h.Logger.WarnContext(r.Context(), "lite-proxy: vault get failed in List",
				"user_id", user.ID, "provider", p, "err", err.Error())
			writeJSONError(w, http.StatusInternalServerError, "VAULT_ERROR", "could not read credential status")
			return
		}
		if agentID != "" {
			scoped := llmproxy.AgentScopedVaultServiceID(agentID, conversation.Provider(p))
			if scoped != "" {
				_, err := h.Vault.Get(r.Context(), user.ID, scoped)
				switch {
				case err == nil:
					e.AgentStored = true
					e.AgentID = agentID
				case errors.Is(err, vault.ErrNotFound):
					e.AgentID = agentID
				default:
					h.Logger.WarnContext(r.Context(), "lite-proxy: vault get failed in List (agent-scoped)",
						"user_id", user.ID, "service_id", scoped, "err", err.Error())
					writeJSONError(w, http.StatusInternalServerError, "VAULT_ERROR", "could not read agent credential status")
					return
				}
			}
		}
		out = append(out, e)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"credentials": out})
}

// resolveServiceID inspects ?agent_id=<id> and returns either the agent-scoped
// vault service ID (after verifying the agent belongs to the calling user) or
// the plain user-scoped provider service ID. Returns a non-nil error responder
// when the agent_id is malformed or doesn't belong to the user.
func (h *LLMCredentialsHandler) resolveServiceID(r *http.Request, userID, provider string) (serviceID, agentID string, errResp func(http.ResponseWriter)) {
	agentID = strings.TrimSpace(r.URL.Query().Get("agent_id"))
	if agentID == "" {
		return provider, "", nil
	}
	if errResp := h.verifyAgentOwnership(r, userID, agentID); errResp != nil {
		return "", "", errResp
	}
	scoped := llmproxy.AgentScopedVaultServiceID(agentID, conversation.Provider(provider))
	if scoped == "" {
		return "", "", func(w http.ResponseWriter) {
			writeJSONError(w, http.StatusBadRequest, "UNKNOWN_PROVIDER", "could not derive agent-scoped service ID")
		}
	}
	return scoped, agentID, nil
}

// verifyAgentOwnership fails closed: if the caller passes an agent_id that
// doesn't belong to them, we 403 rather than silently writing into a
// neighbor's vault scope.
func (h *LLMCredentialsHandler) verifyAgentOwnership(r *http.Request, userID, agentID string) func(http.ResponseWriter) {
	agents, err := h.Store.ListAgents(r.Context(), userID)
	if err != nil {
		h.Logger.WarnContext(r.Context(), "lite-proxy: list agents failed",
			"user_id", userID, "err", err.Error())
		return func(w http.ResponseWriter) {
			writeJSONError(w, http.StatusInternalServerError, "STORE_ERROR", "could not verify agent ownership")
		}
	}
	for _, a := range agents {
		if a.ID == agentID {
			return nil
		}
	}
	return func(w http.ResponseWriter) {
		writeJSONError(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent does not exist or does not belong to this user")
	}
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
		// Anthropic keys also begin with `sk-` (sk-ant-…). Explicitly
		// reject those rather than letting the broader prefix swallow them.
		if strings.HasPrefix(key, "sk-ant-") {
			return "this looks like an Anthropic api_key (sk-ant-…); did you mean to set the anthropic provider?", false
		}
		if !(strings.HasPrefix(key, "sk-") || strings.HasPrefix(key, "sk-proj-")) {
			return "openai api_key must start with sk-", false
		}
	}
	return "", true
}
