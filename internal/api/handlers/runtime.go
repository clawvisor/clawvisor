package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	runtimeautovault "github.com/clawvisor/clawvisor/internal/runtime/autovault"
	runtimepolicy "github.com/clawvisor/clawvisor/internal/runtime/policy"
	runtimeproxy "github.com/clawvisor/clawvisor/internal/runtime/proxy"
	runtimereview "github.com/clawvisor/clawvisor/internal/runtime/review"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
	"github.com/google/uuid"
)

type RuntimeManager interface {
	CreateRuntimeSession(ctx context.Context, agentID, userID string, req runtimeproxy.CreateSessionRequest) (*runtimeproxy.CreateSessionResult, error)
	ListRuntimeSessionsForUser(ctx context.Context, userID string) ([]*store.RuntimeSession, error)
	RevokeRuntimeSession(ctx context.Context, sessionID string) error
	ProxyURL() string
	CACertPEM() string
}

type RuntimeHandler struct {
	st          store.Store
	manager     RuntimeManager
	cfg         *config.Config
	vault       vault.Vault
	reviewCache runtimereview.HeldApprovalCache
}

func NewRuntimeHandler(st store.Store, v vault.Vault, manager RuntimeManager, cfg *config.Config, reviewCache runtimereview.HeldApprovalCache) *RuntimeHandler {
	return &RuntimeHandler{st: st, vault: v, manager: manager, cfg: cfg, reviewCache: reviewCache}
}

func (h *RuntimeHandler) CreateSession(w http.ResponseWriter, r *http.Request) {
	agent := middleware.AgentFromContext(r.Context())
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	if h.manager == nil || h.cfg == nil || !h.cfg.RuntimeProxy.Enabled {
		writeError(w, http.StatusConflict, "RUNTIME_PROXY_DISABLED", "runtime proxy is not enabled")
		return
	}
	var req struct {
		Mode            string         `json:"mode"`
		ObservationMode *bool          `json:"observation_mode,omitempty"`
		TTLSeconds      int            `json:"ttl_seconds,omitempty"`
		Metadata        map[string]any `json:"metadata,omitempty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	settings := defaultAgentRuntimeSettings(h.cfg, agent.ID)
	if agent.RuntimeSettings != nil {
		settings = agent.RuntimeSettings
	}
	if req.ObservationMode == nil {
		observe := strings.EqualFold(settings.RuntimeMode, "observe")
		req.ObservationMode = &observe
	}
	req.Metadata = mergeRuntimeSessionMetadata(req.Metadata, *settings)
	result, err := h.manager.CreateRuntimeSession(r.Context(), agent.ID, agent.UserID, runtimeproxy.CreateSessionRequest{
		Mode:            req.Mode,
		ObservationMode: req.ObservationMode,
		TTLSeconds:      req.TTLSeconds,
		Metadata:        req.Metadata,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create runtime session")
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (h *RuntimeHandler) CreatePlaceholder(w http.ResponseWriter, r *http.Request) {
	agent := middleware.AgentFromContext(r.Context())
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	if h.vault == nil {
		writeError(w, http.StatusConflict, "RUNTIME_PLACEHOLDERS_DISABLED", "runtime placeholder vault is not configured")
		return
	}
	var req struct {
		Service string `json:"service"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Service == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "service is required")
		return
	}
	if _, err := h.vault.Get(r.Context(), agent.UserID, req.Service); err != nil {
		if errors.Is(err, vault.ErrNotFound) {
			writeError(w, http.StatusNotFound, "SERVICE_NOT_ACTIVATED", "service credential is not activated")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load service credential")
		return
	}
	placeholder, err := runtimeautovault.GeneratePlaceholder(runtimeautovault.PlaceholderPrefix(req.Service))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not mint runtime placeholder")
		return
	}
	if err := h.st.CreateRuntimePlaceholder(r.Context(), &store.RuntimePlaceholder{
		Placeholder: placeholder,
		UserID:      agent.UserID,
		AgentID:     agent.ID,
		ServiceID:   req.Service,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not save runtime placeholder")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"placeholder": placeholder,
		"service":     req.Service,
	})
}

