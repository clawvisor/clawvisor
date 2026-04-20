package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/events"
	"github.com/clawvisor/clawvisor/internal/groupchat"
	"github.com/clawvisor/clawvisor/internal/installer"
	"github.com/clawvisor/clawvisor/pkg/store"
)

const (
	pluginPairRequestExpiry = 5 * time.Minute
	pluginPairTokenWindow   = 5 * time.Minute
	// pluginPairCodeExpiry bounds the window during which a minted code is
	// usable. Short enough that a leaked code has a tiny exploitation
	// window; long enough that the user can paste the setup link into an
	// agent without racing a timer.
	pluginPairCodeExpiry = 10 * time.Minute
	// idempotencyHeader is the header name used by the plugin to collapse
	// long-poll retries onto a single pair/agent-add request.
	idempotencyHeader = "Idempotency-Key"
)

// pluginPairEventType is the event name published when a plugin pair request
// changes state (approved / denied). Distinct from "queue" so unrelated
// dashboard views don't re-fetch on every pair transition, but also picked
// up by WaitFor with eventTypes=nil (any event) for simplicity.
const pluginPairEventType = "plugin_pair"

// pairTokenBundle is the short-lived result handed back to a waiting plugin
// after its pair request is approved. Held in memory (5 min) rather than
// persisted so raw tokens never land on disk; the plugin is responsible for
// writing them to its own config.
type pairTokenBundle struct {
	BridgeToken string            `json:"bridge_token"`
	Agents      map[string]string `json:"agents"` // agent_name → raw cvis_ token
	StoredAt    time.Time         `json:"-"`
}

// PluginPairingHandler manages the OpenClaw plugin pairing lifecycle: the
// plugin requests a pair, the dashboard user approves (minting a bridge
// token + agent tokens in one step), and the long-poll returns the raw
// tokens to the waiting plugin.
type PluginPairingHandler struct {
	st                  store.Store
	eventHub            events.EventHub
	logger              *slog.Logger
	serverVersion       string // for logging plugin/server version drift at pair time
	buffer              groupchat.Buffer // for the GET /bridges/{id}/buffer dump endpoint
	serverURLForInstall string // URL the proxy uses to reach this server (embedded into install artifact)

	// bundles caches token bundles by pair request ID for the approval
	// window. Raw tokens are never persisted, only delivered via the
	// plugin's long-poll response.
	mu      sync.RWMutex
	bundles map[string]pairTokenBundle
}

func NewPluginPairingHandler(st store.Store, eventHub events.EventHub, logger *slog.Logger) *PluginPairingHandler {
	h := &PluginPairingHandler{
		st:       st,
		eventHub: eventHub,
		logger:   logger,
		bundles:  make(map[string]pairTokenBundle),
	}
	return h
}

// SetServerVersion wires this Clawvisor build's version string so the
// pair handler can log plugin/server drift. Called from server setup.
func (h *PluginPairingHandler) SetServerVersion(v string) { h.serverVersion = v }

// SetBuffer wires the in-memory (or Redis-backed) message buffer so the
// GET /bridges/{id}/buffer dump endpoint can enumerate it.
func (h *PluginPairingHandler) SetBuffer(b groupchat.Buffer) { h.buffer = b }

// ── Pair request (unauthenticated, initiated by plugin) ──────────────────────

type requestPairBody struct {
	PairCode           string   `json:"pair_code"`
	InstallFingerprint string   `json:"install_fingerprint"`
	Hostname           string   `json:"hostname"`
	AgentIDs           []string `json:"agent_ids"`
	// PluginVersion is the version string the plugin reports (from its
	// embedded VERSION file). Used for operator visibility and to log
	// mismatches against the server's own version — we don't hard-reject
	// yet because we haven't established a compat policy. Later we can
	// tighten this if we see users running mismatched plugin/server
	// combinations in the wild.
	PluginVersion string `json:"plugin_version,omitempty"`
}

