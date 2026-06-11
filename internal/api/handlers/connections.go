package handlers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/events"
	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
)

var (
	errAlreadyResolved = errors.New("connection request already resolved")
	errExpired         = errors.New("connection request expired")
	errForbidden       = errors.New("connection request does not belong to this user")
	errAgentNameTaken  = errors.New("agent name is already in use")
)

const (
	connectionRequestExpiry   = 5 * time.Minute
	connectionTokenWindow     = 5 * time.Minute
	claimCodeTTL              = 5 * time.Minute
	maxPendingRequests        = 10
	pollTimeout               = 30 * time.Second
	maxConcurrentPollsPerUser = 10
)

// ConnectionsHandler manages agent connection request lifecycle.
type ConnectionsHandler struct {
	st          store.Store
	notifier    notify.Notifier
	eventHub    events.EventHub
	logger      *slog.Logger
	baseURL     string
	multiTenant bool

	// Token cache for approved agent tokens. Backed by either in-memory
	// or Redis, depending on server configuration.
	tokenCache TokenCache

	// Claim code cache for the bootstrap-curl flow. In-memory only —
	// codes are 5-minute single-use and don't survive process restart,
	// which is fine for transient bootstrap credentials.
	claimCache ClaimCodeCache

	// Per-user concurrent poll tracking.
	userPollsMu sync.Mutex
	userPolls   map[string]int

	// Per-IP concurrent poll tracking.
	ipPollsMu sync.Mutex
	ipPolls   map[string]int
}

type approvedToken struct {
	raw        string
	approvedAt time.Time
}

func NewConnectionsHandler(st store.Store, notifier notify.Notifier,
	eventHub events.EventHub, logger *slog.Logger, baseURL string, multiTenant bool) *ConnectionsHandler {
	return &ConnectionsHandler{
		st:          st,
		notifier:    notifier,
		eventHub:    eventHub,
		logger:      logger,
		baseURL:     baseURL,
		multiTenant: multiTenant,
		tokenCache:  newMemoryTokenCache(connectionTokenWindow),
		claimCache:  newMemoryClaimCodeCache(),
		userPolls:   make(map[string]int),
		ipPolls:     make(map[string]int),
	}
}

// SetTokenCache overrides the default in-memory token cache.
func (h *ConnectionsHandler) SetTokenCache(tc TokenCache) {
	h.tokenCache = tc
}

// SetClaimCodeCache overrides the default in-memory claim code cache.
func (h *ConnectionsHandler) SetClaimCodeCache(cc ClaimCodeCache) {
	h.claimCache = cc
}