func (h *RuntimeHandler) ListUserPlaceholders(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	entries, err := h.st.ListRuntimePlaceholders(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list shadow tokens")
		return
	}
	if entries == nil {
		entries = []*store.RuntimePlaceholder{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"total":   len(entries),
	})
}

func (h *RuntimeHandler) CreateUserPlaceholder(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	if h.vault == nil {
		writeError(w, http.StatusConflict, "RUNTIME_PLACEHOLDERS_DISABLED", "runtime placeholder vault is not configured")
		return
	}
	var req struct {
		AgentID string `json:"agent_id"`
		Service string `json:"service"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.AgentID = strings.TrimSpace(req.AgentID)
	req.Service = strings.TrimSpace(req.Service)
	if req.AgentID == "" || req.Service == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "agent_id and service are required")
		return
	}
	agents, err := h.st.ListAgents(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load agents")
		return
	}
	var agent *store.Agent
	for _, candidate := range agents {
		if candidate.ID == req.AgentID {
			agent = candidate
			break
		}
	}
	if agent == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "agent not found")
		return
	}
	if _, err := h.vault.Get(r.Context(), user.ID, req.Service); err != nil {
		if errors.Is(err, vault.ErrNotFound) {
			writeError(w, http.StatusNotFound, "SERVICE_NOT_ACTIVATED", "service credential is not activated")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load service credential")
		return
	}
	placeholder, err := runtimeautovault.GeneratePlaceholder(runtimeautovault.PlaceholderPrefix(req.Service))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not mint runtime placeholder")
		return
	}
	entry := &store.RuntimePlaceholder{
		Placeholder: placeholder,
		UserID:      user.ID,
		AgentID:     agent.ID,
		ServiceID:   req.Service,
	}
	if err := h.st.CreateRuntimePlaceholder(r.Context(), entry); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not save runtime placeholder")
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

func (h *RuntimeHandler) DeleteUserPlaceholder(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	placeholder := r.PathValue("placeholder")
	if strings.TrimSpace(placeholder) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "placeholder is required")
		return
	}
	if err := h.st.DeleteRuntimePlaceholder(r.Context(), placeholder, user.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "shadow token not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not revoke shadow token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"placeholder": placeholder,
		"status":      "revoked",
	})
}

func (h *RuntimeHandler) ListSessions(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	sessions, err := h.manager.ListRuntimeSessionsForUser(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list runtime sessions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": sessions,
		"total":   len(sessions),
	})
}

func (h *RuntimeHandler) RevokeSession(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	sessionID := r.PathValue("id")
	session, err := h.st.GetRuntimeSession(r.Context(), sessionID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "runtime session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get runtime session")
		return
	}
	if session.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your runtime session")
		return
	}
	if err := h.manager.RevokeRuntimeSession(r.Context(), sessionID); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not revoke runtime session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sessionID,
		"status":     "revoked",
	})
}

func (h *RuntimeHandler) Status(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	_ = user
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":                  h.cfg != nil && h.cfg.RuntimeProxy.Enabled,
		"proxy_url":                h.manager.ProxyURL(),
		"observation_mode_default": h.cfg != nil && h.cfg.RuntimePolicy.ObservationModeDefault,
		"inline_approval_enabled":  h.cfg != nil && h.cfg.RuntimePolicy.InlineApprovalEnabled,
		"tool_lease_timeout_seconds": func() int {
			if h.cfg == nil {
				return 0
			}
			return h.cfg.RuntimePolicy.ToolLeaseTimeoutSeconds
		}(),
		"one_off_ttl_seconds": func() int {
			if h.cfg == nil {
				return 0
			}
			return h.cfg.RuntimePolicy.OneOffTTLSeconds
		}(),
		"autovault_mode": func() string {
			if h.cfg == nil {
				return ""
			}
			return h.cfg.RuntimePolicy.AutovaultMode
		}(),
		"inject_stored_bearer": h.cfg != nil && h.cfg.RuntimePolicy.InjectStoredBearer,
		"ca_cert_pem":          h.manager.CACertPEM(),
		"starter_profiles":     runtimepolicy.StarterProfiles(),
	})
}

func (h *RuntimeHandler) ListApprovals(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	records, err := h.st.ListPendingApprovalRecords(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list runtime approvals")
		return
	}
	sessionByID, err := h.runtimeSessionsByID(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not inspect runtime approval sessions")
		return
	}
	var filtered []*store.ApprovalRecord
	now := time.Now().UTC()
	for _, rec := range records {
		if rec.SessionID == nil || *rec.SessionID == "" {
			filtered = append(filtered, rec)
			continue
		}
		session := sessionByID[*rec.SessionID]
		if session == nil || session.RevokedAt != nil || session.ExpiresAt.Before(now) {
			continue
		}
		filtered = append(filtered, rec)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": filtered,
		"total":   len(filtered),
	})
}

func (h *RuntimeHandler) runtimeSessionsByID(ctx context.Context, userID string) (map[string]*store.RuntimeSession, error) {
	out := map[string]*store.RuntimeSession{}
	if h.manager != nil {
		sessions, err := h.manager.ListRuntimeSessionsForUser(ctx, userID)
		if err != nil {
			return nil, err
		}
		for _, session := range sessions {
			if session != nil {
				out[session.ID] = session
			}
		}
		return out, nil
	}
	agents, err := h.st.ListAgents(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, agent := range agents {
		sessions, err := h.st.ListRuntimeSessionsByAgent(ctx, agent.ID)
		if err != nil {
			return nil, err
		}
		for _, session := range sessions {
			if session != nil {
				out[session.ID] = session
			}
		}
	}
	return out, nil
}

func (h *RuntimeHandler) ListEvents(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	filter := store.RuntimeEventFilter{
		SessionID: r.URL.Query().Get("session_id"),
		Limit:     200,
	}
	events, err := h.st.ListRuntimeEvents(r.Context(), user.ID, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list runtime events")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": events,
		"total":   len(events),
	})
}

func (h *RuntimeHandler) ResolveApproval(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	approvalID := r.PathValue("id")
	rec, err := h.st.GetApprovalRecord(r.Context(), approvalID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "runtime approval not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get runtime approval")
		return
	}
	if rec.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your runtime approval")
		return
	}
	var req struct {
		Resolution string `json:"resolution"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Resolution != "allow_once" && req.Resolution != "allow_session" && req.Resolution != "allow_always" && req.Resolution != "deny" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "resolution must be allow_once, allow_session, allow_always, or deny")
		return
	}
	status := "approved"
	if req.Resolution == "deny" {
		status = "denied"
	}
	if err := validateApprovalRecordTransition(rec, req.Resolution, status); err != nil {
		writeError(w, http.StatusConflict, "INVALID_APPROVAL_TRANSITION", err.Error())
		return
	}
	var promotedTask *store.Task
	switch req.Resolution {
	case "allow_once":
		switch {
		case rec.Kind == "credential_review":
			if err := h.createCredentialAuthorization(r.Context(), rec, "once"); err != nil {
				writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
				return
			}
		case rec.ResolutionTransport == "consume_one_off_retry":
			if err := h.createRuntimeOneOffApproval(r.Context(), rec); err != nil {
				writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
				return
			}
		}
	case "allow_session", "allow_always":
		if rec.Kind == "credential_review" {
			scope := "session"
			if req.Resolution == "allow_always" {
				scope = "standing"
			}
			if err := h.createCredentialAuthorization(r.Context(), rec, scope); err != nil {
				writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
				return
			}
		} else {
			lifetime := "session"
			if req.Resolution == "allow_always" {
				lifetime = "standing"
			}
			promotedTask, err = h.promoteRuntimeApprovalToTask(r.Context(), rec, lifetime)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
				return
			}
		}
	}
	if err := h.st.ResolveApprovalRecord(r.Context(), rec.ID, req.Resolution, status, time.Now().UTC()); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not resolve runtime approval")
		return
	}
	if rec.RequestID != nil && strings.TrimSpace(*rec.RequestID) != "" {
		if err := h.st.ClearApprovalRecordRequestID(r.Context(), rec.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not finalize runtime approval")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"approval_id": rec.ID,
		"status":      status,
		"resolution":  req.Resolution,
		"task_id": func() string {
			if promotedTask == nil {
				return ""
			}
			return promotedTask.ID
		}(),
	})
}

