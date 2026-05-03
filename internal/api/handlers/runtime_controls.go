package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	runtimepolicy "github.com/clawvisor/clawvisor/internal/runtime/policy"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func (h *RuntimeHandler) ListRules(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	filter := store.RuntimePolicyRuleFilter{
		AgentID: strings.TrimSpace(r.URL.Query().Get("agent_id")),
		Kind:    strings.TrimSpace(r.URL.Query().Get("kind")),
		Limit:   500,
	}
	if enabledRaw := strings.TrimSpace(r.URL.Query().Get("enabled")); enabledRaw != "" {
		enabled := enabledRaw == "true" || enabledRaw == "1"
		filter.Enabled = &enabled
	}
	rules, err := h.st.ListRuntimePolicyRules(r.Context(), user.ID, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list runtime rules")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": rules,
		"total":   len(rules),
	})
}

func (h *RuntimeHandler) CreateRule(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	rule, err := h.decodeRuntimePolicyRuleBody(r, user.ID, "")
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	if err := h.st.CreateRuntimePolicyRule(r.Context(), rule); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, "CONFLICT", "runtime rule already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create runtime rule")
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

func (h *RuntimeHandler) GetRule(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	rule, err := h.st.GetRuntimePolicyRule(r.Context(), r.PathValue("id"))
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "runtime rule not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get runtime rule")
		return
	}
	if rule.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your runtime rule")
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (h *RuntimeHandler) UpdateRule(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	ruleID := r.PathValue("id")
	existing, err := h.st.GetRuntimePolicyRule(r.Context(), ruleID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "runtime rule not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get runtime rule")
		return
	}
	if existing.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your runtime rule")
		return
	}
	rule, err := h.decodeRuntimePolicyRuleBody(r, user.ID, ruleID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	rule.CreatedAt = existing.CreatedAt
	rule.LastMatchedAt = existing.LastMatchedAt
	if err := h.st.UpdateRuntimePolicyRule(r.Context(), rule); err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "runtime rule not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not update runtime rule")
		return
	}
	updated, err := h.st.GetRuntimePolicyRule(r.Context(), ruleID)
	if err != nil {
		writeJSON(w, http.StatusOK, rule)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *RuntimeHandler) DeleteRule(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	if err := h.st.DeleteRuntimePolicyRule(r.Context(), r.PathValue("id"), user.ID); err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "runtime rule not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not delete runtime rule")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

func (h *RuntimeHandler) ListStarterProfiles(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": runtimepolicy.StarterProfiles(),
		"total":   len(runtimepolicy.StarterProfiles()),
	})
}

