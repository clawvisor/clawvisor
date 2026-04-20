package handlers

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/policy"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// ProxyHandler serves the proxy-facing API: config fetch, TurnEvent
// ingest, signing-key registration. Authenticated via cvisproxy_... token
// through RequireProxy middleware.
//
// See docs/proxy-api.md for the normative wire contract.
type ProxyHandler struct {
	st     store.Store
	logger *slog.Logger
}

func NewProxyHandler(st store.Store, logger *slog.Logger) *ProxyHandler {
	return &ProxyHandler{st: st, logger: logger}
}

// -- GET /api/proxy/config ----------------------------------------------------

type proxyConfigAgent struct {
	AgentTokenID string `json:"agent_token_id"`
	AgentLabel   string `json:"agent_label"`
}

type proxyConfigResponse struct {
	ContractVersion  string                  `json:"contract_version"`
	ProxyInstanceID  string                  `json:"proxy_instance_id"`
	BridgeID         string                  `json:"bridge_id"`
	Agents           []proxyConfigAgent      `json:"agents"`
	ProviderParsers  []string                `json:"provider_parsers"`
	ServerTime       string                  `json:"server_time"`
	ConfigTTLSeconds int                     `json:"config_ttl_seconds"`
	// Stage 3 M2: policy engine.
	Policy *policy.CompiledPolicy `json:"policy,omitempty"`
	Bans   []proxyConfigBan       `json:"bans,omitempty"`
}

type proxyConfigBan struct {
	AgentTokenID string `json:"agent_token_id"`
	RuleName     string `json:"rule_name,omitempty"`
	ExpiresAt    string `json:"expires_at"`
}

// Config handles GET /api/proxy/config. Returns runtime config the proxy
// refreshes periodically (default 60s). Includes the list of agent tokens
// the proxy should accept via Proxy-Authorization.
func (h *ProxyHandler) Config(w http.ResponseWriter, r *http.Request) {
	p := middleware.ProxyFromContext(r.Context())
	if p == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	bt, err := h.st.GetBridgeTokenByID(r.Context(), p.BridgeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusForbidden, "BRIDGE_REVOKED", "bridge not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load bridge")
		return
	}
	if bt.RevokedAt != nil {
		writeError(w, http.StatusForbidden, "BRIDGE_REVOKED", "bridge has been revoked")
		return
	}

	agents, err := h.st.ListAgents(r.Context(), bt.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list agents")
		return
	}
	// Filter to agents attached to this bridge. Today all agents of a
	// user are implicitly bridge-connected when the bridge exists; Stage
	// 2 tightens this. For Stage 1 we include every active agent.
	configAgents := make([]proxyConfigAgent, 0, len(agents))
	for _, a := range agents {
		configAgents = append(configAgents, proxyConfigAgent{
			AgentTokenID: a.ID,
			AgentLabel:   a.Name,
		})
	}

	// Stage 3 M2: deliver the compiled policy + active bans on every
	// config refresh. Enforcement + ban check happen client-side at the
	// proxy — the server just publishes the authoritative state.
	var compiled *policy.CompiledPolicy
	if pol, err := h.st.GetPolicyByBridge(r.Context(), p.BridgeID); err == nil && pol != nil && pol.Enabled && pol.CompiledJSON != "" {
		cp := &policy.CompiledPolicy{}
		if uerr := json.Unmarshal([]byte(pol.CompiledJSON), cp); uerr == nil {
			compiled = cp
		} else {
			h.logger.Warn("proxy/config: bad compiled_json", "bridge_id", p.BridgeID, "err", uerr)
		}
	}
	var activeBans []proxyConfigBan
	if bans, err := h.st.ListActiveBans(r.Context(), p.BridgeID); err == nil {
		for _, b := range bans {
			activeBans = append(activeBans, proxyConfigBan{
				AgentTokenID: b.AgentTokenID,
				RuleName:     b.RuleName,
				ExpiresAt:    b.ExpiresAt.UTC().Format(time.RFC3339),
			})
		}
	}

	writeJSON(w, http.StatusOK, proxyConfigResponse{
		ContractVersion:  "v1-draft",
		ProxyInstanceID:  p.ID,
		BridgeID:         p.BridgeID,
		Agents:           configAgents,
		ProviderParsers:  []string{"anthropic", "openai", "telegram"},
		ServerTime:       time.Now().UTC().Format(time.RFC3339Nano),
		ConfigTTLSeconds: 60,
		Policy:           compiled,
		Bans:             activeBans,
	})
}