func (h *RuntimeHandler) createCredentialAuthorization(ctx context.Context, rec *store.ApprovalRecord, scope string) error {
	var payload runtimeproxy.RuntimeCredentialReviewPayload
	if err := json.Unmarshal(rec.PayloadJSON, &payload); err != nil {
		return fmt.Errorf("could not parse runtime credential approval payload")
	}
	if rec.AgentID == nil || *rec.AgentID == "" {
		return fmt.Errorf("runtime credential approval missing agent context")
	}
	auth := &store.CredentialAuthorization{
		ID:            uuid.NewSHA1(uuid.NameSpaceURL, []byte("credential-approval:"+rec.ID+":"+scope)).String(),
		ApprovalID:    &rec.ID,
		UserID:        rec.UserID,
		AgentID:       *rec.AgentID,
		Scope:         scope,
		CredentialRef: payload.CredentialRef,
		Service:       payload.Service,
		Host:          payload.Host,
		HeaderName:    payload.HeaderName,
		Scheme:        payload.Scheme,
		Status:        "active",
		MetadataJSON:  mustJSON(map[string]any{"detector": payload.Detector, "source": "runtime_approval"}),
	}
	switch scope {
	case "once":
		if payload.SessionID == "" {
			return fmt.Errorf("runtime credential approval missing session context")
		}
		auth.SessionID = &payload.SessionID
		expiresAt := time.Now().UTC().Add(time.Duration(h.oneOffTTLSeconds()) * time.Second)
		auth.ExpiresAt = &expiresAt
	case "session":
		if payload.SessionID == "" {
			return fmt.Errorf("runtime credential approval missing session context")
		}
		auth.SessionID = &payload.SessionID
	}
	if err := h.st.CreateCredentialAuthorization(ctx, auth); err != nil {
		if !errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("could not create runtime credential authorization")
		}
		existing, getErr := h.st.GetCredentialAuthorization(ctx, auth.ID)
		if getErr != nil {
			return fmt.Errorf("could not create runtime credential authorization")
		}
		auth = existing
	}
	_ = h.st.CreateRuntimeEvent(ctx, &store.RuntimeEvent{
		Timestamp:           time.Now().UTC(),
		SessionID:           payload.SessionID,
		UserID:              rec.UserID,
		AgentID:             auth.AgentID,
		EventType:           "runtime.autovault.authorization_created",
		ActionKind:          "egress",
		ApprovalID:          &rec.ID,
		ResolutionTransport: nullableStr(rec.ResolutionTransport),
		Decision:            nullableStr("allow"),
		Outcome:             nullableStr("created"),
		Reason:              nullableStr("runtime credential authorization created"),
		MetadataJSON: mustJSON(map[string]any{
			"scope":          auth.Scope,
			"host":           auth.Host,
			"header_name":    auth.HeaderName,
			"scheme":         auth.Scheme,
			"service_guess":  auth.Service,
			"credential_ref": auth.CredentialRef,
		}),
	})
	return nil
}

