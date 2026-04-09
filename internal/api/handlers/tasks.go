package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/callback"
	"github.com/clawvisor/clawvisor/internal/events"
	"github.com/clawvisor/clawvisor/internal/groupchat"
	"github.com/clawvisor/clawvisor/internal/intent"
	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// hardcodedApprovalActions lists service:action pairs that always require
// per-request human approval, regardless of policy or task scope.
var hardcodedApprovalActions = map[string]bool{
	"apple.imessage:send_message": true,
}

// RequiresHardcodedApproval returns true if the given service+action always
// requires individual human approval.
func RequiresHardcodedApproval(service, action string) bool {
	return hardcodedApprovalActions[service+":"+action]
}

// TasksHandler manages task-scoped authorization.
type TasksHandler struct {
	st           store.Store
	vault        vault.Vault
	adapterReg   *adapters.Registry
	notifier     notify.Notifier
	cfg          config.Config
	logger       *slog.Logger
	baseURL      string
	eventHub     events.EventHub
	assessor     taskrisk.Assessor
	contentDedup *dedupCache
	msgBuffer    *groupchat.MessageBuffer // may be nil; set via SetGroupApproval
	llmHealth    *llm.Health              // may be nil; needed for approval check LLM calls
	agentPairer  notify.AgentGroupPairer  // may be nil; set via SetGroupApproval
}

func NewTasksHandler(
	st store.Store,
	v vault.Vault,
	adapterReg *adapters.Registry,
	notifier notify.Notifier,
	cfg config.Config,
	logger *slog.Logger,
	baseURL string,
	eventHub events.EventHub,
	assessor taskrisk.Assessor,
) *TasksHandler {
	dedupTTL := time.Duration(cfg.Gateway.ContentDedupTTLSeconds) * time.Second
	if dedupTTL <= 0 {
		dedupTTL = 5 * time.Second
	}
	return &TasksHandler{
		st: st, vault: v, adapterReg: adapterReg, notifier: notifier, cfg: cfg, logger: logger, baseURL: baseURL,
		eventHub: eventHub, assessor: assessor,
		contentDedup: newDedupCache(dedupTTL),
	}
}

// SetGroupApproval configures the message buffer, LLM health, and agent-group
// pairer used for on-demand group chat approval checks during task creation.
func (h *TasksHandler) SetGroupApproval(buf *groupchat.MessageBuffer, health *llm.Health, pairer notify.AgentGroupPairer) {
	h.msgBuffer = buf
	h.llmHealth = health
	h.agentPairer = pairer
}

// ── Create ────────────────────────────────────────────────────────────────────

type createTaskRequest struct {
	Purpose           string              `json:"purpose"`
	AuthorizedActions []store.TaskAction  `json:"authorized_actions"`
	PlannedCalls      []store.PlannedCall `json:"planned_calls,omitempty"`
	ExpiresInSeconds  int                 `json:"expires_in_seconds"`
	CallbackURL       string              `json:"callback_url"`
	Lifetime          string              `json:"lifetime"` // "session" (default) or "standing"
}