// RequestPair handles POST /api/plugin/pair (unauthenticated). The OpenClaw
// plugin calls this at install time after the agent deposits a one-time
// pair code into its config. The pair code is atomically consumed here so
// the same code cannot start two pair flows, and userID is derived from
// the code — never trusted from the request body.
//
// The Idempotency-Key header (optional but recommended) lets long-poll
// retries collapse onto a single pending pair request so the dashboard
// doesn't show duplicate cards when the client reconnects.
func (h *PluginPairingHandler) RequestPair(w http.ResponseWriter, r *http.Request) {
	var body requestPairBody
	if !decodeJSON(w, r, &body) {
		return
	}
	code := strings.TrimSpace(body.PairCode)
	if code == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "pair_code is required — generate one from the Clawvisor dashboard and paste the setup link into your agent")
		return
	}
	codeHash := auth.HashToken(code)

	// Log plugin version for operator visibility. If this starts showing
	// mismatches in production, that's the signal to introduce a real
	// compat policy here.
	if body.PluginVersion != "" && h.serverVersion != "" && body.PluginVersion != h.serverVersion {
		h.logger.Warn("plugin pair: version mismatch — plugin and server may drift out of compat",
			"plugin_version", body.PluginVersion,
			"server_version", h.serverVersion,
			"hostname", body.Hostname)
	}

	// Peek the code before consuming so we can resolve the user_id for the
	// idempotency check. An idempotent retry (same Idempotency-Key after a
	// long-poll drop) needs to find the original pending request even
	// though the code was already consumed on the first attempt.
	pc, err := h.st.GetPluginPairCodeByHash(r.Context(), codeHash)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_PAIR_CODE", "pair code is invalid, already used, or expired — generate a fresh one from the dashboard")
		return
	}
	userID := pc.UserID

	idemKey := strings.TrimSpace(r.Header.Get(idempotencyHeader))
	if idemKey != "" {
		if existing, err := h.st.GetPluginPairRequestByIdempotencyKey(r.Context(), userID, idemKey); err == nil {
			// Retry of a prior successful attempt — no need to re-consume.
			h.resumePairOrWait(w, r, existing)
			return
		}
	}

	// First-time consumption (atomic). Returns ErrNotFound if already
	// consumed or expired — single error path so an attacker can't probe
	// for which condition failed.
	if _, err := h.st.ConsumePluginPairCode(r.Context(), codeHash); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_PAIR_CODE", "pair code is invalid, already used, or expired — generate a fresh one from the dashboard")
		return
	}

	req := &store.PluginPairRequest{
		UserID:             userID,
		InstallFingerprint: body.InstallFingerprint,
		Hostname:           body.Hostname,
		AgentIDs:           body.AgentIDs,
		Status:             "pending",
		IdempotencyKey:     idemKey,
		ExpiresAt:          time.Now().Add(pluginPairRequestExpiry),
	}
	if err := h.st.CreatePluginPairRequest(r.Context(), req); err != nil {
		h.logger.Warn("create plugin pair request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create pair request")
		return
	}
	if h.eventHub != nil {
		h.eventHub.Publish(userID, events.Event{Type: pluginPairEventType})
	}
	h.resumePairOrWait(w, r, req)
}

// resumePairOrWait either long-polls for resolution (wait=true) or returns
// the pending pair request body immediately. Shared by fresh-request and
// idempotent-retry paths.
func (h *PluginPairingHandler) resumePairOrWait(w http.ResponseWriter, r *http.Request, req *store.PluginPairRequest) {
	if req.Status != "pending" {
		h.writePairResult(w, req)
		return
	}
	if r.URL.Query().Get("wait") == "true" && h.eventHub != nil {
		timeout := parseLongPollTimeout(r)
		resolved := h.waitForPairResolution(r.Context(), req.ID, req.UserID, time.Duration(timeout)*time.Second)
		h.writePairResult(w, resolved)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"pair_id":    req.ID,
		"status":     req.Status,
		"expires_at": req.ExpiresAt,
	})
}

// PollPair handles GET /api/plugin/pair/{id}?wait=true — the plugin's
// long-poll endpoint for resuming a pair request after a reconnect.
func (h *PluginPairingHandler) PollPair(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	pr, err := h.st.GetPluginPairRequest(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "pair request not found")
		} else {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not fetch pair request")
		}
		return
	}
	if pr.Status != "pending" {
		h.writePairResult(w, pr)
		return
	}
	if r.URL.Query().Get("wait") != "true" || h.eventHub == nil {
		h.writePairResult(w, pr)
		return
	}
	timeout := parseLongPollTimeout(r)
	resolved := h.waitForPairResolution(r.Context(), id, pr.UserID, time.Duration(timeout)*time.Second)
	h.writePairResult(w, resolved)
}