// RequestConnect handles POST /api/agents/connect (unauthenticated).
// An agent calls this to request access to the daemon.
func (h *ConnectionsHandler) RequestConnect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name           string                `json:"name"`
		Description    string                `json:"description"`
		CallbackURL    string                `json:"callback_url"`
		UserID         string                `json:"user_id"`
		InstallContext *store.InstallContext `json:"install_context"`
	}
	// decodeJSON tolerates an empty body so callers can send everything as
	// query params and skip the Content-Type / -d flags entirely.
	if !decodeJSONAllowEmpty(w, r, &body) {
		return
	}
	// Name may also arrive as a query param to keep the bootstrap curl
	// body-less. Body wins if both are set (legacy callers).
	if body.Name == "" {
		body.Name = r.URL.Query().Get("name")
	}
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "name is required")
		return
	}

	// Install context may also arrive as query params so the body-less
	// bootstrap curl can carry it. Body wins if both are set. The dashboard
	// wizard fills these in from the answers the user gave (harness, mode)
	// before clicking past API key, so the resulting agent record knows
	// which harness it came from. See pkg/store.InstallContext for the
	// canonical field list.
	//
	// `body.InstallContext == nil` alone is not enough: a client posting
	// `{"install_context": {}}` decodes to a non-nil pointer at a zero-value
	// struct, which would silently skip the query-param fallback. Treat that
	// case the same as "not provided" so the URL-supplied harness/mode aren't
	// dropped on the floor.
	if body.InstallContext == nil || body.InstallContext.IsEmpty() {
		q := r.URL.Query()
		ic := store.InstallContext{
			Harness:        q.Get("harness"),
			HarnessVersion: q.Get("harness_version"),
			InstallMode:    q.Get("mode"),
			HostOS:         q.Get("host_os"),
			ContainerID:    q.Get("container_id"),
			AuthMode:       q.Get("auth_mode"),
			AliasIntent:    q.Get("alias_intent"),
		}
		if !ic.IsEmpty() {
			body.InstallContext = &ic
		}
	}

	// Bound every install_context field server-side. This endpoint is gated
	// only by claim/user_id, so a hostile (or buggy) harness could otherwise
	// push megabytes into a string field and balloon the connection_requests
	// row (and, on approval, the denormalized copy on the agents row). The
	// caps are generous for legitimate values: harness identifiers and
	// enum-shaped fields are short by design; container IDs are typically
	// 12-64 hex chars and we allow 128.
	if body.InstallContext != nil {
		clampInstallContext(body.InstallContext)
	}

	// Resolve the target user. A `?claim=<code>` query param (minted by an
	// authenticated dashboard session) takes precedence and avoids leaking
	// user_id into the bootstrap curl URL.
	//
	// Claim handling is two-phase: Peek first to identify the user so we
	// can run the cheap validation that follows (name collisions, max
	// pending), then Consume only when we're about to create the request.
	// Burning the single-use code on a 4xx the caller could fix would
	// leave the dashboard renderering a stale claim for up to four minutes
	// before the next mint refetch — too long for a corrected retry.
	// Fallback paths: user_id in the body (legacy callers, skill-based
	// setup flow) or admin@local in single-tenant mode.
	var (
		owner        *store.User
		err          error
		pendingClaim string // non-empty when we owe a Consume after validation
	)
	if claim := r.URL.Query().Get("claim"); claim != "" {
		userID, ok := h.claimCache.Peek(claim)
		if !ok {
			writeError(w, http.StatusUnauthorized, "INVALID_CLAIM", "claim code is invalid, expired, or already consumed")
			return
		}
		owner, err = h.st.GetUserByID(r.Context(), userID)
		if err != nil {
			writeError(w, http.StatusNotFound, "USER_NOT_FOUND", "user not found")
			return
		}
		pendingClaim = claim
	} else if body.UserID != "" {
		owner, err = h.st.GetUserByID(r.Context(), body.UserID)
		if err != nil {
			writeError(w, http.StatusNotFound, "USER_NOT_FOUND", "user not found")
			return
		}
	} else if h.multiTenant {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "user_id or claim is required")
		return
	} else {
		owner, err = h.st.GetUserByEmail(r.Context(), "admin@local")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not resolve daemon owner")
			return
		}
	}

	// Reject duplicate agent names up front so the bootstrap curl never
	// silently clobbers an existing agent. The check runs before any DB
	// write or notification — a name collision must leave the existing
	// agent (and the on-disk JSON for that name on the caller's machine)
	// untouched. We also reject if a *pending* request already exists for
	// the same name; otherwise two concurrent bootstrap curls could both
	// resolve into agents and only the first would be addressable by its
	// chosen name.
	existingAgents, err := h.st.ListAgents(r.Context(), owner.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list agents")
		return
	}
	for _, a := range existingAgents {
		if a.Name == body.Name {
			writeError(w, http.StatusConflict, "AGENT_NAME_EXISTS",
				fmt.Sprintf("agent %q already exists; pick a different name or delete it first", body.Name))
			return
		}
	}
	pendingRequests, err := h.st.ListPendingConnectionRequests(r.Context(), owner.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list pending requests")
		return
	}
	for _, p := range pendingRequests {
		if p.Name == body.Name {
			writeError(w, http.StatusConflict, "AGENT_NAME_EXISTS",
				fmt.Sprintf("a pending request named %q is already waiting; approve or deny it before creating another with the same name", body.Name))
			return
		}
	}

	// Check pending count for this user.
	count, err := h.st.CountPendingConnectionRequestsForUser(r.Context(), owner.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not check pending requests")
		return
	}
	if count >= maxPendingRequests {
		writeError(w, http.StatusTooManyRequests, "TOO_MANY_PENDING", "too many pending connection requests")
		return
	}

	// All validations passed — now atomically consume the claim. A
	// concurrent caller racing on the same code loses here (Consume
	// returns !ok), preserving single-use semantics.
	if pendingClaim != "" {
		if _, ok := h.claimCache.Consume(pendingClaim); !ok {
			writeError(w, http.StatusUnauthorized, "INVALID_CLAIM", "claim code is invalid, expired, or already consumed")
			return
		}
	}

	req := &store.ConnectionRequest{
		UserID:         owner.ID,
		Name:           body.Name,
		Description:    body.Description,
		CallbackURL:    body.CallbackURL,
		Status:         "pending",
		IPAddress:      r.RemoteAddr,
		ExpiresAt:      time.Now().Add(connectionRequestExpiry),
		InstallContext: body.InstallContext,
	}
	if err := h.st.CreateConnectionRequest(r.Context(), req); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create connection request")
		return
	}

	// One-paste install path: the caller proved possession of a dashboard-
	// minted claim code, which IS the user's pre-authorization. Auto-approve
	// immediately so the curl returns a token in one round-trip, without a
	// second click in the dashboard or a Telegram notification. Without a
	// claim (legacy user_id / single-tenant paths) we fall through to the
	// notify-then-wait flow below.
	if pendingClaim != "" {
		agentID, approveErr := h.ApproveByID(r.Context(), req.ID, owner.ID)
		if approveErr != nil {
			h.logger.WarnContext(r.Context(), "lite-proxy: claim auto-approve failed",
				"connection_id", req.ID, "err", approveErr.Error())
			writeError(w, http.StatusInternalServerError, "AUTO_APPROVE_FAILED",
				"connection was created but auto-approval failed; approve it in the dashboard")
			return
		}
		raw, ok := h.tokenCache.Load(req.ID)
		if !ok {
			h.logger.WarnContext(r.Context(), "lite-proxy: auto-approved connection missing token in cache",
				"connection_id", req.ID)
			writeError(w, http.StatusInternalServerError, "TOKEN_UNAVAILABLE",
				"connection was approved but the token cache no longer has it; re-run the bootstrap")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"connection_id": req.ID,
			"agent_id":      agentID,
			"status":        "approved",
			"token":         raw,
			"expires_at":    req.ExpiresAt,
		})
		return
	}

	// Notify owner via SSE and push notification.
	if h.eventHub != nil {
		h.eventHub.Publish(owner.ID, events.Event{Type: "queue"})
	}
	if h.notifier != nil {
		approveURL := fmt.Sprintf("%s/dashboard/agents?action=approve&connection_id=%s", h.baseURL, req.ID)
		denyURL := fmt.Sprintf("%s/dashboard/agents?action=deny&connection_id=%s", h.baseURL, req.ID)
		if msgID, err := h.notifier.SendConnectionRequest(r.Context(), notify.ConnectionRequest{
			ConnectionID: req.ID,
			UserID:       owner.ID,
			AgentName:    body.Name,
			IPAddress:    r.RemoteAddr,
			ApproveURL:   approveURL,
			DenyURL:      denyURL,
		}); err != nil {
			h.logger.WarnContext(r.Context(), "failed to send connection request notification", "err", err)
		} else if msgID != "" {
			_ = h.st.SaveNotificationMessage(r.Context(), "connection", req.ID, "telegram", msgID)
		}
	}

	// If wait=true, long-poll until the connection request is resolved.
	// The status code distinguishes outcomes so a `curl -sf` bootstrap
	// exits non-zero on anything other than approval — that way
	// --remove-on-error cleans up the tokenless response body and the
	// caller never ends up with garbage on disk.
	if r.URL.Query().Get("wait") == "true" && h.eventHub != nil {
		resolved := h.waitForConnectionResolution(r.Context(), req.ID, owner.ID, longPollDeadline(r))
		if r.Context().Err() != nil {
			return
		}
		resp := map[string]any{
			"connection_id": req.ID,
			"status":        resolved.Status,
			"expires_at":    resolved.ExpiresAt,
		}
		// finalStatus stamps the response and returns the right HTTP code.
		// Hoisted into a closure because the timeout branch loops back
		// through it after re-reading state on a lost race.
		writeFinal := func(fresh string, expiresAt any) {
			resp["status"] = fresh
			if expiresAt != nil {
				resp["expires_at"] = expiresAt
			}
			switch fresh {
			case "approved":
				raw, ok := h.tokenCache.Load(req.ID)
				if !ok {
					// The approve handler wrote the token to the cache;
					// if it's gone by the time we read it, returning 201
					// without a token field would write garbage to the
					// caller's disk. Surface as 500 so --remove-on-error
					// cleans up.
					h.logger.WarnContext(r.Context(), "lite-proxy: approved request missing token in cache",
						"connection_id", req.ID)
					writeError(w, http.StatusInternalServerError, "TOKEN_UNAVAILABLE",
						"connection was approved but the token cache no longer has it; ask the user to re-approve")
					return
				}
				resp["token"] = raw
				writeJSON(w, http.StatusCreated, resp)
			case "denied":
				writeJSON(w, http.StatusForbidden, resp)
			case "expired":
				writeJSON(w, http.StatusGone, resp)
			default:
				writeJSON(w, http.StatusRequestTimeout, resp)
			}
		}
		switch resolved.Status {
		case "approved", "denied", "expired":
			writeFinal(resolved.Status, resolved.ExpiresAt)
		default:
			// "pending" reaching the wait deadline is the long-poll
			// equivalent of a timeout. Conditionally expire so a late
			// Approve that raced into the window isn't clobbered — the
			// store method gates on WHERE status='pending'. If we lose
			// the race (modified=false), re-read and respond with
			// whatever the real terminal state is.
			modified, expireErr := h.expireByID(r.Context(), req.ID, owner.ID)
			switch {
			case expireErr != nil:
				writeFinal("pending", resolved.ExpiresAt)
			case modified:
				writeFinal("expired", resolved.ExpiresAt)
			default:
				fresh, fetchErr := h.st.GetConnectionRequest(r.Context(), req.ID)
				if fetchErr != nil {
					writeFinal("pending", resolved.ExpiresAt)
				} else {
					writeFinal(fresh.Status, fresh.ExpiresAt)
				}
			}
		}
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"connection_id": req.ID,
		"status":        req.Status,
		"poll_url":      "/api/agents/connect/" + req.ID + "/status",
		"expires_at":    req.ExpiresAt,
	})
}