// Create declares a new task scope.
//
// POST /api/tasks
// Auth: agent bearer token
func (h *TasksHandler) Create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agent := middleware.AgentFromContext(ctx)
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var req createTaskRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Purpose == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "purpose is required")
		return
	}
	if len(req.AuthorizedActions) == 0 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "authorized_actions is required and must be non-empty")
		return
	}

	// Validate each authorized action.
	for _, a := range req.AuthorizedActions {
		serviceType, serviceAlias := parseServiceAlias(a.Service)

		// Guard virtual services (file, bash, search, web, agent, unknown) are
		// scope-only markers used by permission hooks — they never execute
		// through adapters, so skip adapter/activation validation.
		if !isGuardVirtualService(serviceType) {
			adapter, ok := h.adapterReg.GetForUser(ctx, serviceType, agent.UserID)
			if !ok {
				writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
					fmt.Sprintf("unknown service %q", a.Service))
				return
			}
			if !adapterSupportsAction(adapter, a.Action) {
				writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
					fmt.Sprintf("service %q does not support action %q", serviceType, a.Action))
				return
			}
			if !h.serviceActivated(ctx, agent.UserID, serviceType, serviceAlias, adapter) {
				code, userErr, _ := serviceNotActivatedResponse(ctx, h.vault, h.st, h.adapterReg, agent.UserID, serviceType, serviceAlias, a.Service, adapter)
				writeError(w, http.StatusBadRequest, code, userErr)
				return
			}
		}
		if a.AutoExecute && RequiresHardcodedApproval(a.Service, a.Action) {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
				fmt.Sprintf("action %s:%s has hardcoded approval — auto_execute must be false", a.Service, a.Action))
			return
		}
	}

	// Validate planned calls: each must reference a service:action covered by authorized_actions.
	for _, pc := range req.PlannedCalls {
		if pc.Service == "" || pc.Action == "" {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "planned_calls entries must have service and action")
			return
		}
		if pc.Reason == "" {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
				fmt.Sprintf("planned_calls entry %s:%s must have a reason", pc.Service, pc.Action))
			return
		}
		covered := false
		pcServiceType, _ := parseServiceAlias(pc.Service)
		for _, a := range req.AuthorizedActions {
			aServiceType, _ := parseServiceAlias(a.Service)
			if aServiceType == pcServiceType && (a.Action == pc.Action || a.Action == "*") {
				covered = true
				break
			}
		}
		if !covered {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
				fmt.Sprintf("planned_calls entry %s:%s is not covered by authorized_actions", pc.Service, pc.Action))
			return
		}
	}

	lifetime := req.Lifetime
	if lifetime == "" {
		lifetime = "session"
	}
	if lifetime != "session" && lifetime != "standing" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "lifetime must be \"session\" or \"standing\"")
		return
	}
	if lifetime == "standing" && req.ExpiresInSeconds > 0 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			"expires_in_seconds cannot be set on a standing task — standing tasks have no expiry (revoke them to deactivate)")
		return
	}

	// Content-based dedup: if an identical task creation request was recently made
	// by the same agent, return the existing task instead of creating a duplicate.
	taskDedupKey := buildDedupKey("task", agent.ID, req.Purpose, req.AuthorizedActions, lifetime)
	if cached, ok := h.contentDedup.Get(taskDedupKey); ok {
		resp := cached.(map[string]any)
		writeJSON(w, http.StatusCreated, resp)
		return
	}

	expiresIn := req.ExpiresInSeconds
	if expiresIn <= 0 {
		expiresIn = h.cfg.Task.DefaultExpirySeconds
	}

	// All tasks start as pending_approval — no policy-based auto-activation.
	task := &store.Task{
		ID:                uuid.New().String(),
		UserID:            agent.UserID,
		AgentID:           agent.ID,
		Purpose:           req.Purpose,
		Status:            "pending_approval",
		Lifetime:          lifetime,
		AuthorizedActions: req.AuthorizedActions,
		PlannedCalls:      req.PlannedCalls,
		ExpiresInSeconds:  expiresIn,
	}
	if req.CallbackURL != "" {
		task.CallbackURL = &req.CallbackURL
	}

	// Run risk assessment (non-blocking — errors are logged, not propagated).
	if h.assessor != nil {
		assessment, err := h.assessor.Assess(ctx, taskrisk.AssessRequest{
			Purpose:           req.Purpose,
			AuthorizedActions: req.AuthorizedActions,
			PlannedCalls:      req.PlannedCalls,
			AgentName:         agent.Name,
		})
		if err != nil {
			h.logger.Warn("task risk assessment failed", "error", err)
		}
		if assessment != nil {
			task.RiskLevel = assessment.RiskLevel
			task.RiskDetails = taskrisk.MarshalAssessment(assessment)
		}
	}

	// Check for group chat approval via LLM analysis of recent messages.
	// Only auto-approve low/medium risk tasks without hardcoded approval actions.
	// The agent must be paired to a group chat and the user must have opted in.
	preApproved := false
	groupChatID := ""
	if h.agentPairer != nil {
		groupChatID, _ = h.agentPairer.AgentGroupChatID(ctx, agent.ID)
	}
	autoApprovalEnabled := false
	autoApprovalNotify := true // on by default
	if groupChatID != "" {
		if nc, err := h.st.GetNotificationConfig(ctx, agent.UserID, "telegram"); err == nil {
			var cfgMap map[string]any
			if json.Unmarshal(nc.Config, &cfgMap) == nil {
				autoApprovalEnabled, _ = cfgMap["auto_approval_enabled"].(bool)
				if v, ok := cfgMap["auto_approval_notify"].(bool); ok {
					autoApprovalNotify = v
				}
			}
		}
	}
	if autoApprovalEnabled && groupChatID != "" && h.msgBuffer != nil && h.llmHealth != nil &&
		task.RiskLevel != "high" && task.RiskLevel != "critical" {
		hasHardcoded := false
		for _, a := range req.AuthorizedActions {
			if RequiresHardcodedApproval(a.Service, a.Action) {
				hasHardcoded = true
				break
			}
		}
		if !hasHardcoded {
			messages := h.msgBuffer.Messages(groupChatID)
			if len(messages) > 0 {
				var actionStrs []string
				for _, a := range req.AuthorizedActions {
					actionStrs = append(actionStrs, a.Service+":"+a.Action)
				}
				result, err := intent.CheckApproval(ctx, h.llmHealth, intent.ApprovalCheckRequest{
					Messages:    messages,
					TaskPurpose: req.Purpose,
					TaskActions: actionStrs,
					AgentName:   agent.Name,
				})
				if err != nil {
					h.logger.Warn("group chat approval check failed", "err", err, "user_id", agent.UserID)
				} else if result != nil && result.Approved {
					preApproved = true
					task.Status = "active"
					now := time.Now().UTC()
					task.ApprovedAt = &now
					if task.Lifetime == "standing" {
						sentinel := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
						task.ExpiresAt = &sentinel
					} else {
						expiresAt := now.Add(time.Duration(task.ExpiresInSeconds) * time.Second)
						task.ExpiresAt = &expiresAt
					}
					task.ApprovalSource = "telegram_group"
					rationale, _ := json.Marshal(map[string]any{
						"explanation": result.Explanation,
						"confidence":  result.Confidence,
						"model":       result.Model,
						"latency_ms":  result.LatencyMS,
					})
					task.ApprovalRationale = rationale
					h.logger.Info("task auto-approved via group chat LLM check",
						"task_id", task.ID, "confidence", result.Confidence,
						"explanation", result.Explanation, "model", result.Model,
						"latency_ms", result.LatencyMS)
				}
			}
		}
	}

	if err := h.st.CreateTask(ctx, task); err != nil {
		h.logger.Warn("create task failed", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create task")
		return
	}

	if preApproved {
		// Send confirmation DM (if notifications enabled).
		if h.notifier != nil && autoApprovalNotify {
			text := fmt.Sprintf("✅ <b>Task auto-approved</b> (group chat observation)\n\n"+
				"<b>Agent:</b> %s\n<b>Purpose:</b> %s",
				agent.Name, req.Purpose)
			if task.RiskLevel != "" {
				text += fmt.Sprintf("\n<b>Risk:</b> %s", task.RiskLevel)
			}
			_ = h.notifier.SendAlert(ctx, agent.UserID, text)
		}

		// Deliver callback to agent if configured.
		if task.CallbackURL != nil && *task.CallbackURL != "" {
			cbKey, _ := h.st.GetAgentCallbackSecret(ctx, task.AgentID)
			go func() {
				_ = callback.DeliverResult(context.Background(), *task.CallbackURL, &callback.Payload{
					Type:   "task",
					TaskID: task.ID,
					Status: "approved",
				}, cbKey)
			}()
		}

		h.publishTasksAndQueue(agent.UserID)

		resp := map[string]any{
			"task_id":         task.ID,
			"status":          "active",
			"message":         "Task auto-approved via Telegram group pre-approval.",
			"approval_source": "telegram_group_observation",
		}
		if task.ExpiresAt != nil {
			resp["expires_at"] = task.ExpiresAt.Format(time.RFC3339)
		}
		h.contentDedup.Put(taskDedupKey, resp)
		writeJSON(w, http.StatusCreated, resp)
		return
	}

	// Send notification.
	if h.notifier != nil {
		approveURL := fmt.Sprintf("%s/dashboard/tasks?action=approve&task_id=%s", h.baseURL, task.ID)
		denyURL := fmt.Sprintf("%s/dashboard/tasks?action=deny&task_id=%s", h.baseURL, task.ID)
		expiresInStr := fmt.Sprintf("%d minutes", expiresIn/60)
		if lifetime == "standing" {
			expiresInStr = "standing (no expiry)"
		}

		if msgID, err := h.notifier.SendTaskApprovalRequest(ctx, notify.TaskApprovalRequest{
			TaskID:       task.ID,
			UserID:       agent.UserID,
			AgentName:    agent.Name,
			Purpose:      req.Purpose,
			Actions:      req.AuthorizedActions,
			PlannedCalls: req.PlannedCalls,
			RiskLevel:    task.RiskLevel,
			ApproveURL:   approveURL,
			DenyURL:      denyURL,
			ExpiresIn:    expiresInStr,
		}); err != nil {
			h.logger.Warn("failed to send task approval notification", "task_id", task.ID, "err", err)
		} else if msgID != "" {
			_ = h.st.SaveNotificationMessage(ctx, "task", task.ID, "telegram", msgID)
		}
	}

	h.publishTasksAndQueue(agent.UserID)

	// If wait=true, long-poll until the task is approved or denied.
	if r.URL.Query().Get("wait") == "true" && h.eventHub != nil {
		timeout := parseLongPollTimeout(r)
		resolved := h.waitForTaskResolution(ctx, task.ID, agent.UserID, time.Duration(timeout)*time.Second)
		sanitizeTaskForResponse(resolved)
		writeJSON(w, http.StatusCreated, resolved)
		return
	}

	resp := map[string]any{
		"task_id": task.ID,
		"status":  "pending_approval",
		"message": "Task approval requested. Waiting for human review.",
	}
	h.contentDedup.Put(taskDedupKey, resp)
	writeJSON(w, http.StatusCreated, resp)
}