func (h *PluginPairingHandler) writePairResult(w http.ResponseWriter, pr *store.PluginPairRequest) {
	resp := map[string]any{
		"pair_id":    pr.ID,
		"status":     pr.Status,
		"expires_at": pr.ExpiresAt,
	}
	if pr.Status == "approved" {
		if bundle, ok := h.loadBundle(pr.ID); ok {
			resp["bridge_token"] = bundle.BridgeToken
			resp["agents"] = bundle.Agents
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *PluginPairingHandler) waitForPairResolution(ctx context.Context, pairID, userID string, timeout time.Duration) *store.PluginPairRequest {
	return events.WaitFor(ctx, h.eventHub, userID, timeout,
		nil,
		func(c context.Context) (*store.PluginPairRequest, bool) {
			pr, err := h.st.GetPluginPairRequest(c, pairID)
			if err != nil {
				return &store.PluginPairRequest{ID: pairID, Status: "pending"}, false
			}
			if pr.Status == "pending" && time.Now().After(pr.ExpiresAt) {
				_ = h.st.UpdatePluginPairRequestStatus(c, pairID, "expired", "")
				pr.Status = "expired"
			}
			return pr, pr.Status != "pending"
		},
	)
}

// ── Approve / deny (user JWT) ────────────────────────────────────────────────

type approvePairBody struct {
	// AutoApprovalEnabled — the consent checkbox shown alongside the pair
	// approval. Stored on the resulting bridge_token row so the user can
	// flip it later from the Agents page without re-pairing.
	AutoApprovalEnabled bool `json:"auto_approval_enabled"`
}

// ApprovePair handles POST /api/plugin/pair/{id}/approve (user JWT). Mints
// a bridge token plus one agent token per AgentID on the pair request; raw
// tokens are cached for pluginPairTokenWindow so the plugin's long-poll
// response can deliver them.
func (h *PluginPairingHandler) ApprovePair(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	id := r.PathValue("id")

	var body approvePairBody
	_ = json.NewDecoder(r.Body).Decode(&body) // body is optional

	pr, err := h.st.GetPluginPairRequest(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "pair request not found")
		} else {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not fetch pair request")
		}
		return
	}
	if pr.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your pair request")
		return
	}
	if pr.Status != "pending" {
		writeError(w, http.StatusConflict, "ALREADY_RESOLVED", "pair request is not pending")
		return
	}
	if time.Now().After(pr.ExpiresAt) {
		_ = h.st.UpdatePluginPairRequestStatus(r.Context(), id, "expired", "")
		writeError(w, http.StatusGone, "EXPIRED", "pair request has expired")
		return
	}

	// Generate raw tokens in-memory. If any DB write later fails, these
	// simply never reach the user — nothing to roll back outside the tx.
	var rawBridgeToken string
	var newBridge *store.NewBridgeInput
	if pr.BridgeTokenID == "" {
		raw, err := auth.GenerateBridgeToken()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not generate bridge token")
			return
		}
		rawBridgeToken = raw
		newBridge = &store.NewBridgeInput{
			TokenHash:           auth.HashToken(raw),
			InstallFingerprint:  pr.InstallFingerprint,
			Hostname:            pr.Hostname,
			AutoApprovalEnabled: body.AutoApprovalEnabled,
		}
	}
	agentTokens := make(map[string]string, len(pr.AgentIDs))
	agentInputs := make([]store.AgentMintInput, 0, len(pr.AgentIDs))
	for _, name := range pr.AgentIDs {
		raw, err := auth.GenerateAgentToken()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not generate agent token")
			return
		}
		agentTokens[name] = raw
		agentInputs = append(agentInputs, store.AgentMintInput{Name: name, TokenHash: auth.HashToken(raw)})
	}

	// Atomic commit: bridge (if needed) + all agent rows + pair-request
	// status flip happen in one transaction. Partial failure leaves no
	// state changes and the user can re-click Approve.
	out, err := h.st.ApprovePluginPair(r.Context(), store.ApprovePluginPairInput{
		PairRequestID: id,
		UserID:        user.ID,
		NewBridge:     newBridge,
		Agents:        agentInputs,
	})
	if err != nil {
		h.logger.Warn("plugin pair approve failed", "err", err, "pair_id", id)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not approve pair request")
		return
	}
	bridgeTokenID := out.BridgeTokenID

	h.storeBundle(id, pairTokenBundle{
		BridgeToken: rawBridgeToken,
		Agents:      agentTokens,
		StoredAt:    time.Now(),
	})
	if h.eventHub != nil {
		h.eventHub.Publish(user.ID, events.Event{Type: pluginPairEventType})
		h.eventHub.Publish(user.ID, events.Event{Type: "queue"}) // also refresh dashboard lists
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "approved",
		"bridge_token_id": bridgeTokenID,
	})
}