// clampInstallContext truncates every string field on the install context
// to its per-field cap. Mutates in place. The caller passes a non-nil
// pointer.
//
// The caps are deliberately generous for legitimate values — enum-shaped
// fields like `harness`, `install_mode`, `host_os`, `auth_mode`,
// `alias_intent` are typically <16 chars; `harness_version` is semver-ish;
// `container_id` is usually 12-64 hex chars. Anything past the cap is the
// caller misusing the field; chop instead of rejecting so a single noisy
// field doesn't kill an otherwise valid connect request.
func clampInstallContext(ic *store.InstallContext) {
	const (
		shortCap     = 64
		versionCap   = 64
		containerCap = 128
	)
	ic.Harness = clampString(ic.Harness, shortCap)
	ic.HarnessVersion = clampString(ic.HarnessVersion, versionCap)
	ic.InstallMode = clampString(ic.InstallMode, shortCap)
	ic.HostOS = clampString(ic.HostOS, shortCap)
	ic.ContainerID = clampString(ic.ContainerID, containerCap)
	ic.AuthMode = clampString(ic.AuthMode, shortCap)
	ic.AliasIntent = clampString(ic.AliasIntent, shortCap)
}

func clampString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// MintClaim handles POST /api/agents/connect/claim (user JWT). It mints a
// short-lived single-use claim code that the dashboard embeds in the
// bootstrap curl URL as `?claim=…`. The unauthenticated RequestConnect
// endpoint consumes the claim to attribute the request to the minting
// user without that user's ID ever appearing in the URL.
//
// The code is 10 URL-safe base64 characters (60 bits of entropy from
// 8 random bytes, truncated). 5-minute single-use codes don't need
// long-term unguessability and a tight URL is easier on the eyes.
func (h *ConnectionsHandler) MintClaim(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not generate claim code")
		return
	}
	code := base64.RawURLEncoding.EncodeToString(b)[:10]
	if err := h.claimCache.Store(code, user.ID, claimCodeTTL); err != nil {
		// If the backend (Redis, typically) rejected the write, returning
		// a 201 with the code would hand the user a credential that
		// doesn't exist anywhere — every bootstrap curl using it would
		// immediately INVALID_CLAIM. Surface the failure instead.
		h.logger.WarnContext(r.Context(), "lite-proxy: claim cache store failed",
			"err", err.Error(), "user_id", user.ID)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not persist claim code")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"code":       code,
		"expires_at": time.Now().Add(claimCodeTTL),
	})
}