func (h *RuntimeHandler) ApplyStarterProfile(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	profileID := r.PathValue("profile")
	profile, ok := runtimepolicy.StarterProfileByID(profileID)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "starter profile not found")
		return
	}
	var body struct {
		AgentID string `json:"agent_id"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		decoder := json.NewDecoder(io.LimitReader(r.Body, maxRequestBodySize))
		if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeDetailedError(w, http.StatusBadRequest, diagnoseJSONError(err))
			return
		}
	}
	if strings.TrimSpace(body.AgentID) != "" {
		if _, err := loadUserAgent(r.Context(), h.st, user.ID, body.AgentID); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "agent_id must belong to the current user")
			return
		}
	}
	applied := make([]*store.RuntimePolicyRule, 0, len(profile.Rules))
	agentID := strings.TrimSpace(body.AgentID)
	for _, rule := range runtimepolicy.ApplyStarterProfileRules(user.ID, strings.TrimSpace(body.AgentID), *profile) {
		err := h.st.CreateRuntimePolicyRule(r.Context(), rule)
		switch {
		case err == nil:
		case errors.Is(err, store.ErrConflict):
			_ = h.st.UpdateRuntimePolicyRule(r.Context(), rule)
		default:
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not apply starter profile")
			return
		}
		applied = append(applied, rule)
	}
	if agentID != "" {
		settings, err := h.st.GetAgentRuntimeSettings(r.Context(), agentID)
		if err != nil {
			if err == store.ErrNotFound {
				settings = defaultAgentRuntimeSettings(h.cfg, agentID)
			} else {
				writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not update starter profile state")
				return
			}
		}
		settings.StarterProfile = profile.ID
		if err := h.st.UpsertAgentRuntimeSettings(r.Context(), settings); err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not update starter profile state")
			return
		}
	}
	for _, commandKey := range profile.CommandKeys {
		if strings.TrimSpace(commandKey) == "" {
			continue
		}
		if err := h.st.UpsertRuntimePresetDecision(r.Context(), &store.RuntimePresetDecision{
			UserID:     user.ID,
			CommandKey: strings.TrimSpace(strings.ToLower(commandKey)),
			Profile:    profile.ID,
			Decision:   "applied",
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not persist starter profile decision")
			return
		}
	}
	_ = h.st.CreateRuntimeEvent(r.Context(), &store.RuntimeEvent{
		Timestamp:  time.Now().UTC(),
		SessionID:  "runtime-controls",
		UserID:     user.ID,
		AgentID:    agentID,
		EventType:  "runtime.policy.starter_profile_applied",
		ActionKind: "task",
		Decision:   nullableStr("allow"),
		Outcome:    nullableStr("created"),
		Reason:     nullableStr("starter profile rules applied"),
		MetadataJSON: mustJSON(map[string]any{
			"profile":  profile.ID,
			"agent_id": agentID,
			"rules":    len(applied),
		}),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"profile": profile,
		"entries": applied,
		"total":   len(applied),
	})
}

func (h *RuntimeHandler) GetPresetDecision(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	commandKey := strings.TrimSpace(r.URL.Query().Get("command_key"))
	profile := strings.TrimSpace(r.URL.Query().Get("profile"))
	if commandKey == "" || profile == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "command_key and profile are required")
		return
	}
	decision, err := h.st.GetRuntimePresetDecision(r.Context(), user.ID, commandKey, profile)
	if err != nil {
		if err == store.ErrNotFound {
			writeJSON(w, http.StatusOK, map[string]any{"decision": nil})
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get runtime preset decision")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"decision": decision})
}

func (h *RuntimeHandler) UpsertPresetDecision(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	var body struct {
		CommandKey string `json:"command_key"`
		Profile    string `json:"profile"`
		Decision   string `json:"decision"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.CommandKey = strings.TrimSpace(strings.ToLower(body.CommandKey))
	body.Profile = strings.TrimSpace(strings.ToLower(body.Profile))
	body.Decision = strings.TrimSpace(strings.ToLower(body.Decision))
	if body.CommandKey == "" || body.Profile == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "command_key and profile are required")
		return
	}
	switch body.Decision {
	case "applied", "skipped", "always_skip":
	default:
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "decision must be applied, skipped, or always_skip")
		return
	}
	decision := &store.RuntimePresetDecision{
		UserID:     user.ID,
		CommandKey: body.CommandKey,
		Profile:    body.Profile,
		Decision:   body.Decision,
	}
	if err := h.st.UpsertRuntimePresetDecision(r.Context(), decision); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not save runtime preset decision")
		return
	}
	current, err := h.st.GetRuntimePresetDecision(r.Context(), user.ID, body.CommandKey, body.Profile)
	if err != nil {
		writeJSON(w, http.StatusOK, decision)
		return
	}
	writeJSON(w, http.StatusOK, current)
}

func (h *RuntimeHandler) GetRuleCandidateForEvent(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	action := strings.TrimSpace(r.URL.Query().Get("action"))
	event, err := h.st.GetRuntimeEvent(r.Context(), r.PathValue("id"))
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "runtime event not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get runtime event")
		return
	}
	if event.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your runtime event")
		return
	}
	candidate, err := runtimepolicy.NormalizeRuntimeEventToRuleCandidate(event, action)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	if event.AgentID != "" {
		candidate.Rule.AgentID = &event.AgentID
	}
	writeJSON(w, http.StatusOK, candidate)
}