// ── Get ───────────────────────────────────────────────────────────────────────

// Get returns task details. Agent must own the task via agent's user_id.
//
// GET /api/tasks/{id}
// Auth: agent bearer token
//
// Query params:
//
//	wait=true    – long-poll until the task leaves a pending state (or timeout)
//	timeout=N    – wait timeout in seconds (default 120, max 120)
func (h *TasksHandler) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agent := middleware.AgentFromContext(ctx)
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	taskID := r.PathValue("id")
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get task")
		return
	}
	if task.UserID != agent.UserID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your task")
		return
	}

	// Long-poll: if wait=true and task is still pending, block until it
	// transitions or the timeout elapses.
	if r.URL.Query().Get("wait") == "true" && isTaskPending(task.Status) && h.eventHub != nil {
		timeout := parseLongPollTimeout(r)
		task = h.waitForTaskResolution(ctx, taskID, agent.UserID, time.Duration(timeout)*time.Second)
	}

	sanitizeTaskForResponse(task)
	writeJSON(w, http.StatusOK, task)
}

// isTaskPending returns true if the status represents a state that is
// waiting on user action.
func isTaskPending(status string) bool {
	return status == "pending_approval" || status == "pending_scope_expansion"
}

// waitForTaskResolution long-polls until the task leaves a pending state
// (approved/denied) or the timeout expires.
func (h *TasksHandler) waitForTaskResolution(ctx context.Context, taskID, userID string, timeout time.Duration) *store.Task {
	return events.WaitFor(ctx, h.eventHub, userID, timeout,
		[]string{"tasks"},
		func(c context.Context) (*store.Task, bool) {
			t, err := h.st.GetTask(c, taskID)
			if err != nil {
				return &store.Task{ID: taskID}, false
			}
			return t, !isTaskPending(t.Status)
		},
	)
}