// PollStatus handles GET /api/agents/connect/{id}/status (unauthenticated).
// Long-polls until the connection request is resolved or timeout.
func (h *ConnectionsHandler) PollStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Check and return current status. If not pending, return immediately.
	respond := func() (done bool) {
		cr, err := h.st.GetConnectionRequest(r.Context(), id)
		if err != nil {
			if err == store.ErrNotFound {
				writeError(w, http.StatusNotFound, "NOT_FOUND", "connection request not found")
			} else {
				writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get connection request")
			}
			return true
		}

		// Check expiry. On a lost race (another writer flipped the row to
		// approved/denied in the window) re-read so we hand the caller
		// the actual terminal state rather than asserting "expired".
		if cr.Status == "pending" && time.Now().After(cr.ExpiresAt) {
			modified, err := h.expireByID(r.Context(), id, cr.UserID)
			if err == nil {
				if modified {
					cr.Status = "expired"
				} else if fresh, fetchErr := h.st.GetConnectionRequest(r.Context(), id); fetchErr == nil {
					cr = fresh
				}
			}
		}

		if cr.Status == "pending" {
			return false
		}

		resp := map[string]any{"status": cr.Status}
		if cr.Status == "approved" {
			if raw, ok := h.tokenCache.Load(id); ok {
				resp["token"] = raw
			}
		}
		writeJSON(w, http.StatusOK, resp)
		return true
	}

	// First check — return immediately if resolved.
	if respond() {
		return
	}

	// Per-IP concurrent poll limit (max 3).
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip == "" {
		ip = r.RemoteAddr
	}
	h.ipPollsMu.Lock()
	if h.ipPolls[ip] >= 3 {
		h.ipPollsMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}
	h.ipPolls[ip]++
	h.ipPollsMu.Unlock()
	defer func() {
		h.ipPollsMu.Lock()
		h.ipPolls[ip]--
		if h.ipPolls[ip] <= 0 {
			delete(h.ipPolls, ip)
		}
		h.ipPollsMu.Unlock()
	}()

	// Look up the connection request to get the owner's user ID for SSE subscription.
	cr, err := h.st.GetConnectionRequest(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}

	// Per-user concurrent poll limit. Degrade to immediate "pending" if exceeded
	// so a single user cannot saturate the instance.
	h.userPollsMu.Lock()
	if h.userPolls[cr.UserID] >= maxConcurrentPollsPerUser {
		h.userPollsMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}
	h.userPolls[cr.UserID]++
	h.userPollsMu.Unlock()
	defer func() {
		h.userPollsMu.Lock()
		h.userPolls[cr.UserID]--
		if h.userPolls[cr.UserID] <= 0 {
			delete(h.userPolls, cr.UserID)
		}
		h.userPollsMu.Unlock()
	}()

	if h.eventHub == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}
	ch, unsub := h.eventHub.Subscribe(cr.UserID)
	defer unsub()

	timer := time.NewTimer(pollTimeout)
	defer timer.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			// Timeout — return current status, or pending if still unresolved.
			if !respond() {
				writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
			}
			return
		case _, ok := <-ch:
			if !ok {
				respond()
				return
			}
			if respond() {
				return
			}
		}
	}
}

