package handlers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/callback"
	"github.com/clawvisor/clawvisor/internal/events"
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
	st         store.Store
	vault      vault.Vault
	adapterReg *adapters.Registry
	notifier   notify.Notifier
	cfg        config.Config
	logger     *slog.Logger
	baseURL    string
	eventHub   *events.Hub
	assessor   taskrisk.Assessor
}

func NewTasksHandler(
	st store.Store,
	v vault.Vault,
	adapterReg *adapters.Registry,
	notifier notify.Notifier,
	cfg config.Config,
	logger *slog.Logger,
	baseURL string,
	eventHub *events.Hub,
	assessor taskrisk.Assessor,
) *TasksHandler {
	return &TasksHandler{
		st: st, vault: v, adapterReg: adapterReg, notifier: notifier, cfg: cfg, logger: logger, baseURL: baseURL,
		eventHub: eventHub, assessor: assessor,
	}
}

// ── Create ────────────────────────────────────────────────────────────────────

type createTaskRequest struct {
	Purpose           string             `json:"purpose"`
	AuthorizedActions []store.TaskAction `json:"authorized_actions"`
	ExpiresInSeconds  int                `json:"expires_in_seconds"`
	CallbackURL       string             `json:"callback_url"`
	Lifetime          string             `json:"lifetime"` // "session" (default) or "standing"
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
			adapter, ok := h.adapterReg.Get(serviceType)
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
				writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
					fmt.Sprintf("service %q is not activated — connect it in the Clawvisor dashboard first", a.Service))
				return
			}
		}
		if a.AutoExecute && RequiresHardcodedApproval(a.Service, a.Action) {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
				fmt.Sprintf("action %s:%s has hardcoded approval — auto_execute must be false", a.Service, a.Action))
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

	if err := h.st.CreateTask(ctx, task); err != nil {
		h.logger.Warn("create task failed", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create task")
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
			TaskID:     task.ID,
			UserID:     agent.UserID,
			AgentName:  agent.Name,
			Purpose:    req.Purpose,
			Actions:    req.AuthorizedActions,
			ApproveURL: approveURL,
			DenyURL:    denyURL,
			ExpiresIn:  expiresInStr,
		}); err != nil {
			h.logger.Warn("failed to send task approval notification", "task_id", task.ID, "err", err)
		} else if msgID != "" {
			_ = h.st.SaveNotificationMessage(ctx, "task", task.ID, "telegram", msgID)
		}
	}

	h.publishTasksAndQueue(agent.UserID)

	writeJSON(w, http.StatusCreated, map[string]any{
		"task_id": task.ID,
		"status":  "pending_approval",
		"message": "Task approval requested. Waiting for human review.",
	})
}

// ── Get ───────────────────────────────────────────────────────────────────────

// Get returns task details. Agent must own the task via agent's user_id.
//
// GET /api/tasks/{id}
// Auth: agent bearer token
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

	sanitizeTaskForResponse(task)
	writeJSON(w, http.StatusOK, task)
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

	tasks, total, err := h.st.ListTasks(ctx, user.ID, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list tasks")
		return
	}
	if tasks == nil {
		tasks = []*store.Task{}
	}
	for _, t := range tasks {
		sanitizeTaskForResponse(t)
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
func sanitizeTaskForResponse(t *store.Task) {
	if t.Lifetime == "standing" {
		t.ExpiresAt = nil
		t.ExpiresInSeconds = 0
		return
	}
	if t.Status == "active" && t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now()) {
		t.Status = "expired"
	}
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
		adapter, ok := h.adapterReg.Get(serviceType)
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
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
				fmt.Sprintf("service %q is not activated — connect it in the Clawvisor dashboard first", req.Service))
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

	h.publishTasksAndQueue(user.ID)

	writeJSON(w, http.StatusOK, map[string]any{
		"task_id": taskID,
		"status":  "revoked",
	})
}

// serviceActivated checks whether a service (with alias) has been activated.
// Credential-free services check service_meta; credential-backed services check the vault.
func (h *TasksHandler) serviceActivated(ctx context.Context, userID, serviceType, alias string, adapter adapters.Adapter) bool {
	if adapter.ValidateCredential(nil) == nil {
		// Credential-free: check service_meta.
		_, err := h.st.GetServiceMeta(ctx, userID, serviceType, alias)
		return err == nil
	}
	// Credential-backed: check vault.
	vKey := vaultKeyForServiceAlias(serviceType, alias)
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
	for i := range task.AuthorizedActions {
		a := &task.AuthorizedActions[i]
		if (a.Service == fullService || a.Service == serviceType) && (a.Action == action || a.Action == "*") {
			return TaskScopeMatch{InScope: true, AutoExecute: a.AutoExecute, MatchedAction: a}
		}
	}
	return TaskScopeMatch{InScope: false}
}