// DenyPair handles POST /api/plugin/pair/{id}/deny (user JWT).
func (h *PluginPairingHandler) DenyPair(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	id := r.PathValue("id")

	pr, err := h.st.GetPluginPairRequest(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "pair request not found")
		} else {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not fetch pair request")
		}
		return
	}
	if pr.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your pair request")
		return
	}
	if pr.Status != "pending" {
		writeError(w, http.StatusConflict, "ALREADY_RESOLVED", "pair request is not pending")
		return
	}
	if err := h.st.UpdatePluginPairRequestStatus(r.Context(), id, "denied", ""); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not update pair request status")
		return
	}
	if h.eventHub != nil {
		h.eventHub.Publish(user.ID, events.Event{Type: pluginPairEventType})
		h.eventHub.Publish(user.ID, events.Event{Type: "queue"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "denied"})
}

// ListPendingPairs handles GET /api/plugin/pair/pending (user JWT) — used
// by the dashboard to surface pending pair requests alongside other
// approvals. Returns store.PluginPairRequest items; the dashboard can tell
// initial-pair from agent-add by whether BridgeTokenID is populated.
func (h *PluginPairingHandler) ListPendingPairs(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	reqs, err := h.st.ListPendingPluginPairRequests(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list pair requests")
		return
	}
	writeJSON(w, http.StatusOK, reqs)
}

// ── Post-pair agent additions (bridge-authenticated) ─────────────────────────

type requestAgentAddBody struct {
	AgentID string `json:"agent_id"`
}

// RequestAgentAdd handles POST /api/plugin/agents (bridge token). A
// plugin that's already paired uses this to request an additional agent
// token; surfaces as a pending pair request in the dashboard so the user
// still gets per-agent audit, but the bridge token itself isn't re-minted.
// Honors the same Idempotency-Key header as the initial pair so retries
// collapse onto a single pending approval.
func (h *PluginPairingHandler) RequestAgentAdd(w http.ResponseWriter, r *http.Request) {
	bridge := middleware.BridgeFromContext(r.Context())
	if bridge == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	var body requestAgentAddBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.AgentID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "agent_id is required")
		return
	}

	idemKey := strings.TrimSpace(r.Header.Get(idempotencyHeader))
	if idemKey != "" {
		if existing, err := h.st.GetPluginPairRequestByIdempotencyKey(r.Context(), bridge.UserID, idemKey); err == nil {
			h.resumePairOrWait(w, r, existing)
			return
		}
	}

	req := &store.PluginPairRequest{
		UserID:             bridge.UserID,
		InstallFingerprint: bridge.InstallFingerprint,
		Hostname:           bridge.Hostname,
		AgentIDs:           []string{body.AgentID},
		Status:             "pending",
		BridgeTokenID:      bridge.ID,
		IdempotencyKey:     idemKey,
		ExpiresAt:          time.Now().Add(pluginPairRequestExpiry),
	}
	if err := h.st.CreatePluginPairRequest(r.Context(), req); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create agent add request")
		return
	}
	if h.eventHub != nil {
		h.eventHub.Publish(bridge.UserID, events.Event{Type: pluginPairEventType})
	}
	h.resumePairOrWait(w, r, req)
}

// ── Pair codes (user JWT, dashboard) ────────────────────────────────────────