// waitForConnectionResolution long-polls until the connection request leaves
// the "pending" state or the timeout expires.
func (h *ConnectionsHandler) waitForConnectionResolution(ctx context.Context, connID, userID string, timeout time.Duration) *store.ConnectionRequest {
	return events.WaitFor(ctx, h.eventHub, userID, timeout,
		nil, // any event type
		func(c context.Context, _ *events.Event) (*store.ConnectionRequest, bool) {
			cr, err := h.st.GetConnectionRequest(c, connID)
			if err != nil {
				return &store.ConnectionRequest{ID: connID, Status: "pending"}, false
			}
			if cr.Status == "pending" && time.Now().After(cr.ExpiresAt) {
				if modified, expireErr := h.expireByID(c, connID, cr.UserID); expireErr == nil {
					if modified {
						cr.Status = "expired"
					} else if fresh, fetchErr := h.st.GetConnectionRequest(c, connID); fetchErr == nil {
						cr = fresh
					}
				}
			}
			return cr, cr.Status != "pending"
		},
	)
}

// Approve handles POST /api/agents/connect/{id}/approve (user JWT).
func (h *ConnectionsHandler) Approve(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	id := r.PathValue("id")
	agentID, err := h.ApproveByID(r.Context(), id, user.ID)
	if err != nil {
		switch err {
		case store.ErrNotFound:
			writeError(w, http.StatusNotFound, "NOT_FOUND", "connection request not found")
		case errForbidden:
			writeError(w, http.StatusForbidden, "FORBIDDEN", "not your connection request")
		case errAlreadyResolved:
			writeError(w, http.StatusConflict, "ALREADY_RESOLVED", "connection request is not pending")
		case errExpired:
			writeError(w, http.StatusGone, "EXPIRED", "connection request has expired")
		case errAgentNameTaken:
			writeError(w, http.StatusConflict, "AGENT_NAME_EXISTS",
				"an agent with this name already exists; deny this request and bootstrap with a different name")
		default:
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not approve connection request")
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "approved",
		"agent_id": agentID,
	})
}