// -- POST /api/proxy/policy-violations ---------------------------------------
//
// Stage 3 M2. Proxy calls this after it applies a block or flag decision
// so the server can build the violation feed + evaluate bans.

type policyViolationRequest struct {
	AgentTokenID    string `json:"agent_token_id"`
	RuleName        string `json:"rule_name"`
	Action          string `json:"action"` // "block" | "flag"
	RequestID       string `json:"request_id,omitempty"`
	DestinationHost string `json:"destination_host,omitempty"`
	DestinationPath string `json:"destination_path,omitempty"`
	Method          string `json:"method,omitempty"`
	Message         string `json:"message,omitempty"`
}

type policyViolationResponse struct {
	Recorded bool `json:"recorded"`
	// If the violation pushed the agent over the ban threshold, Ban is
	// populated. Proxy can reject the agent immediately instead of
	// waiting for the next config refresh.
	Ban *proxyConfigBan `json:"ban,omitempty"`
}

// PolicyViolations handles POST /api/proxy/policy-violations.
func (h *ProxyHandler) PolicyViolations(w http.ResponseWriter, r *http.Request) {
	p := middleware.ProxyFromContext(r.Context())
	if p == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	var req policyViolationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RuleName == "" || req.Action == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "rule_name and action are required")
		return
	}
	if req.Action != "block" && req.Action != "flag" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "action must be block or flag")
		return
	}
	v := &store.PolicyViolation{
		BridgeID:        p.BridgeID,
		AgentTokenID:    req.AgentTokenID,
		RuleName:        req.RuleName,
		Action:          req.Action,
		RequestID:       req.RequestID,
		DestinationHost: req.DestinationHost,
		DestinationPath: req.DestinationPath,
		Method:          req.Method,
		Message:         req.Message,
	}
	if err := h.st.InsertPolicyViolation(r.Context(), v); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not record violation")
		return
	}

	// Ban evaluation: only block violations count; flag is observational.
	resp := policyViolationResponse{Recorded: true}
	if req.Action == "block" && req.AgentTokenID != "" {
		if pol, err := h.st.GetPolicyByBridge(r.Context(), p.BridgeID); err == nil && pol != nil && pol.Enabled && pol.CompiledJSON != "" {
			cp := &policy.CompiledPolicy{}
			if uerr := json.Unmarshal([]byte(pol.CompiledJSON), cp); uerr == nil && cp.Ban.Enabled && cp.Ban.MaxViolations > 0 && cp.Ban.Window > 0 {
				since := time.Now().Add(-cp.Ban.Window)
				n, cerr := h.st.CountPolicyViolationsForAgent(r.Context(), p.BridgeID, req.AgentTokenID, req.RuleName, since)
				if cerr == nil && n >= cp.Ban.MaxViolations {
					ban := &store.AgentBan{
						BridgeID:       p.BridgeID,
						AgentTokenID:   req.AgentTokenID,
						RuleName:       req.RuleName,
						ExpiresAt:      time.Now().Add(cp.Ban.Duration),
						ViolationCount: n,
					}
					if berr := h.st.UpsertAgentBan(r.Context(), ban); berr == nil {
						resp.Ban = &proxyConfigBan{
							AgentTokenID: ban.AgentTokenID,
							RuleName:     ban.RuleName,
							ExpiresAt:    ban.ExpiresAt.UTC().Format(time.RFC3339),
						}
					}
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// -- POST /api/proxy/turns ----------------------------------------------------

// turnEventRequest mirrors the TurnEvent schema on the wire (docs/proxy-api.md §8.1).
type turnEventRequest struct {
	EventID        string          `json:"event_id"`
	TS             string          `json:"ts"`
	Source         string          `json:"source"`
	SourceVersion  string          `json:"source_version"`
	Stream         string          `json:"stream"`
	AgentTokenID   string          `json:"agent_token_id"`
	BridgeID       string          `json:"bridge_id"`
	ConversationID string          `json:"conversation_id"`
	Provider       string          `json:"provider"`
	Direction      string          `json:"direction"`
	Role           string          `json:"role"`
	Turn           turnPayload     `json:"turn"`
	RawRef         json.RawMessage `json:"raw_ref,omitempty"`
	Signature      json.RawMessage `json:"signature,omitempty"`
}

type turnPayload struct {
	Text         string          `json:"text,omitempty"`
	ToolCalls    json.RawMessage `json:"tool_calls,omitempty"`
	ToolResults  json.RawMessage `json:"tool_results,omitempty"`
}

type turnsIngestRequest struct {
	Events []turnEventRequest `json:"events"`
}

type turnWarning struct {
	EventID string `json:"event_id"`
	Code    string `json:"code"`
	Error   string `json:"error"`
}

type turnsIngestResponse struct {
	Accepted int           `json:"accepted"`
	Rejected []turnWarning `json:"rejected,omitempty"`
	Warnings []turnWarning `json:"warnings,omitempty"`
}

// Turns handles POST /api/proxy/turns. Ingests a batch of TurnEvents with
// partial-failure semantics: per-event rejections reported inline, other
// events succeed. See docs/proxy-api.md §5.2.
func (h *ProxyHandler) Turns(w http.ResponseWriter, r *http.Request) {
	p := middleware.ProxyFromContext(r.Context())
	if p == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var req turnsIngestRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if len(req.Events) == 0 {
		writeJSON(w, http.StatusOK, turnsIngestResponse{Accepted: 0})
		return
	}
	if len(req.Events) > 1000 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "batch exceeds 1000 events")
		return
	}

	// Preload signing keys we'll need for verification. Stage 1 treats
	// verification as audit metadata — invalid signatures produce a
	// warning, not a rejection.
	sigKeys := make(map[string]ed25519.PublicKey)

	resp := turnsIngestResponse{}
	for _, ev := range req.Events {
		warns, rejected := h.processTurnEvent(r.Context(), p, &ev, sigKeys)
		if rejected != nil {
			resp.Rejected = append(resp.Rejected, *rejected)
			continue
		}
		resp.Accepted++
		resp.Warnings = append(resp.Warnings, warns...)
	}

	writeJSON(w, http.StatusOK, resp)
}

// processTurnEvent validates + persists one event. Returns either a
// rejection (event dropped) or a list of warnings (event accepted but
// flagged). Mutating operations on h.st happen here.
func (h *ProxyHandler) processTurnEvent(ctx context.Context, p *store.ProxyInstance, ev *turnEventRequest, sigCache map[string]ed25519.PublicKey) ([]turnWarning, *turnWarning) {
	// Basic shape validation. Cheap fast-fail before we touch the DB.
	if ev.EventID == "" {
		return nil, &turnWarning{EventID: ev.EventID, Code: "INVALID_REQUEST", Error: "event_id is required"}
	}
	if ev.Source == "" || ev.Stream == "" || ev.TS == "" {
		return nil, &turnWarning{EventID: ev.EventID, Code: "INVALID_REQUEST", Error: "source, stream, ts are required"}
	}
	if ev.BridgeID != "" && ev.BridgeID != p.BridgeID {
		return nil, &turnWarning{EventID: ev.EventID, Code: "BRIDGE_MISMATCH", Error: "event bridge_id does not match authenticated proxy's bridge"}
	}

	ts, err := parseEventTimestamp(ev.TS)
	if err != nil {
		return nil, &turnWarning{EventID: ev.EventID, Code: "INVALID_REQUEST", Error: fmt.Sprintf("invalid ts: %v", err)}
	}
	// Clamp far-future events (Stage 0 rule, inherited).
	if ts.After(time.Now().UTC().Add(60 * time.Second)) {
		return nil, &turnWarning{EventID: ev.EventID, Code: "FUTURE_TIMESTAMP", Error: "ts is more than 60s in the future"}
	}

	// Signature verification (audit only at Stage 1).
	sigStatus := "unsigned"
	var warnings []turnWarning
	if len(ev.Signature) > 0 && !bytes.Equal(ev.Signature, []byte("null")) {
		if ok, reason := h.verifySignature(ctx, p.ID, ev, sigCache); ok {
			sigStatus = "valid"
		} else {
			sigStatus = "invalid"
			warnings = append(warnings, turnWarning{
				EventID: ev.EventID,
				Code:    "SIGNATURE_INVALID",
				Error:   reason,
			})
		}
	}

	// Dedup by event_id.
	existing, err := h.st.GetTranscriptEventByID(ctx, ev.EventID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		h.logger.Error("transcript event lookup failed", "err", err)
		return nil, &turnWarning{EventID: ev.EventID, Code: "INTERNAL_ERROR", Error: "lookup failed"}
	}
	if existing != nil {
		return nil, &turnWarning{EventID: ev.EventID, Code: "DUPLICATE_EVENT", Error: "event_id already ingested"}
	}

	// Persist.
	te := &store.TranscriptEvent{
		EventID:         ev.EventID,
		BridgeID:        p.BridgeID,
		Source:          ev.Source,
		SourceVersion:   ev.SourceVersion,
		Stream:          ev.Stream,
		AgentTokenID:    ev.AgentTokenID,
		ConversationID:  ev.ConversationID,
		Provider:        ev.Provider,
		Direction:       ev.Direction,
		Role:            ev.Role,
		Text:            ev.Turn.Text,
		ToolCallsJSON:   jsonOrEmpty(ev.Turn.ToolCalls),
		ToolResultsJSON: jsonOrEmpty(ev.Turn.ToolResults),
		RawRefJSON:      jsonOrEmpty(ev.RawRef),
		SignatureJSON:   jsonOrEmpty(ev.Signature),
		SigStatus:       sigStatus,
		TS:              ts,
	}
	if err := h.st.InsertTranscriptEvent(ctx, te); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, &turnWarning{EventID: ev.EventID, Code: "DUPLICATE_EVENT", Error: "event_id already ingested"}
		}
		h.logger.Error("transcript event insert failed", "err", err, "event_id", ev.EventID)
		return nil, &turnWarning{EventID: ev.EventID, Code: "INTERNAL_ERROR", Error: "persist failed"}
	}
	return warnings, nil
}