func (h *RuntimeHandler) createRuntimeOneOffApproval(ctx context.Context, rec *store.ApprovalRecord) error {
	var payload runtimeproxy.RuntimeApprovalPayload
	if err := json.Unmarshal(rec.PayloadJSON, &payload); err != nil {
		return fmt.Errorf("could not parse runtime approval payload")
	}
	if err := h.st.CreateOneOffApproval(ctx, &store.OneOffApproval{
		SessionID:          payload.SessionID,
		RequestFingerprint: payload.RequestFingerprint,
		ApprovalID:         &rec.ID,
		ApprovedAt:         time.Now().UTC(),
		ExpiresAt:          time.Now().UTC().Add(time.Duration(h.oneOffTTLSeconds()) * time.Second),
	}); err != nil {
		return fmt.Errorf("could not create one-off approval")
	}
	metadataJSON, _ := json.Marshal(map[string]any{"host": payload.Host, "method": payload.Method, "path": payload.Path})
	_ = h.st.CreateRuntimeEvent(ctx, &store.RuntimeEvent{
		Timestamp:           time.Now().UTC(),
		SessionID:           payload.SessionID,
		UserID:              rec.UserID,
		AgentID:             payload.AgentID,
		EventType:           "runtime.egress.one_off_created",
		ActionKind:          "egress",
		ApprovalID:          &rec.ID,
		RequestFingerprint:  nullableStr(payload.RequestFingerprint),
		ResolutionTransport: nullableStr(rec.ResolutionTransport),
		Decision:            nullableStr("allow"),
		Outcome:             nullableStr("created"),
		Reason:              nullableStr("runtime one-off retry authorization created"),
		MetadataJSON:        metadataJSON,
	})
	return nil
}