// ApproveByID is the core approve logic, callable from HTTP handlers and
// the notifier decision consumer.
func (h *ConnectionsHandler) ApproveByID(ctx context.Context, id, userID string) (agentID string, err error) {
	cr, err := h.st.GetConnectionRequest(ctx, id)
	if err != nil {
		return "", err
	}
	if cr.UserID != userID {
		return "", errForbidden
	}
	if cr.Status != "pending" {
		return "", errAlreadyResolved
	}
	if time.Now().After(cr.ExpiresAt) {
		_, _ = h.expireByID(ctx, id, userID)
		return "", errExpired
	}

	// Re-check name uniqueness at approve time. The request-creation guard
	// runs much earlier; between then and now a second agent with the same
	// name could have been created (concurrent approve of another pending
	// request, an Add Agent form submission, etc.). Without this re-check
	// the duplicate guarantee leaks. The store has no unique index on
	// (user_id, name) so we enforce it in code.
	existing, listErr := h.st.ListAgents(ctx, userID)
	if listErr != nil {
		return "", fmt.Errorf("list agents: %w", listErr)
	}
	for _, a := range existing {
		if a.Name == cr.Name {
			return "", errAgentNameTaken
		}
	}

	rawToken, err := auth.GenerateAgentToken()
	if err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}

	agent, err := h.st.CreateAgent(ctx, userID, cr.Name, auth.HashToken(rawToken))
	if err != nil {
		return "", fmt.Errorf("create agent: %w", err)
	}
	if cr.Description != "" {
		if err := h.st.UpdateAgentDescription(ctx, agent.ID, userID, cr.Description); err != nil {
			return "", fmt.Errorf("save agent description: %w", err)
		}
	}
	// Denormalize the install context onto the agent so the dashboard can
	// still tell "this is an OpenClaw install" after the connection request
	// drops out of the pending list. Best-effort: nil install_context means
	// nothing to copy (legacy or unenriched flows), an Update failure on a
	// fresh agent is harmless metadata loss.
	if cr.InstallContext != nil {
		if err := h.st.SetAgentInstallContext(ctx, agent.ID, cr.InstallContext); err != nil {
			h.logger.WarnContext(ctx, "approve: failed to copy install_context to agent",
				"err", err.Error(), "agent_id", agent.ID, "connection_id", cr.ID)
		}
	}

	if err := h.st.UpdateConnectionRequestStatus(ctx, id, "approved", agent.ID); err != nil {
		return "", fmt.Errorf("update status: %w", err)
	}

	h.tokenCache.Store(id, rawToken)
	h.decrementNotifierPolling(userID)
	h.updateNotificationMsg(ctx, id, userID, "✅ <b>Approved</b> — agent connected.")

	if h.eventHub != nil {
		h.eventHub.Publish(userID, events.Event{Type: "queue"})
	}

	return agent.ID, nil
}