// verifySignature looks up the proxy's signing key by key_id, verifies
// the event's signature against a canonical serialization. Returns
// (ok, reason-if-not-ok).
func (h *ProxyHandler) verifySignature(ctx context.Context, proxyInstanceID string, ev *turnEventRequest, sigCache map[string]ed25519.PublicKey) (bool, string) {
	var sig struct {
		Alg   string `json:"alg"`
		KeyID string `json:"key_id"`
		Sig   string `json:"sig"`
	}
	if err := json.Unmarshal(ev.Signature, &sig); err != nil {
		return false, "signature not parseable"
	}
	if sig.Alg != "ed25519" {
		return false, fmt.Sprintf("unsupported alg %q", sig.Alg)
	}

	pub, ok := sigCache[sig.KeyID]
	if !ok {
		key, err := h.st.GetProxySigningKey(ctx, proxyInstanceID, sig.KeyID)
		if err != nil {
			return false, fmt.Sprintf("key_id %q not registered", sig.KeyID)
		}
		raw, err := base64.StdEncoding.DecodeString(key.PublicKey)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return false, "stored public key is malformed"
		}
		pub = ed25519.PublicKey(raw)
		sigCache[sig.KeyID] = pub
	}

	sigBytes, err := base64.StdEncoding.DecodeString(sig.Sig)
	if err != nil {
		return false, "sig not base64"
	}

	// Canonical serialization: same event with signature field stripped.
	payload, err := canonicalizeForSigning(ev)
	if err != nil {
		return false, fmt.Sprintf("canonicalize: %v", err)
	}

	if !ed25519.Verify(pub, payload, sigBytes) {
		return false, "signature does not match payload"
	}
	return true, ""
}

