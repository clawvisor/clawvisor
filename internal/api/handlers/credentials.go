package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// CredentialHandler handles both the proxy-facing credential-lookup
// endpoint and the user-facing vault UX (move to vault, list, rotate,
// revoke). Stage 2 M1-M2.
type CredentialHandler struct {
	st     store.Store
	vault  vault.Vault
	logger *slog.Logger
}

func NewCredentialHandler(st store.Store, v vault.Vault, logger *slog.Logger) *CredentialHandler {
	return &CredentialHandler{st: st, vault: v, logger: logger}
}

// vaultServicePrefix namespaces the credential-injection secrets inside
// the shared vault so they don't collide with regular service creds.
// The full vault key is "proxy-inject:{credential_ref}".
const vaultServicePrefix = "proxy-inject:"

func vaultKey(credentialRef string) string {
	return vaultServicePrefix + credentialRef
}

// -- Proxy-facing: POST /api/proxy/credential-lookup ----------------------

type credentialLookupRequest struct {
	AgentTokenID    string `json:"agent_token_id"`
	CredentialRef   string `json:"credential_ref"`
	DestinationHost string `json:"destination_host,omitempty"`
	DestinationPath string `json:"destination_path,omitempty"`
	RequestID       string `json:"request_id,omitempty"`
}

type credentialLookupResponse struct {
	Credential   string `json:"credential"`
	CredentialID string `json:"credential_id"`
	TTLSeconds   int    `json:"ttl_seconds"`
	CacheKey     string `json:"cache_key"`
}

// Lookup serves POST /api/proxy/credential-lookup. Authenticated via
// RequireProxy middleware. Returns plaintext credential to the proxy
// which caches it briefly and injects into outbound requests.
// Docs: design-proxy-stage2.md §2.2.
func (h *CredentialHandler) Lookup(w http.ResponseWriter, r *http.Request) {
	p := middleware.ProxyFromContext(r.Context())
	if p == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var req credentialLookupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.CredentialRef == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "credential_ref is required")
		return
	}

	// Find the bridge → user for this proxy.
	bt, err := h.st.GetBridgeTokenByID(r.Context(), p.BridgeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load bridge")
		return
	}

	// Look up the injectable credential row.
	cred, err := h.st.GetInjectableCredential(r.Context(), bt.UserID, req.CredentialRef)
	if err != nil {
		h.logUsage(r.Context(), &req, "not_found")
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "CREDENTIAL_NOT_FOUND", "no credential configured for "+req.CredentialRef)
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "credential lookup failed")
		return
	}
	if cred.RevokedAt != nil {
		h.logUsage(r.Context(), &req, "denied_revoked")
		writeError(w, http.StatusForbidden, "CREDENTIAL_REVOKED", "credential has been revoked")
		return
	}

	// ACL check: if UsableByAgents is non-empty, the caller's agent
	// must be listed. Empty = any agent of the bridge can use.
	//
	// UsableByAgents stores agent UUIDs (how the dashboard identifies
	// agents) while the proxy sends req.AgentTokenID = raw cvis_ token
	// (from Proxy-Authorization). Resolve the raw token to an agent here
	// so the dashboard's ACL entries match.
	if len(cred.UsableByAgents) > 0 {
		agent, aerr := h.resolveAgentFromRequest(r.Context(), bt.UserID, req.AgentTokenID)
		if aerr != nil || agent == nil {
			h.logUsage(r.Context(), &req, "denied_acl")
			writeError(w, http.StatusForbidden, "CREDENTIAL_NOT_AUTHORIZED", "agent not in ACL for this credential")
			return
		}
		ok := false
		for _, allowed := range cred.UsableByAgents {
			if allowed == agent.ID || allowed == req.AgentTokenID {
				ok = true
				break
			}
		}
		if !ok {
			h.logUsage(r.Context(), &req, "denied_acl")
			writeError(w, http.StatusForbidden, "CREDENTIAL_NOT_AUTHORIZED", "agent not in ACL for this credential")
			return
		}
	}

	// Decrypt from vault.
	plain, err := h.vault.Get(r.Context(), bt.UserID, cred.VaultKey)
	if err != nil {
		h.logUsage(r.Context(), &req, "not_found")
		if errors.Is(err, vault.ErrNotFound) {
			writeError(w, http.StatusNotFound, "CREDENTIAL_NOT_FOUND", "vault entry missing")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "vault read failed")
		return
	}

	h.logUsage(r.Context(), &req, "granted")

	// cache_key bakes in the credential_ref + rotation counter so the
	// proxy's self-invalidating cache flips when we rotate server-side.
	cacheKey := cred.CredentialRef + "#" + cred.ID
	writeJSON(w, http.StatusOK, credentialLookupResponse{
		Credential:   string(plain),
		CredentialID: cred.ID,
		TTLSeconds:   300,
		CacheKey:     cacheKey,
	})
}