// ── List ──────────────────────────────────────────────────────────────────────

// List returns pending and active tasks for the authenticated user.
//
// GET /api/tasks
// Auth: user JWT
func (h *TasksHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	var filter store.TaskFilter
	if r.URL.Query().Get("active_only") == "true" {
		filter.ActiveOnly = true
	}
	if v := r.URL.Query().Get("status"); v != "" {
		filter.Status = v
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			filter.Limit = n
			if filter.Limit > maxListLimit {
				filter.Limit = maxListLimit
			}
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			filter.Offset = n
		}
	}

	h.logger.Info("listing tasks", "active_only", filter.ActiveOnly, "status", filter.Status, "limit", filter.Limit, "offset", filter.Offset)

	tasks, total, err := h.st.ListTasks(ctx, user.ID, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list tasks")
		return
	}
	if tasks == nil {
		tasks = []*store.Task{}
	}
	for _, t := range tasks {
		if sanitizeTaskForResponse(t) {
			// Opportunistically mark expired tasks in the DB so the
			// background sweep doesn't have to catch them later.
			_ = h.st.UpdateTaskStatus(ctx, t.ID, "expired")
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total": total,
		"tasks": tasks,
	})
}

// sanitizeTaskForResponse cleans up task fields before serialization:
//   - Standing tasks: nil out the sentinel expiry so it doesn't leak.
//   - Active session tasks past their expiry: report status as "expired"
//     even if the background cleanup goroutine hasn't swept them yet.
func sanitizeTaskForResponse(t *store.Task) (nowExpired bool) {
	if t.Lifetime == "standing" {
		t.ExpiresAt = nil
		t.ExpiresInSeconds = 0
		return false
	}
	if t.Status == "active" && t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now()) {
		t.Status = "expired"
		return true
	}
	return false
}

// ── Approve ───────────────────────────────────────────────────────────────────