// Deny handles POST /api/agents/connect/{id}/deny (user JWT).
func (h *ConnectionsHandler) Deny(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	id := r.PathValue("id")
	if err := h.DenyByID(r.Context(), id, user.ID); err != nil {
		switch err {
		case store.ErrNotFound:
			writeError(w, http.StatusNotFound, "NOT_FOUND", "connection request not found")
		case errForbidden:
			writeError(w, http.StatusForbidden, "FORBIDDEN", "not your connection request")
		case errAlreadyResolved:
			writeError(w, http.StatusConflict, "ALREADY_RESOLVED", "connection request is not pending")
		default:
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not deny connection request")
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "denied"})
}

// DenyByID is the core deny logic, callable from HTTP handlers and
// the notifier decision consumer.
func (h *ConnectionsHandler) DenyByID(ctx context.Context, id, userID string) error {
	cr, err := h.st.GetConnectionRequest(ctx, id)
	if err != nil {
		return err
	}
	if cr.UserID != userID {
		return errForbidden
	}
	if cr.Status != "pending" {
		return errAlreadyResolved
	}

	if err := h.st.UpdateConnectionRequestStatus(ctx, id, "denied", ""); err != nil {
		return fmt.Errorf("update status: %w", err)
	}

	h.decrementNotifierPolling(userID)
	h.updateNotificationMsg(ctx, id, userID, "❌ <b>Denied</b> — connection rejected.")

	if h.eventHub != nil {
		h.eventHub.Publish(userID, events.Event{Type: "queue"})
	}
	return nil
}

// expireByID transitions a pending connection request to "expired" only if
// it's still pending. Returns (modified, err): modified=true means the
// row was flipped to expired; modified=false means another writer (Approve
// or Deny) beat us to the row and the caller must re-read state instead of
// assuming the request is gone. Without this guard a timed-out long-poll
// could clobber an approval that landed in the race window, orphaning the
// agent the approval created.
func (h *ConnectionsHandler) expireByID(ctx context.Context, id, userID string) (bool, error) {
	modified, err := h.st.UpdateConnectionRequestStatusIfPending(ctx, id, "expired")
	if err != nil {
		return false, err
	}
	if !modified {
		return false, nil
	}
	h.decrementNotifierPolling(userID)
	h.updateNotificationMsg(ctx, id, userID, "⏰ <b>Expired</b> — connection request timed out.")
	if h.eventHub != nil {
		h.eventHub.Publish(userID, events.Event{Type: "queue"})
	}
	return true, nil
}

func (h *ConnectionsHandler) decrementNotifierPolling(userID string) {
	if h.notifier == nil {
		return
	}
	if pd, ok := h.notifier.(notify.PollingDecrementer); ok {
		pd.DecrementPolling(userID)
	}
}

func (h *ConnectionsHandler) updateNotificationMsg(ctx context.Context, targetID, userID, text string) {
	if h.notifier == nil {
		return
	}
	msgID, err := h.st.GetNotificationMessage(ctx, "connection", targetID, "telegram")
	if err != nil {
		return
	}
	if err := h.notifier.UpdateMessage(ctx, userID, msgID, text); err != nil {
		h.logger.WarnContext(ctx, "telegram message update failed", "err", err, "target_type", "connection", "target_id", targetID)
	}
}

// List handles GET /api/agents/connections (user JWT).
func (h *ConnectionsHandler) List(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	requests, err := h.st.ListPendingConnectionRequests(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list connection requests")
		return
	}
	if requests == nil {
		requests = []*store.ConnectionRequest{}
	}
	writeJSON(w, http.StatusOK, requests)
}