// canonicalizeForSigning produces the byte-exact payload the proxy signs.
// Must match the proxy's canonicalization in third_party/proxy/internal/signer/
// exactly. See docs/proxy-api.md §8.1 canonical-serialization note.
//
// Stage 1 canonicalization: JSON-marshal the event with the signature
// field stripped, using Go's default encoder (no indent, no sort-keys,
// but map iteration is sorted). Because Go's json.Marshal sorts map keys
// by default for maps, and our struct fields emit in declaration order,
// this is stable per struct layout. Both sides must use the same struct.
func canonicalizeForSigning(ev *turnEventRequest) ([]byte, error) {
	copy := *ev
	copy.Signature = nil
	return json.Marshal(&copy)
}

// -- POST /api/proxy/signing-keys/rotate --------------------------------------

type signingKeyRotateRequest struct {
	KeyID     string `json:"key_id"`
	Alg       string `json:"alg"`
	PublicKey string `json:"public_key"`
}

type signingKeyRotateResponse struct {
	KeyID        string `json:"key_id"`
	RegisteredAt string `json:"registered_at"`
}

// SigningKeyRotate handles POST /api/proxy/signing-keys/rotate. Proxy
// registers a new public key at startup and on daily rotation. Idempotent
// on duplicate key_id (returns original registration).
func (h *ProxyHandler) SigningKeyRotate(w http.ResponseWriter, r *http.Request) {
	p := middleware.ProxyFromContext(r.Context())
	if p == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var req signingKeyRotateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.KeyID == "" || req.Alg == "" || req.PublicKey == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "key_id, alg, public_key are required")
		return
	}
	if req.Alg != "ed25519" {
		writeError(w, http.StatusBadRequest, "UNSUPPORTED_ALG", fmt.Sprintf("alg %q is not supported; Stage 1 requires ed25519", req.Alg))
		return
	}
	if _, err := base64.StdEncoding.DecodeString(req.PublicKey); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_KEY", "public_key must be base64-encoded")
		return
	}

	// Idempotent re-registration.
	existing, err := h.st.GetProxySigningKey(r.Context(), p.ID, req.KeyID)
	if err == nil {
		writeJSON(w, http.StatusOK, signingKeyRotateResponse{
			KeyID:        existing.KeyID,
			RegisteredAt: existing.RegisteredAt.UTC().Format(time.RFC3339Nano),
		})
		return
	}
	if !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "key lookup failed")
		return
	}

	key := &store.ProxySigningKey{
		ProxyInstanceID: p.ID,
		KeyID:           req.KeyID,
		Alg:             req.Alg,
		PublicKey:       req.PublicKey,
	}
	if err := h.st.RegisterProxySigningKey(r.Context(), key); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "key registration failed")
		return
	}
	writeJSON(w, http.StatusOK, signingKeyRotateResponse{
		KeyID:        req.KeyID,
		RegisteredAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// -- helpers -----------------------------------------------------------------

func parseEventTimestamp(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("timestamp %q is not RFC3339", s)
}

func jsonOrEmpty(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if bytes.Equal(raw, []byte("null")) {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
