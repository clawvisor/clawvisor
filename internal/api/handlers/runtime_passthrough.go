package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/store"
)

const (
	runtimePassthroughKind          = "passthrough"
	maxRuntimePassthroughTTLSeconds = 30 * 24 * 60 * 60
)

type runtimePassthroughState struct {
	Enabled   bool       `json:"enabled"`
	RuleID    string     `json:"rule_id,omitempty"`
	AgentID   string     `json:"agent_id,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

func (h *RuntimeHandler) GetPassthrough(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	agentID := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	state := h.activePassthroughForUser(r.Context(), user.ID, agentID, time.Now().UTC())
	writeJSON(w, http.StatusOK, state)
}

func (h *RuntimeHandler) EnablePassthrough(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	var body struct {
		AgentID          string `json:"agent_id"`
		TTLSeconds       int    `json:"ttl_seconds"`
		Indefinite       bool   `json:"indefinite"`
		Reason           string `json:"reason"`
		ConfirmationText string `json:"confirmation_text"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.AgentID = strings.TrimSpace(body.AgentID)
	if body.AgentID != "" {
		if _, err := loadUserAgent(r.Context(), h.st, user.ID, body.AgentID); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "agent_id must belong to the current user")
			return
		}
	}
	if !body.Indefinite && body.TTLSeconds <= 0 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "ttl_seconds is required unless indefinite is true")
		return
	}
	if !body.Indefinite && body.TTLSeconds > maxRuntimePassthroughTTLSeconds {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "ttl_seconds must be 30 days or less")
		return
	}
	if body.AgentID == "" && !strings.EqualFold(strings.TrimSpace(body.ConfirmationText), "enable global passthrough") {
		writeError(w, http.StatusBadRequest, "CONFIRMATION_REQUIRED", "set confirmation_text to 'enable global passthrough' for all-agents passthrough")
		return
	}
	if body.AgentID != "" && body.Indefinite && !strings.EqualFold(strings.TrimSpace(body.ConfirmationText), "enable passthrough") {
		writeError(w, http.StatusBadRequest, "CONFIRMATION_REQUIRED", "set confirmation_text to 'enable passthrough' for indefinite passthrough")
		return
	}
	var expiresAt string
	var expiresPtr *time.Time
	if !body.Indefinite {
		t := time.Now().UTC().Add(time.Duration(body.TTLSeconds) * time.Second)
		expiresAt = t.Format(time.RFC3339Nano)
		expiresPtr = &t
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		reason = "user-enabled passthrough mode"
	}
	rule := &store.RuntimePolicyRule{
		ID:      uuid.NewString(),
		UserID:  user.ID,
		Kind:    runtimePassthroughKind,
		Action:  "allow",
		Path:    expiresAt,
		Reason:  reason,
		Source:  "break_glass",
		Enabled: true,
	}
	if body.AgentID != "" {
		rule.AgentID = &body.AgentID
	}
	if err := h.st.CreateRuntimePolicyRule(r.Context(), rule); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not enable passthrough")
		return
	}
	writeJSON(w, http.StatusCreated, runtimePassthroughState{
		Enabled:   true,
		RuleID:    rule.ID,
		AgentID:   body.AgentID,
		ExpiresAt: expiresPtr,
		Reason:    reason,
	})
}

func (h *RuntimeHandler) DisablePassthrough(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	ruleID := strings.TrimSpace(r.PathValue("id"))
	if ruleID != "" {
		if err := h.st.DeleteRuntimePolicyRule(r.Context(), ruleID, user.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not disable passthrough")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "disabled"})
		return
	}
	rules, err := h.st.ListRuntimePolicyRules(r.Context(), user.ID, store.RuntimePolicyRuleFilter{
		Kind: runtimePassthroughKind,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list passthrough state")
		return
	}
	for _, rule := range rules {
		if rule != nil && rule.Source == "break_glass" {
			if err := h.st.DeleteRuntimePolicyRule(r.Context(), rule.ID, user.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not disable passthrough")
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "disabled"})
}

func (h *RuntimeHandler) activePassthroughForUser(ctx context.Context, userID, agentID string, now time.Time) runtimePassthroughState {
	rules, err := h.st.ListRuntimePolicyRules(ctx, userID, store.RuntimePolicyRuleFilter{
		Kind: runtimePassthroughKind,
	})
	if err != nil {
		return runtimePassthroughState{}
	}
	return activePassthroughFromRules(rules, agentID, now)
}

func activePassthroughFromRules(rules []*store.RuntimePolicyRule, agentID string, now time.Time) runtimePassthroughState {
	agentID = strings.TrimSpace(agentID)
	var bestAgent *store.RuntimePolicyRule
	var bestGlobal *store.RuntimePolicyRule
	for _, rule := range rules {
		if rule == nil || !rule.Enabled || rule.Kind != runtimePassthroughKind || rule.Action != "allow" {
			continue
		}
		if rule.AgentID != nil && agentID != "" && *rule.AgentID != agentID {
			continue
		}
		if rule.AgentID != nil && agentID == "" {
			continue
		}
		_, active := passthroughExpiry(rule.Path, now)
		if !active {
			continue
		}
		if rule.AgentID != nil {
			if bestAgent == nil || rule.CreatedAt.After(bestAgent.CreatedAt) {
				bestAgent = rule
			}
			continue
		}
		if bestGlobal == nil || rule.CreatedAt.After(bestGlobal.CreatedAt) {
			bestGlobal = rule
		}
	}
	if bestAgent != nil {
		return passthroughStateFromRule(bestAgent, now)
	}
	if bestGlobal != nil {
		return passthroughStateFromRule(bestGlobal, now)
	}
	return runtimePassthroughState{}
}

func passthroughStateFromRule(rule *store.RuntimePolicyRule, now time.Time) runtimePassthroughState {
	if rule == nil {
		return runtimePassthroughState{}
	}
	expiresAt, active := passthroughExpiry(rule.Path, now)
	if !active {
		return runtimePassthroughState{}
	}
	stateAgent := ""
	if rule.AgentID != nil {
		stateAgent = *rule.AgentID
	}
	return runtimePassthroughState{
		Enabled:   true,
		RuleID:    rule.ID,
		AgentID:   stateAgent,
		ExpiresAt: expiresAt,
		Reason:    rule.Reason,
	}
}

func passthroughExpiry(value string, now time.Time) (*time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, true
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, false
	}
	if !t.After(now) {
		return &t, false
	}
	return &t, true
}