func (h *RuntimeHandler) promoteRuntimeApprovalToTask(ctx context.Context, rec *store.ApprovalRecord, lifetime string) (*store.Task, error) {
	task, sessionID, agentID, err := h.buildRuntimeTaskFromApproval(rec, lifetime)
	if err != nil {
		return nil, err
	}
	if err := h.st.CreateTask(ctx, task); err != nil {
		if errors.Is(err, store.ErrConflict) {
			existing, getErr := h.st.GetTask(ctx, task.ID)
			if getErr == nil {
				task = existing
			} else {
				return nil, fmt.Errorf("could not load promoted runtime task")
			}
		} else {
			return nil, fmt.Errorf("could not create promoted runtime task")
		}
	}
	if sessionID != "" {
		now := time.Now().UTC()
		metadataJSON, _ := json.Marshal(map[string]any{"approval_id": rec.ID, "resolution_transport": rec.ResolutionTransport})
		_ = h.st.UpsertActiveTaskSession(ctx, &store.ActiveTaskSession{
			TaskID:       task.ID,
			SessionID:    sessionID,
			UserID:       rec.UserID,
			AgentID:      agentID,
			MetadataJSON: metadataJSON,
			StartedAt:    now,
			LastSeenAt:   now,
			Status:       "active",
		})
	}
	if h.reviewCache != nil && sessionID != "" {
		_ = h.reviewCache.RebindTask(sessionID, rec.ID, task.ID)
	}
	metadataJSON, _ := json.Marshal(map[string]any{"lifetime": lifetime, "approval_kind": rec.Kind})
	_ = h.st.CreateRuntimeEvent(ctx, &store.RuntimeEvent{
		Timestamp:           time.Now().UTC(),
		SessionID:           sessionID,
		UserID:              rec.UserID,
		AgentID:             agentID,
		EventType:           "runtime.task.promoted",
		ActionKind:          "task",
		ApprovalID:          &rec.ID,
		TaskID:              &task.ID,
		MatchedTaskID:       &task.ID,
		ResolutionTransport: nullableStr(rec.ResolutionTransport),
		Decision:            nullableStr("allow"),
		Outcome:             nullableStr("promoted"),
		Reason:              nullableStr("runtime approval promoted into task scope"),
		MetadataJSON:        metadataJSON,
	})
	return task, nil
}