// MintPairCode handles POST /api/plugin/pair-codes (user JWT). Mints a
// short-lived one-time capability the user pastes into their OpenClaw
// install (via the /skill/setup-openclaw setup link). The raw code is
// returned once; only its hash is stored.
func (h *PluginPairingHandler) MintPairCode(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	raw, err := auth.GeneratePluginPairCode()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not generate pair code")
		return
	}
	pc := &store.PluginPairCode{
		UserID:    user.ID,
		CodeHash:  auth.HashToken(raw),
		ExpiresAt: time.Now().Add(pluginPairCodeExpiry),
	}
	if err := h.st.CreatePluginPairCode(r.Context(), pc); err != nil {
		h.logger.Warn("create plugin pair code failed", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not persist pair code")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"code":       raw,
		"expires_at": pc.ExpiresAt,
	})
}

// ── Bridges management (user JWT, dashboard) ─────────────────────────────────

// ListBridges handles GET /api/plugin/bridges (user JWT). Returns the
// user's bridge tokens for the Agents page — never includes the token
// hash, only metadata (hostname, fingerprint, toggles, timestamps).
func (h *PluginPairingHandler) ListBridges(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	bridges, err := h.st.ListBridgeTokens(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list bridges")
		return
	}
	writeJSON(w, http.StatusOK, bridges)
}

type patchBridgeBody struct {
	AutoApprovalEnabled *bool `json:"auto_approval_enabled,omitempty"`
}

// PatchBridge handles PATCH /api/plugin/bridges/{id} (user JWT). Currently
// supports toggling auto_approval_enabled — the dashboard checkbox next to
// the bridge row on the Agents page.
func (h *PluginPairingHandler) PatchBridge(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	id := r.PathValue("id")

	var body patchBridgeBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.AutoApprovalEnabled == nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "no supported fields provided")
		return
	}
	if err := h.st.UpdateBridgeTokenAutoApproval(r.Context(), id, user.ID, *body.AutoApprovalEnabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "bridge not found or revoked")
		} else {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not update bridge")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// BufferForBridge handles GET /api/plugin/bridges/{id}/buffer (user JWT).
// Returns the current in-memory buffer contents for the bridge, grouped
// by conversation. Scoped by the bridge's owning user, so a dashboard
// user can only see their own buffers. Useful for debugging "did this
// message actually land?" without tailing server logs.
//
// Response shape:
//
//	{
//	  "bridge_id": "...",
//	  "conversations": {
//	    "slack:C0123/thread:A": [
//	      {"ts":"…","role":"user","sender":"Alice","text":"…","seq":1,"event_id":"…"},
//	      …
//	    ],
//	    …
//	  }
//	}
func (h *PluginPairingHandler) BufferForBridge(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	id := r.PathValue("id")

	bt, err := h.st.GetBridgeTokenByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "bridge not found")
		} else {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load bridge")
		}
		return
	}
	if bt.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your bridge")
		return
	}
	if h.buffer == nil {
		writeError(w, http.StatusServiceUnavailable, "BUFFER_UNAVAILABLE", "message buffer is not configured")
		return
	}

	// Keys are `u:{user_id}:{conversation}` — enumerate just this user's
	// scope. Strip the prefix for a cleaner dashboard display.
	userPrefix := "u:" + user.ID + ":"
	raw := h.buffer.MessagesForKeyPrefix(userPrefix)
	out := make(map[string][]map[string]any, len(raw))
	for key, msgs := range raw {
		convo := strings.TrimPrefix(key, userPrefix)
		entries := make([]map[string]any, 0, len(msgs))
		for _, m := range msgs {
			// Filter to messages attributable to THIS bridge so a dashboard
			// user looking at bridge A doesn't see traffic ingested by
			// bridge B under the same user.
			if m.BridgeID != "" && m.BridgeID != id {
				continue
			}
			entries = append(entries, map[string]any{
				"ts":       m.Timestamp.UTC().Format(time.RFC3339),
				"role":     m.Role,
				"sender":   m.SenderName,
				"text":     m.Text,
				"seq":      m.Seq,
				"event_id": m.EventID,
			})
		}
		if len(entries) > 0 {
			out[convo] = entries
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bridge_id":     id,
		"conversations": out,
	})
}