// resolveAgentFromRequest interprets req.AgentTokenID as either a raw
// cvis_ token (proxy usually sends this) or an agent UUID (test harness
// may send this). Returns (nil, nil) on empty input, error for a DB
// failure, or (nil, ErrNotFound) for an unknown token.
func (h *CredentialHandler) resolveAgentFromRequest(ctx context.Context, userID, tokenOrID string) (*store.Agent, error) {
	tokenOrID = strings.TrimSpace(tokenOrID)
	if tokenOrID == "" {
		return nil, nil
	}
	// Try raw-token → hash → agent lookup first.
	if hash := auth.HashToken(tokenOrID); hash != "" {
		if a, err := h.st.GetAgentByToken(ctx, hash); err == nil {
			if a.UserID != userID {
				return nil, store.ErrNotFound
			}
			return a, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	// Fallback: UUID was sent directly.
	agents, err := h.st.ListAgents(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		if a.ID == tokenOrID {
			return a, nil
		}
	}
	return nil, store.ErrNotFound
}

func (h *CredentialHandler) logUsage(ctx context.Context, req *credentialLookupRequest, decision string) {
	_ = h.st.LogCredentialUsage(ctx, &store.CredentialUsageRecord{
		AgentTokenID:    req.AgentTokenID,
		CredentialRef:   req.CredentialRef,
		DestinationHost: req.DestinationHost,
		DestinationPath: req.DestinationPath,
		Decision:        decision,
		RequestID:       req.RequestID,
	})
}

// -- User-facing: credential management -----------------------------------

type upsertCredentialRequest struct {
	CredentialRef  string   `json:"credential_ref"`            // required
	Credential     string   `json:"credential"`                // plaintext; server encrypts
	UsableByAgents []string `json:"usable_by_agents,omitempty"` // empty = any agent
}

// UpsertCredential handles POST /api/vault/injectable-credentials.
// User JWT authed. Encrypts the credential value, stores in vault, writes
// the metadata row in injectable_credentials.
func (h *CredentialHandler) UpsertCredential(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	var req upsertCredentialRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.CredentialRef == "" || req.Credential == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "credential_ref and credential are required")
		return
	}

	vkey := vaultKey(req.CredentialRef)
	if err := h.vault.Set(r.Context(), user.ID, vkey, []byte(req.Credential)); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "vault write failed")
		return
	}

	c := &store.InjectableCredential{
		UserID:         user.ID,
		CredentialRef:  req.CredentialRef,
		VaultKey:       vkey,
		UsableByAgents: req.UsableByAgents,
	}
	if err := h.st.UpsertInjectableCredential(r.Context(), c); err != nil {
		// Best-effort rollback of the vault write.
		_ = h.vault.Delete(r.Context(), user.ID, vkey)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "metadata write failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"credential_ref": req.CredentialRef,
		"id":             c.ID,
	})
}

// ListCredentials handles GET /api/vault/injectable-credentials. Returns
// metadata only — the plaintext value is never exposed to dashboard.
func (h *CredentialHandler) ListCredentials(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	creds, err := h.st.ListInjectableCredentials(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "list failed")
		return
	}
	// Defensive: never return vault_key to clients.
	out := make([]map[string]any, 0, len(creds))
	for _, c := range creds {
		out = append(out, map[string]any{
			"id":               c.ID,
			"credential_ref":   c.CredentialRef,
			"usable_by_agents": c.UsableByAgents,
			"created_at":       c.CreatedAt.UTC().Format(time.RFC3339),
			"rotated_at":       c.RotatedAt,
			"revoked_at":       c.RevokedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": out})
}

// RevokeCredential handles DELETE /api/vault/injectable-credentials/{ref}.
func (h *CredentialHandler) RevokeCredential(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	ref := r.PathValue("ref")
	if ref == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "credential_ref path value required")
		return
	}
	if err := h.st.RevokeInjectableCredential(r.Context(), user.ID, ref); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "credential not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "revoke failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// UsageLog handles GET /api/vault/injectable-credentials/usage.
// Returns recent lookup audit rows for the authenticated user.
func (h *CredentialHandler) UsageLog(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	since := time.Now().Add(-7 * 24 * time.Hour)
	records, err := h.st.ListCredentialUsage(r.Context(), user.ID, since, 500)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "list failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": records})
}

// -- Built-in injection rules ---------------------------------------------

// BuiltInInjectionRules is the set of injection rules Clawvisor ships
// with. Served to all users via ListInjectionRules (user_id NULL). The
// seed is idempotent — re-run at startup to pick up updates.
func BuiltInInjectionRules() []*store.InjectionRule {
	return []*store.InjectionRule{
		{
			HostPattern:    "api.anthropic.com",
			PathPattern:    "/v1/*",
			Method:         "*",
			InjectStyle:    "header",
			InjectTarget:   "x-api-key",
			InjectTemplate: "{{credential}}",
			CredentialRef:  "vault:anthropic",
			Priority:       100,
			Enabled:        true,
		},
		{
			HostPattern:    "api.openai.com",
			PathPattern:    "/v1/*",
			Method:         "*",
			InjectStyle:    "header",
			InjectTarget:   "Authorization",
			InjectTemplate: "Bearer {{credential}}",
			CredentialRef:  "vault:openai",
			Priority:       100,
			Enabled:        true,
		},
		{
			HostPattern:    "generativelanguage.googleapis.com",
			PathPattern:    "/v1*/*",
			Method:         "*",
			InjectStyle:    "query",
			InjectTarget:   "key",
			InjectTemplate: "{{credential}}",
			CredentialRef:  "vault:google-ai",
			Priority:       100,
			Enabled:        true,
		},
	}
}

// SeedBuiltInInjectionRules writes the built-in rules into the store if
// they don't already exist. Safe to call on every server boot.
func SeedBuiltInInjectionRules(ctx context.Context, st store.Store, logger *slog.Logger) {
	rules := BuiltInInjectionRules()
	existing, err := st.ListInjectionRules(ctx, "") // empty userID → just built-ins
	if err != nil {
		logger.Warn("seed injection rules: list failed", "err", err)
		return
	}
	known := make(map[string]bool, len(existing))
	for _, r := range existing {
		if r.UserID == "" {
			known[r.HostPattern+"|"+r.PathPattern+"|"+r.CredentialRef] = true
		}
	}
	for _, r := range rules {
		key := r.HostPattern + "|" + r.PathPattern + "|" + r.CredentialRef
		if known[key] {
			continue
		}
		if err := st.CreateInjectionRule(ctx, r); err != nil {
			logger.Warn("seed injection rule failed", "err", err, "host", r.HostPattern)
		}
	}
}