func (h *RuntimeHandler) PromoteEventToTask(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	event, err := h.st.GetRuntimeEvent(r.Context(), r.PathValue("id"))
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "runtime event not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get runtime event")
		return
	}
	if event.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your runtime event")
		return
	}
	var body struct {
		Lifetime string `json:"lifetime"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.Lifetime = strings.TrimSpace(strings.ToLower(body.Lifetime))
	if body.Lifetime == "" {
		body.Lifetime = "session"
	}
	if body.Lifetime != "session" && body.Lifetime != "standing" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "lifetime must be session or standing")
		return
	}
	task, err := h.createTaskFromRuntimeEvent(r.Context(), event, body.Lifetime)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"task":     task,
		"task_id":  task.ID,
		"lifetime": body.Lifetime,
	})
}

func (h *RuntimeHandler) createTaskFromRuntimeEvent(ctx context.Context, event *store.RuntimeEvent, lifetime string) (*store.Task, error) {
	if event == nil {
		return nil, fmt.Errorf("runtime event is required")
	}
	task := &store.Task{
		ID:             uuid.NewSHA1(uuid.NameSpaceURL, []byte("runtime-event:"+event.ID+":"+lifetime)).String(),
		UserID:         event.UserID,
		AgentID:        event.AgentID,
		Lifetime:       lifetime,
		Status:         "active",
		SchemaVersion:  2,
		ApprovalSource: "manual",
	}
	now := time.Now().UTC()
	task.ApprovedAt = &now
	if lifetime == "session" {
		expiresIn := h.taskExpirySeconds()
		task.ExpiresInSeconds = expiresIn
		expiresAt := now.Add(time.Duration(expiresIn) * time.Second)
		task.ExpiresAt = &expiresAt
	}
	meta := runtimepolicy.NormalizeRuntimeEventTaskEnvelope(event)
	if meta == nil {
		return nil, fmt.Errorf("runtime event cannot be promoted to a task")
	}
	task.Purpose = meta.Purpose
	task.ExpectedUse = meta.ExpectedUse
	task.ExpectedEgress = meta.ExpectedEgress
	task.ExpectedTools = meta.ExpectedTools
	if err := h.st.CreateTask(ctx, task); err != nil {
		if !errors.Is(err, store.ErrConflict) {
			return nil, fmt.Errorf("could not create task")
		}
		existing, getErr := h.st.GetTask(ctx, task.ID)
		if getErr != nil {
			return nil, fmt.Errorf("could not load promoted task")
		}
		task = existing
	}
	if event.SessionID != "" {
		metadataJSON := mustJSON(map[string]any{"runtime_event_id": event.ID, "source": "runtime_event"})
		_ = h.st.UpsertActiveTaskSession(ctx, &store.ActiveTaskSession{
			TaskID:       task.ID,
			SessionID:    event.SessionID,
			UserID:       event.UserID,
			AgentID:      event.AgentID,
			MetadataJSON: metadataJSON,
			StartedAt:    now,
			LastSeenAt:   now,
			Status:       "active",
		})
	}
	return task, nil
}

func (h *RuntimeHandler) decodeRuntimePolicyRuleBody(r *http.Request, userID, ruleID string) (*store.RuntimePolicyRule, error) {
	var body struct {
		AgentID       string         `json:"agent_id"`
		Scope         string         `json:"scope"`
		Kind          string         `json:"kind"`
		Action        string         `json:"action"`
		Service       string         `json:"service"`
		ServiceAction string         `json:"service_action"`
		Host          string         `json:"host"`
		Method        string         `json:"method"`
		Path          string         `json:"path"`
		PathRegex     string         `json:"path_regex"`
		HeadersShape  map[string]any `json:"headers_shape_json"`
		BodyShape     map[string]any `json:"body_shape_json"`
		ToolName      string         `json:"tool_name"`
		InputShape    map[string]any `json:"input_shape_json"`
		InputRegex    string         `json:"input_regex"`
		Reason        string         `json:"reason"`
		Source        string         `json:"source"`
		Enabled       *bool          `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequestBodySize)).Decode(&body); err != nil {
		return nil, fmt.Errorf("%s", diagnoseJSONError(err).Error)
	}
	kind := strings.TrimSpace(strings.ToLower(body.Kind))
	action := strings.TrimSpace(strings.ToLower(body.Action))
	scope := strings.TrimSpace(strings.ToLower(body.Scope))
	switch kind {
	case "egress", "tool", "service":
	default:
		return nil, fmt.Errorf("kind must be egress, tool, or service")
	}
	switch action {
	case "allow", "deny", "review":
	default:
		return nil, fmt.Errorf("action must be allow, deny, or review")
	}
	var agentID *string
	switch scope {
	case "", "agent":
		if strings.TrimSpace(body.AgentID) != "" {
			if _, err := loadUserAgent(r.Context(), h.st, userID, body.AgentID); err != nil {
				return nil, fmt.Errorf("agent_id must belong to the current user")
			}
			agentID = &body.AgentID
		}
	case "global":
	default:
		return nil, fmt.Errorf("scope must be agent or global")
	}
	if kind == "egress" && strings.TrimSpace(body.Host) == "" {
		return nil, fmt.Errorf("host is required for egress rules")
	}
	if kind == "tool" && strings.TrimSpace(body.ToolName) == "" {
		return nil, fmt.Errorf("tool_name is required for tool rules")
	}
	if kind == "service" && strings.TrimSpace(body.Service) == "" {
		return nil, fmt.Errorf("service is required for service rules")
	}
	source := strings.TrimSpace(strings.ToLower(body.Source))
	if source == "" {
		source = "user"
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	rule := &store.RuntimePolicyRule{
		ID:            ruleID,
		UserID:        userID,
		AgentID:       agentID,
		Kind:          kind,
		Action:        action,
		Service:       strings.TrimSpace(body.Service),
		ServiceAction: strings.TrimSpace(body.ServiceAction),
		Host:          strings.TrimSpace(strings.ToLower(body.Host)),
		Method:        strings.ToUpper(strings.TrimSpace(body.Method)),
		Path:          strings.TrimSpace(body.Path),
		PathRegex:     strings.TrimSpace(body.PathRegex),
		ToolName:      strings.TrimSpace(body.ToolName),
		InputRegex:    strings.TrimSpace(body.InputRegex),
		Reason:        strings.TrimSpace(body.Reason),
		Source:        source,
		Enabled:       enabled,
	}
	if len(body.HeadersShape) > 0 {
		rule.HeadersShape = mustJSON(body.HeadersShape)
	}
	if len(body.BodyShape) > 0 {
		rule.BodyShape = mustJSON(body.BodyShape)
	}
	if len(body.InputShape) > 0 {
		rule.InputShape = mustJSON(body.InputShape)
	}
	if kind == "tool" {
		rule.Service = ""
		rule.ServiceAction = ""
		rule.Host = ""
		rule.Method = ""
		rule.Path = ""
		rule.PathRegex = ""
		rule.HeadersShape = nil
		rule.BodyShape = nil
	}
	if kind == "egress" {
		rule.Service = ""
		rule.ServiceAction = ""
		rule.ToolName = ""
		rule.InputShape = nil
		rule.InputRegex = ""
	}
	if kind == "service" {
		rule.Host = ""
		rule.Method = ""
		rule.Path = ""
		rule.PathRegex = ""
		rule.HeadersShape = nil
		rule.BodyShape = nil
		rule.ToolName = ""
		rule.InputShape = nil
		rule.InputRegex = ""
		if rule.ServiceAction == "" {
			rule.ServiceAction = "*"
		}
	}
	return rule, nil
}