// RevokeBridge handles DELETE /api/plugin/bridges/{id} (user JWT). Marks
// revoked_at; the token immediately stops working on the next request
// (middleware checks revoked_at). No attempt is made to invalidate agent
// tokens that were minted alongside — those are separate identities.
func (h *PluginPairingHandler) RevokeBridge(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	id := r.PathValue("id")
	if err := h.st.RevokeBridgeToken(r.Context(), id, user.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "bridge not found or already revoked")
		} else {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not revoke bridge")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ── Token bundle cache ────────────────────────────────────────────────────────

func (h *PluginPairingHandler) storeBundle(pairID string, bundle pairTokenBundle) {
	h.mu.Lock()
	h.bundles[pairID] = bundle
	// Opportunistic cleanup: drop any bundles past the token window.
	cutoff := time.Now().Add(-pluginPairTokenWindow)
	for id, b := range h.bundles {
		if b.StoredAt.Before(cutoff) {
			delete(h.bundles, id)
		}
	}
	h.mu.Unlock()
}

func (h *PluginPairingHandler) loadBundle(pairID string) (pairTokenBundle, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	b, ok := h.bundles[pairID]
	if !ok {
		return pairTokenBundle{}, false
	}
	if time.Since(b.StoredAt) > pluginPairTokenWindow {
		return pairTokenBundle{}, false
	}
	return b, true
}

// ── Proxy enablement + install artifact (Stage 1) ────────────────────────────

// SetServerURL tells the handler how to populate the `server_url` field of
// the install artifact — this is how the proxy (running in Docker) will
// reach the Clawvisor server. Typically "http://clawvisor-server:25297"
// for compose deployments or "http://host.docker.internal:25297" for
// proxy-in-docker / server-on-host setups. Set from server setup.
func (h *PluginPairingHandler) SetServerURL(u string) { h.serverURLForInstall = u }

// enableProxyResponse is returned from POST /enable-proxy. Includes the
// install artifact directly so the dashboard can offer a one-click
// "Download docker-compose.yml / install.sh" flow.
type enableProxyResponse struct {
	BridgeID          string `json:"bridge_id"`
	ProxyInstanceID   string `json:"proxy_instance_id"`
	ProxyToken        string `json:"proxy_token,omitempty"` // returned ONCE; user re-enables to rotate
	GeneratedAt       string `json:"generated_at"`
	DockerComposeYAML string `json:"docker_compose_yaml"`
	InstallScript     string `json:"install_script"`
	ProxyConfigYAML   string `json:"proxy_config_yaml"`
	PluginSecretsJSON string `json:"plugin_secrets_json"`
}

// EnableProxy handles POST /api/plugin/bridges/{id}/enable-proxy.
// Requires user JWT. Flips bridge_tokens.proxy_enabled = true, mints a
// fresh cvisproxy_... token, creates a proxy_instance row, and returns
// the rendered install artifact. The cvisproxy_ token is shown ONCE.
// Re-run this endpoint to rotate.
func (h *PluginPairingHandler) EnableProxy(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	id := r.PathValue("id")

	bt, err := h.st.GetBridgeTokenByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "bridge not found")
		} else {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load bridge")
		}
		return
	}
	if bt.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your bridge")
		return
	}
	if bt.RevokedAt != nil {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "bridge has been revoked")
		return
	}

	// If a previous proxy_instance exists, revoke it (one active proxy per
	// bridge at a time; users rotate by re-running enable-proxy).
	if existing, err := h.st.GetProxyInstanceForBridge(r.Context(), id); err == nil && existing != nil {
		_ = h.st.RevokeProxyInstance(r.Context(), existing.ID)
	}

	rawToken, err := auth.GenerateProxyToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "token generation failed")
		return
	}
	proxyInstance := &store.ProxyInstance{
		BridgeID:  id,
		TokenHash: auth.HashToken(rawToken),
	}
	if err := h.st.CreateProxyInstance(r.Context(), proxyInstance); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "proxy instance creation failed")
		return
	}

	if err := h.st.SetBridgeProxyEnabled(r.Context(), id, user.ID, true); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not enable proxy")
		return
	}

	artifact, err := h.renderInstallArtifact(r.Context(), id, rawToken)
	if err != nil {
		h.logger.Error("render install artifact", "err", err, "bridge_id", id)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not render install artifact")
		return
	}

	writeJSON(w, http.StatusOK, enableProxyResponse{
		BridgeID:          id,
		ProxyInstanceID:   proxyInstance.ID,
		ProxyToken:        rawToken,
		GeneratedAt:       artifact.GeneratedAt,
		DockerComposeYAML: artifact.DockerComposeYAML,
		InstallScript:     artifact.InstallScript,
		ProxyConfigYAML:   artifact.ProxyConfigYAML,
		PluginSecretsJSON: artifact.PluginSecretsJSON,
	})
}