// Approve sets the task to active and starts its expiry timer.
//
// POST /api/tasks/{id}/approve
// Auth: user JWT
func (h *TasksHandler) Approve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	taskID := r.PathValue("id")
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get task")
		return
	}
	if task.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your task")
		return
	}
	if task.Status != "pending_approval" {
		writeError(w, http.StatusConflict, "INVALID_STATE", "task is not pending approval")
		return
	}

	// Standing tasks have no expiry; session tasks expire after ExpiresInSeconds.
	var expiresAt time.Time
	if task.Lifetime == "standing" {
		// Far-future sentinel — standing tasks are revoked manually, not expired.
		expiresAt = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	} else {
		expiresAt = time.Now().UTC().Add(time.Duration(task.ExpiresInSeconds) * time.Second)
	}

	if err := h.st.UpdateTaskApproved(ctx, taskID, expiresAt); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not approve task")
		return
	}

	// Deliver callback if set.
	if task.CallbackURL != nil && *task.CallbackURL != "" {
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, task.AgentID)
		go func() {
			_ = callback.DeliverResult(context.Background(), *task.CallbackURL, &callback.Payload{
				Type:   "task",
				TaskID: taskID,
				Status: "approved",
			}, cbKey)
		}()
	}

	h.publishTasksAndQueue(user.ID)

	resp := map[string]any{
		"task_id": taskID,
		"status":  "active",
	}
	if task.Lifetime != "standing" {
		resp["expires_at"] = expiresAt.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

// ── Deny ──────────────────────────────────────────────────────────────────────

// Deny rejects a pending task.
//
// POST /api/tasks/{id}/deny
// Auth: user JWT
func (h *TasksHandler) Deny(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	taskID := r.PathValue("id")
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get task")
		return
	}
	if task.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your task")
		return
	}
	if task.Status != "pending_approval" && task.Status != "pending_scope_expansion" {
		writeError(w, http.StatusConflict, "INVALID_STATE", "task is not pending approval or scope expansion")
		return
	}

	if err := h.st.UpdateTaskStatus(ctx, taskID, "denied"); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not deny task")
		return
	}
	if err := h.st.DeleteChainFactsByTask(ctx, taskID); err != nil {
		h.logger.Warn("chain facts cleanup failed", "err", err, "task_id", taskID)
	}

	h.publishTasksAndQueue(user.ID)

	// Deliver callback if set.
	if task.CallbackURL != nil && *task.CallbackURL != "" {
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, task.AgentID)
		go func() {
			_ = callback.DeliverResult(context.Background(), *task.CallbackURL, &callback.Payload{
				Type:   "task",
				TaskID: taskID,
				Status: "denied",
			}, cbKey)
		}()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"task_id": taskID,
		"status":  "denied",
	})
}

// ── Complete ──────────────────────────────────────────────────────────────────

// Complete marks a task as finished.
//
// POST /api/tasks/{id}/complete
// Auth: agent bearer token
func (h *TasksHandler) Complete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agent := middleware.AgentFromContext(ctx)
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	taskID := r.PathValue("id")
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get task")
		return
	}
	if task.UserID != agent.UserID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your task")
		return
	}
	if task.Status != "active" {
		writeError(w, http.StatusConflict, "INVALID_STATE", "task is not active")
		return
	}

	if err := h.st.UpdateTaskStatus(ctx, taskID, "completed"); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not complete task")
		return
	}
	if err := h.st.DeleteChainFactsByTask(ctx, taskID); err != nil {
		h.logger.Warn("chain facts cleanup failed", "err", err, "task_id", taskID)
	}

	h.publishTasksAndQueue(agent.UserID)

	writeJSON(w, http.StatusOK, map[string]any{
		"task_id": taskID,
		"status":  "completed",
	})
}

// ── Expand ────────────────────────────────────────────────────────────────────

type expandTaskRequest struct {
	Service     string `json:"service"`
	Action      string `json:"action"`
	AutoExecute bool   `json:"auto_execute"`
	Reason      string `json:"reason"`
}

// Expand requests adding a new action to a task's scope.
//
// POST /api/tasks/{id}/expand
// Auth: agent bearer token
func (h *TasksHandler) Expand(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agent := middleware.AgentFromContext(ctx)
	if agent == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	taskID := r.PathValue("id")
	var req expandTaskRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Service == "" || req.Action == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "service and action are required")
		return
	}

	// Validate service and action exist (skip for guard virtual services).
	serviceType, serviceAlias := parseServiceAlias(req.Service)
	if !isGuardVirtualService(serviceType) {
		adapter, ok := h.adapterReg.GetForUser(ctx, serviceType, agent.UserID)
		if !ok {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
				fmt.Sprintf("unknown service %q", req.Service))
			return
		}
		if !adapterSupportsAction(adapter, req.Action) {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
				fmt.Sprintf("service %q does not support action %q", serviceType, req.Action))
			return
		}
		if !h.serviceActivated(ctx, agent.UserID, serviceType, serviceAlias, adapter) {
			code, userErr, _ := serviceNotActivatedResponse(ctx, h.vault, h.st, h.adapterReg, agent.UserID, serviceType, serviceAlias, req.Service, adapter)
			writeError(w, http.StatusBadRequest, code, userErr)
			return
		}
	}

	// Validate hardcode.
	if req.AutoExecute && RequiresHardcodedApproval(req.Service, req.Action) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			fmt.Sprintf("action %s:%s has hardcoded approval — auto_execute must be false", req.Service, req.Action))
		return
	}

	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get task")
		return
	}
	if task.UserID != agent.UserID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your task")
		return
	}
	if task.Status != "active" && task.Status != "expired" {
		writeError(w, http.StatusConflict, "INVALID_STATE", "task must be active or expired to expand")
		return
	}
	if task.Lifetime == "standing" {
		writeError(w, http.StatusConflict, "INVALID_OPERATION",
			"standing tasks cannot be expanded — revoke this task and create a new one with the additional actions, or create a separate session task for the new action")
		return
	}

	pendingAction := &store.TaskAction{
		Service:     req.Service,
		Action:      req.Action,
		AutoExecute: req.AutoExecute,
	}

	if err := h.st.SetTaskPendingExpansion(ctx, taskID, pendingAction, req.Reason); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not request scope expansion")
		return
	}

	// Send notification.
	if h.notifier != nil {
		approveURL := fmt.Sprintf("%s/dashboard/tasks?action=expand_approve&task_id=%s", h.baseURL, taskID)
		denyURL := fmt.Sprintf("%s/dashboard/tasks?action=expand_deny&task_id=%s", h.baseURL, taskID)

		if msgID, err := h.notifier.SendScopeExpansionRequest(ctx, notify.ScopeExpansionRequest{
			TaskID:     taskID,
			UserID:     agent.UserID,
			AgentName:  agent.Name,
			Purpose:    task.Purpose,
			NewAction:  *pendingAction,
			Reason:     req.Reason,
			ApproveURL: approveURL,
			DenyURL:    denyURL,
		}); err != nil {
			h.logger.Warn("failed to send scope expansion notification", "task_id", taskID, "err", err)
		} else if msgID != "" {
			_ = h.st.SaveNotificationMessage(ctx, "task", taskID, "telegram", msgID)
		}
	}

	h.publishTasksAndQueue(agent.UserID)

	// If wait=true, long-poll until the expansion is approved or denied.
	if r.URL.Query().Get("wait") == "true" && h.eventHub != nil {
		timeout := parseLongPollTimeout(r)
		resolved := h.waitForTaskResolution(ctx, taskID, agent.UserID, time.Duration(timeout)*time.Second)
		sanitizeTaskForResponse(resolved)
		writeJSON(w, http.StatusOK, resolved)
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"task_id": taskID,
		"status":  "pending_scope_expansion",
		"message": fmt.Sprintf("Scope expansion requested for %s:%s. Waiting for approval.", req.Service, req.Action),
	})
}