func (h *RuntimeHandler) buildRuntimeTaskFromApproval(rec *store.ApprovalRecord, lifetime string) (*store.Task, string, string, error) {
	approvedAt := time.Now().UTC()
	task := &store.Task{
		ID:             uuid.NewSHA1(uuid.NameSpaceURL, []byte("runtime-approval:"+rec.ID+":"+lifetime)).String(),
		UserID:         rec.UserID,
		Lifetime:       lifetime,
		Status:         "active",
		SchemaVersion:  2,
		ApprovedAt:     &approvedAt,
		ApprovalSource: "manual",
	}
	if rec.AgentID != nil {
		task.AgentID = *rec.AgentID
	}
	sessionID := ""
	if rec.SessionID != nil {
		sessionID = *rec.SessionID
	}
	if lifetime == "session" {
		expiresIn := h.taskExpirySeconds()
		task.ExpiresInSeconds = expiresIn
		expiresAt := approvedAt.Add(time.Duration(expiresIn) * time.Second)
		task.ExpiresAt = &expiresAt
	}
	switch rec.ResolutionTransport {
	case "consume_one_off_retry":
		var payload runtimeproxy.RuntimeApprovalPayload
		if err := json.Unmarshal(rec.PayloadJSON, &payload); err != nil {
			return nil, "", "", fmt.Errorf("could not parse runtime approval payload")
		}
		if payload.AgentID != "" {
			task.AgentID = payload.AgentID
		}
		if payload.SessionID != "" {
			sessionID = payload.SessionID
		}
		task.Purpose = firstRuntimeTaskPurpose(payload.Reason, fmt.Sprintf("Runtime egress to %s%s", payload.Host, payload.Path))
		task.ExpectedUse = firstRuntimeTaskPurpose(payload.Reason, fmt.Sprintf("%s %s%s", payload.Method, payload.Host, payload.Path))
		expectedEgress, _ := json.Marshal([]runtimetasks.ExpectedEgress{{
			Host:       payload.Host,
			Method:     payload.Method,
			Path:       payload.Path,
			QueryShape: requiredKeyShape(payload.Query),
			BodyShape:  requiredKeyShape(payload.Body),
			Why:        firstRuntimeTaskPurpose(payload.Reason, "Runtime-reviewed egress"),
		}})
		task.ExpectedEgress = expectedEgress
	case "release_held_tool_use":
		var payload runtimeproxy.HeldToolUseApprovalPayload
		if err := json.Unmarshal(rec.PayloadJSON, &payload); err != nil {
			return nil, "", "", fmt.Errorf("could not parse held tool approval payload")
		}
		if payload.AgentID != "" {
			task.AgentID = payload.AgentID
		}
		if payload.SessionID != "" {
			sessionID = payload.SessionID
		}
		task.Purpose = firstRuntimeTaskPurpose(payload.Reason, fmt.Sprintf("Runtime tool use: %s", payload.ToolName))
		task.ExpectedUse = firstRuntimeTaskPurpose(payload.Reason, fmt.Sprintf("Use runtime tool %s", payload.ToolName))
		expectedTools, _ := json.Marshal([]runtimetasks.ExpectedTool{{
			ToolName:   payload.ToolName,
			InputShape: requiredKeyShape(payload.ToolInput),
			Why:        firstRuntimeTaskPurpose(payload.Reason, "Runtime-reviewed tool use"),
		}})
		task.ExpectedTools = expectedTools
	default:
		return nil, "", "", fmt.Errorf("unsupported runtime approval transport")
	}
	return task, sessionID, task.AgentID, nil
}

func firstRuntimeTaskPurpose(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "Runtime-promoted task"
}

func requiredKeyShape(values map[string]any) map[string]any {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		if key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return nil
	}
	required := make([]any, 0, len(keys))
	for _, key := range keys {
		required = append(required, key)
	}
	return map[string]any{"required_keys": required}
}

func (h *RuntimeHandler) taskExpirySeconds() int {
	if h.cfg == nil || h.cfg.Task.DefaultExpirySeconds <= 0 {
		return 1800
	}
	return h.cfg.Task.DefaultExpirySeconds
}

func (h *RuntimeHandler) oneOffTTLSeconds() int {
	if h.cfg == nil || h.cfg.RuntimePolicy.OneOffTTLSeconds <= 0 {
		return 300
	}
	return h.cfg.RuntimePolicy.OneOffTTLSeconds
}

func (h *RuntimeHandler) ListLeases(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "session_id is required")
		return
	}
	session, err := h.st.GetRuntimeSession(r.Context(), sessionID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "runtime session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get runtime session")
		return
	}
	if session.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your runtime session")
		return
	}
	leases, err := h.st.ListOpenToolExecutionLeases(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list runtime leases")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": leases,
		"total":   len(leases),
	})
}

func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return data
}

func mergeRuntimeSessionMetadata(existing map[string]any, settings store.AgentRuntimeSettings) map[string]any {
	merged := map[string]any{}
	for key, value := range existing {
		merged[key] = value
	}
	if _, ok := merged["runtime_enabled"]; !ok {
		merged["runtime_enabled"] = settings.RuntimeEnabled
	}
	if _, ok := merged["runtime_mode"]; !ok {
		merged["runtime_mode"] = settings.RuntimeMode
	}
	if _, ok := merged["starter_profile"]; !ok {
		merged["starter_profile"] = settings.StarterProfile
	}
	if _, ok := merged["outbound_credential_mode"]; !ok {
		merged["outbound_credential_mode"] = settings.OutboundCredentialMode
	}
	if _, ok := merged["inject_stored_bearer"]; !ok {
		merged["inject_stored_bearer"] = settings.InjectStoredBearer
	}
	return merged
}