// DisableProxy handles POST /api/plugin/bridges/{id}/disable-proxy.
// Flips proxy_enabled off, revokes the active proxy instance. User is
// responsible for tearing down their local proxy container.
func (h *PluginPairingHandler) DisableProxy(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	id := r.PathValue("id")

	bt, err := h.st.GetBridgeTokenByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "bridge not found")
		} else {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load bridge")
		}
		return
	}
	if bt.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your bridge")
		return
	}

	if existing, err := h.st.GetProxyInstanceForBridge(r.Context(), id); err == nil && existing != nil {
		_ = h.st.RevokeProxyInstance(r.Context(), existing.ID)
	}
	if err := h.st.SetBridgeProxyEnabled(r.Context(), id, user.ID, false); err != nil && !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not disable proxy")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// renderInstallArtifact produces the per-bridge docker-compose / install
// script with concrete tokens embedded. Called by EnableProxy (with the
// freshly-minted raw proxy token) and InstallArtifact (with a
// rotate-required placeholder).
func (h *PluginPairingHandler) renderInstallArtifact(ctx context.Context, bridgeID, rawProxyToken string) (*installer.Artifact, error) {
	bt, err := h.st.GetBridgeTokenByID(ctx, bridgeID)
	if err != nil {
		return nil, err
	}
	// The install artifact also embeds the plugin's secrets so the
	// compose bootstrap writes them into the shared volume. We DON'T have
	// the raw bridge/agent tokens at this point (they were handed back
	// only during the original pair flow) — for now we emit placeholders
	// and require the user to paste them in. Better UX: extend this later
	// to deliver tokens via a user-side re-auth step. Stage 1 accepts the
	// paste-step and documents it.
	//
	// Plugin secrets placeholders instruct the user to fill them in; the
	// installer won't start OpenClaw with empty secrets.
	input := installer.Input{
		BridgeID:   bridgeID,
		ServerURL:  h.serverURLForInstall,
		ProxyToken: rawProxyToken,
		ProxyOnly:  bt.IsProxyOnly,
	}
	if !bt.IsProxyOnly {
		input.BridgeToken = "<PASTE_YOUR_BRIDGE_TOKEN_HERE>"
		input.AgentTokens = map[string]string{"main": "<PASTE_YOUR_AGENT_TOKEN_HERE>"}
	}
	if input.ServerURL == "" {
		input.ServerURL = "http://clawvisor-server:25297"
	}
	art, err := installer.Render(input)
	if err != nil {
		return nil, err
	}
	return art, nil
}