// ExpandApprove approves a pending scope expansion.
//
// POST /api/tasks/{id}/expand/approve
// Auth: user JWT
func (h *TasksHandler) ExpandApprove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	taskID := r.PathValue("id")
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get task")
		return
	}
	if task.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your task")
		return
	}
	if task.Status != "pending_scope_expansion" || task.PendingAction == nil {
		writeError(w, http.StatusConflict, "INVALID_STATE", "task has no pending scope expansion")
		return
	}

	// Carry the expansion rationale into the action for intent verification.
	if task.PendingReason != "" {
		task.PendingAction.ExpansionRationale = task.PendingReason
	}

	// Add the pending action to authorized_actions.
	newActions := append(task.AuthorizedActions, *task.PendingAction)
	expiresAt := time.Now().UTC().Add(time.Duration(task.ExpiresInSeconds) * time.Second)

	if err := h.st.UpdateTaskActions(ctx, taskID, newActions, expiresAt); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not expand task")
		return
	}

	// Deliver callback if set.
	if task.CallbackURL != nil && *task.CallbackURL != "" {
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, task.AgentID)
		go func() {
			_ = callback.DeliverResult(context.Background(), *task.CallbackURL, &callback.Payload{
				Type:   "task",
				TaskID: taskID,
				Status: "scope_expanded",
			}, cbKey)
		}()
	}

	h.publishTasksAndQueue(user.ID)

	writeJSON(w, http.StatusOK, map[string]any{
		"task_id":    taskID,
		"status":     "active",
		"expires_at": expiresAt.Format(time.RFC3339),
	})
}

// ExpandDeny denies a pending scope expansion.
//
// POST /api/tasks/{id}/expand/deny
// Auth: user JWT
func (h *TasksHandler) ExpandDeny(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	taskID := r.PathValue("id")
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get task")
		return
	}
	if task.UserID != user.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "not your task")
		return
	}
	if task.Status != "pending_scope_expansion" {
		writeError(w, http.StatusConflict, "INVALID_STATE", "task has no pending scope expansion")
		return
	}

	// Revert to active (or expired if it was expired before).
	newStatus := "active"
	if task.ExpiresAt != nil && time.Now().After(*task.ExpiresAt) {
		newStatus = "expired"
	}

	// Clear pending_action by updating with the same actions (no new one added)
	// and keeping the same expiry.
	exp := time.Now().UTC()
	if task.ExpiresAt != nil {
		exp = *task.ExpiresAt
	}
	if err := h.st.UpdateTaskActions(ctx, taskID, task.AuthorizedActions, exp); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not deny expansion")
		return
	}
	// Restore proper status (UpdateTaskActions sets status to active).
	if newStatus != "active" {
		_ = h.st.UpdateTaskStatus(ctx, taskID, newStatus)
	}

	h.publishTasksAndQueue(user.ID)

	// Deliver callback if set.
	if task.CallbackURL != nil && *task.CallbackURL != "" {
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, task.AgentID)
		go func() {
			_ = callback.DeliverResult(context.Background(), *task.CallbackURL, &callback.Payload{
				Type:   "task",
				TaskID: taskID,
				Status: "scope_expansion_denied",
			}, cbKey)
		}()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"task_id": taskID,
		"status":  newStatus,
	})
}