// CreateProxyOnlyBridge handles POST /api/plugin/bridges/proxy-only.
// Mints a brand-new "proxy-only" bridge (no OpenClaw plugin pair),
// enables the proxy on it, and returns the install artifact including
// the one-time cvisproxy_ token. Intended for Claude Code / Cursor
// users who want the Network Proxy + credential injection without
// running the plugin. Stage 2 M4.
func (h *PluginPairingHandler) CreateProxyOnlyBridge(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	// Optional user-provided label, just for the dashboard. No token is
	// issued for the bridge (no plugin will authenticate as it), so we
	// store a placeholder hash keyed to the fresh bridge id.
	var req struct {
		Hostname string `json:"hostname,omitempty"`
	}
	// Best-effort body decode; empty body is fine for this endpoint.
	if r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	hostname := strings.TrimSpace(req.Hostname)
	if hostname == "" {
		hostname = "proxy-only"
	}

	// The token_hash column is NOT NULL UNIQUE — proxy-only bridges have
	// no plugin token, so we synthesize a per-bridge sentinel hash using
	// a short random suffix. Raw proxy token is generated separately.
	randomSuffix, err := auth.GenerateBridgeToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not generate bridge id")
		return
	}
	syntheticHash := "proxy-only:" + auth.HashToken(randomSuffix)

	bt := &store.BridgeToken{
		UserID:              user.ID,
		TokenHash:           syntheticHash,
		InstallFingerprint:  "proxy-only",
		Hostname:            hostname,
		AutoApprovalEnabled: false,
		IsProxyOnly:         true,
	}
	if err := h.st.CreateBridgeToken(r.Context(), bt); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create bridge")
		return
	}
	bridgeID := bt.ID

	rawToken, err := auth.GenerateProxyToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "token generation failed")
		return
	}
	proxyInstance := &store.ProxyInstance{
		BridgeID:  bridgeID,
		TokenHash: auth.HashToken(rawToken),
	}
	if err := h.st.CreateProxyInstance(r.Context(), proxyInstance); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "proxy instance creation failed")
		return
	}
	if err := h.st.SetBridgeProxyEnabled(r.Context(), bridgeID, user.ID, true); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not enable proxy")
		return
	}

	artifact, err := h.renderInstallArtifact(r.Context(), bridgeID, rawToken)
	if err != nil {
		h.logger.Error("render install artifact (proxy-only)", "err", err, "bridge_id", bridgeID)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not render install artifact")
		return
	}
	writeJSON(w, http.StatusOK, enableProxyResponse{
		BridgeID:          bridgeID,
		ProxyInstanceID:   proxyInstance.ID,
		ProxyToken:        rawToken,
		GeneratedAt:       artifact.GeneratedAt,
		DockerComposeYAML: artifact.DockerComposeYAML,
		InstallScript:     artifact.InstallScript,
		ProxyConfigYAML:   artifact.ProxyConfigYAML,
		PluginSecretsJSON: artifact.PluginSecretsJSON,
	})
}

// ── Plugin runtime config (read by plugin at startup) ──────────────────────

type bridgeRuntimeConfigResponse struct {
	BridgeID         string `json:"bridge_id"`
	ProxyEnabled     bool   `json:"proxy_enabled"`
	// ScavengerEnabled tells the plugin whether to run its JSONL-scavenger
	// code path. Always the inverse of ProxyEnabled: when the proxy is
	// authoritative for transcripts we don't want a second tamperable
	// source. Plugin reads this at startup and on a periodic poll.
	ScavengerEnabled bool `json:"scavenger_enabled"`
}

// BridgeSelfConfig handles GET /api/plugin/bridges/self/config.
// Authenticated by the bridge token itself (not user JWT). Returns the
// runtime flags the plugin reads at startup + heartbeat. Scoped tight —
// no user data, just flags the plugin needs to decide how to behave.
func (h *PluginPairingHandler) BridgeSelfConfig(w http.ResponseWriter, r *http.Request) {
	bridge := middleware.BridgeFromContext(r.Context())
	if bridge == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	writeJSON(w, http.StatusOK, bridgeRuntimeConfigResponse{
		BridgeID:         bridge.ID,
		ProxyEnabled:     bridge.ProxyEnabled,
		ScavengerEnabled: !bridge.ProxyEnabled,
	})
}

// InstallArtifact handles GET /api/plugin/bridges/{id}/install-artifact.
// Returns the current artifact for an already-enabled bridge. Does NOT
// return the proxy token (that's shown only once on enable). Useful for
// re-downloading compose/install files after the initial enable response.
func (h *PluginPairingHandler) InstallArtifact(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	id := r.PathValue("id")

	bt, err := h.st.GetBridgeTokenByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "bridge not found")
		} else {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load bridge")
		}
		return
	}
	if bt.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your bridge")
		return
	}
	if !bt.ProxyEnabled {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "proxy is not enabled for this bridge")
		return
	}

	// Re-render without the raw proxy token. Docker-compose still has the
	// placeholder but operators who already ran enable-proxy once will
	// have saved it; this endpoint is for re-downloading the templates.
	artifact, err := h.renderInstallArtifact(r.Context(), id, "<ROTATE-TO-GET-NEW-TOKEN>")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not render install artifact")
		return
	}
	writeJSON(w, http.StatusOK, enableProxyResponse{
		BridgeID:          id,
		GeneratedAt:       artifact.GeneratedAt,
		DockerComposeYAML: artifact.DockerComposeYAML,
		InstallScript:     artifact.InstallScript,
		ProxyConfigYAML:   artifact.ProxyConfigYAML,
		PluginSecretsJSON: artifact.PluginSecretsJSON,
		// ProxyToken intentionally omitted on re-fetch.
	})
}