// ── Core approve/deny methods (used by HTTP handlers and Telegram consumer) ──

// ApproveByTaskID approves a pending task.
func (h *TasksHandler) ApproveByTaskID(ctx context.Context, taskID, userID string) error {
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.UserID != userID {
		return fmt.Errorf("not your task")
	}
	if task.Status != "pending_approval" {
		return fmt.Errorf("task is not pending approval")
	}

	var expiresAt time.Time
	if task.Lifetime == "standing" {
		expiresAt = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	} else {
		expiresAt = time.Now().UTC().Add(time.Duration(task.ExpiresInSeconds) * time.Second)
	}
	if err := h.st.UpdateTaskApproved(ctx, taskID, expiresAt); err != nil {
		return err
	}

	h.updateNotificationMsg(ctx, "task", taskID, userID, "✅ <b>Approved</b> — task activated.")
	h.publishTasksAndQueue(userID)

	if task.CallbackURL != nil && *task.CallbackURL != "" {
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, task.AgentID)
		go func() {
			_ = callback.DeliverResult(context.Background(), *task.CallbackURL, &callback.Payload{
				Type:   "task",
				TaskID: taskID,
				Status: "approved",
			}, cbKey)
		}()
	}
	return nil
}

// DenyByTaskID denies a pending task.
func (h *TasksHandler) DenyByTaskID(ctx context.Context, taskID, userID string) error {
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.UserID != userID {
		return fmt.Errorf("not your task")
	}
	if task.Status != "pending_approval" && task.Status != "pending_scope_expansion" {
		return fmt.Errorf("task is not pending")
	}

	if err := h.st.UpdateTaskStatus(ctx, taskID, "denied"); err != nil {
		return err
	}

	h.updateNotificationMsg(ctx, "task", taskID, userID, "❌ <b>Denied</b> — task rejected.")
	h.decrementNotifierPolling(userID)
	h.publishTasksAndQueue(userID)

	if task.CallbackURL != nil && *task.CallbackURL != "" {
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, task.AgentID)
		go func() {
			_ = callback.DeliverResult(context.Background(), *task.CallbackURL, &callback.Payload{
				Type:   "task",
				TaskID: taskID,
				Status: "denied",
			}, cbKey)
		}()
	}
	return nil
}

// ExpandApproveByTaskID approves a pending scope expansion.
func (h *TasksHandler) ExpandApproveByTaskID(ctx context.Context, taskID, userID string) error {
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.UserID != userID {
		return fmt.Errorf("not your task")
	}
	if task.Status != "pending_scope_expansion" || task.PendingAction == nil {
		return fmt.Errorf("task has no pending scope expansion")
	}

	// Carry the expansion rationale into the action for intent verification.
	if task.PendingReason != "" {
		task.PendingAction.ExpansionRationale = task.PendingReason
	}

	newActions := append(task.AuthorizedActions, *task.PendingAction)
	expiresAt := time.Now().UTC().Add(time.Duration(task.ExpiresInSeconds) * time.Second)

	if err := h.st.UpdateTaskActions(ctx, taskID, newActions, expiresAt); err != nil {
		return err
	}

	h.updateNotificationMsg(ctx, "task", taskID, userID, "✅ <b>Scope expanded</b>")
	h.publishTasksAndQueue(userID)

	if task.CallbackURL != nil && *task.CallbackURL != "" {
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, task.AgentID)
		go func() {
			_ = callback.DeliverResult(context.Background(), *task.CallbackURL, &callback.Payload{
				Type:   "task",
				TaskID: taskID,
				Status: "scope_expanded",
			}, cbKey)
		}()
	}
	return nil
}

// ExpandDenyByTaskID denies a pending scope expansion.
func (h *TasksHandler) ExpandDenyByTaskID(ctx context.Context, taskID, userID string) error {
	task, err := h.st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.UserID != userID {
		return fmt.Errorf("not your task")
	}
	if task.Status != "pending_scope_expansion" {
		return fmt.Errorf("task has no pending scope expansion")
	}

	newStatus := "active"
	if task.ExpiresAt != nil && time.Now().After(*task.ExpiresAt) {
		newStatus = "expired"
	}

	exp := time.Now().UTC()
	if task.ExpiresAt != nil {
		exp = *task.ExpiresAt
	}
	if err := h.st.UpdateTaskActions(ctx, taskID, task.AuthorizedActions, exp); err != nil {
		return err
	}
	if newStatus != "active" {
		_ = h.st.UpdateTaskStatus(ctx, taskID, newStatus)
	}

	h.updateNotificationMsg(ctx, "task", taskID, userID, "❌ <b>Scope expansion denied</b>")
	h.decrementNotifierPolling(userID)
	h.publishTasksAndQueue(userID)

	if task.CallbackURL != nil && *task.CallbackURL != "" {
		cbKey, _ := h.st.GetAgentCallbackSecret(ctx, task.AgentID)
		go func() {
			_ = callback.DeliverResult(context.Background(), *task.CallbackURL, &callback.Payload{
				Type:   "task",
				TaskID: taskID,
				Status: "scope_expansion_denied",
			}, cbKey)
		}()
	}
	return nil
}

// decrementNotifierPolling calls DecrementPolling on the notifier if it supports it.
func (h *TasksHandler) decrementNotifierPolling(userID string) {
	if h.notifier == nil {
		return
	}
	if pd, ok := h.notifier.(notify.PollingDecrementer); ok {
		pd.DecrementPolling(userID)
	}
}

// updateNotificationMsg updates the Telegram message for a target
// using the notification_messages table.
func (h *TasksHandler) updateNotificationMsg(ctx context.Context, targetType, targetID, userID, text string) {
	if h.notifier == nil {
		return
	}
	msgID, err := h.st.GetNotificationMessage(ctx, targetType, targetID, "telegram")
	if err != nil {
		return
	}
	if err := h.notifier.UpdateMessage(ctx, userID, msgID, text); err != nil {
		h.logger.Warn("telegram message update failed", "err", err, "target_type", targetType, "target_id", targetID)
	}
}

// publishTasksAndQueue publishes SSE events for tasks and queue changes.
func (h *TasksHandler) publishTasksAndQueue(userID string) {
	if h.eventHub == nil {
		return
	}
	h.eventHub.Publish(userID, events.Event{Type: "tasks"})
	h.eventHub.Publish(userID, events.Event{Type: "queue"})
}

// ── Revoke ────────────────────────────────────────────────────────────────────

// Revoke cancels an active (typically standing) task.
//
// POST /api/tasks/{id}/revoke
// Auth: user JWT
func (h *TasksHandler) Revoke(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := middleware.UserFromContext(ctx)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	taskID := r.PathValue("id")
	if err := h.st.RevokeTask(ctx, taskID, user.ID); err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "task not found or not active")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not revoke task")
		return
	}
	if err := h.st.DeleteChainFactsByTask(ctx, taskID); err != nil {
		h.logger.Warn("chain facts cleanup failed", "err", err, "task_id", taskID)
	}

	h.publishTasksAndQueue(user.ID)

	writeJSON(w, http.StatusOK, map[string]any{
		"task_id": taskID,
		"status":  "revoked",
	})
}

// serviceActivated checks whether a service (with alias) has been activated.
// Credential-free services check service_meta; credential-backed services check the vault.
// It requires an exact alias match — callers should use serviceNotActivatedResponse
// to produce a helpful error listing available connections when this returns false.
func (h *TasksHandler) serviceActivated(ctx context.Context, userID, serviceType, alias string, adapter adapters.Adapter) bool {
	if adapter.ValidateCredential(nil) == nil {
		_, err := h.st.GetServiceMeta(ctx, userID, serviceType, alias)
		return err == nil
	}
	vKey := h.adapterReg.VaultKeyWithAlias(serviceType, alias)
	_, err := h.vault.Get(ctx, userID, vKey)
	return err == nil
}

// ── Task scope check helper ───────────────────────────────────────────────────

// TaskScopeMatch describes whether a service/action is in a task's authorized actions.
type TaskScopeMatch struct {
	InScope       bool
	AutoExecute   bool
	MatchedAction *store.TaskAction
}

// CheckTaskScope checks if service/action is in the task's authorized actions.
// It matches both exact (with alias, e.g. "google.gmail:personal") and
// base service type (e.g. "google.gmail" matches any alias).
func CheckTaskScope(task *store.Task, serviceType, alias, action string) TaskScopeMatch {
	fullService := serviceType
	if alias != "" && alias != "default" {
		fullService = serviceType + ":" + alias
	}
	// First pass: look for an exact match on the full service (including alias).
	for i := range task.AuthorizedActions {
		a := &task.AuthorizedActions[i]
		if a.Service == fullService && (a.Action == action || a.Action == "*") {
			return TaskScopeMatch{InScope: true, AutoExecute: a.AutoExecute, MatchedAction: a}
		}
	}
	// Second pass: fall back to base service type only when the request
	// didn't include an alias or no exact match was found.
	if fullService != serviceType {
		for i := range task.AuthorizedActions {
			a := &task.AuthorizedActions[i]
			if a.Service == serviceType && (a.Action == action || a.Action == "*") {
				return TaskScopeMatch{InScope: true, AutoExecute: a.AutoExecute, MatchedAction: a}
			}
		}
	}
	return TaskScopeMatch{InScope: false}
}
