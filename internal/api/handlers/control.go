package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/events"
	"github.com/clawvisor/clawvisor/internal/intent"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/google/uuid"
)

type LLMControlHandler struct {
	BaseURL        string
	Store          store.Store
	TaskCheckouts  llmproxy.TaskCheckoutStore
	Audit          *llmproxy.AuditEmitter
	ScriptSessions llmproxy.ScriptSessionCache
	// IntentVerifier is consulted before minting a script session so
	// that the derived capability (placeholder + host + methods +
	// path prefixes) is checked against the active task scope. nil
	// falls back to a deterministic check only.
	IntentVerifier   intent.Verifier
	PendingApprovals llmproxy.PendingApprovalCache
	EventHub         events.EventHub
}

func NewLLMControlHandler(baseURL string) *LLMControlHandler {
	return &LLMControlHandler{BaseURL: baseURL}
}

func (h *LLMControlHandler) Capabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"control_host": "https://clawvisor.local",
		"direct_url":   strings.TrimRight(h.BaseURL, "/") + "/api/control/skill",
		"base_path":    "/control",
		"direct_path":  "/api/control",
		"note":         "clawvisor.local/control is synthetic and is handled inside proxy-lite tool calls. Use direct_url when fetching documentation outside a proxy-lite tool call.",
		"endpoints": []map[string]string{
			{"method": "GET", "path": "/control/skill", "purpose": "Return schemas and examples for Clawvisor control-plane calls."},
			{"method": "GET", "path": "/control/vault/items", "purpose": "List available vault item IDs that can be requested in a task."},
			{"method": "GET", "path": "/control/vault/items/{id}", "purpose": "Return compact, non-secret metadata for one vault item ID."},
			{"method": "GET", "path": "/control/tasks", "purpose": "List this agent's active tasks and current focus."},
			{"method": "POST", "path": "/control/tasks", "purpose": "Create a task approval request for future tool use."},
			{"method": "POST", "path": "/control/task/checkout", "purpose": "Set the current task focus for disambiguating later tool use."},
			{"method": "GET", "path": "/control/tasks/{id}", "purpose": "Fetch task status."},
			{"method": "POST", "path": "/control/tasks/{id}/expand", "purpose": "Request additional scope for an existing task."},
		},
	})
}

func (h *LLMControlHandler) Skill(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        "clawvisor-control",
		"description": "Use this control plane to ask the user for permission before attempting tool work that may be blocked.",
		"base_url":    "https://clawvisor.local",
		"direct_docs": strings.TrimRight(h.BaseURL, "/") + "/api/control/skill",
		"rules": []string{
			"clawvisor.local is synthetic. Do not expect DNS lookup for the naked domain to work.",
			"Use direct_docs for reading these schemas from a shell.",
			"Proxy-lite sessions can request task permission through the synthetic Clawvisor control endpoint at https://clawvisor.local/control/tasks.",
			"Clawvisor handles the synthetic URL before the shell command runs.",
			"Before creating a task, tell me that you are requesting a Clawvisor task and that I will need to approve it.",
			"Creating or expanding a task requests permission. It does not grant permission until I approve it.",
			"When multiple active tasks exist, use /control/task/checkout to select the task you are actively working on. Checkout is only a routing preference; it does not grant new permission.",
			"If you already have an autovault_... placeholder, do not call /control/vault/items just to identify it. Create the task for the intended API call, omit required_credentials, and use the placeholder directly after approval.",
			"Use /control/vault/items only when you need Clawvisor to mint a new placeholder from an available vault item. The response is just IDs; do not pipe or shell-filter it. If you need non-secret metadata for one item, fetch /control/vault/items/{id}.",
			"Prefer expected_tools for harness tools such as bash, exec_command, WebFetch, Read, Write, or Edit.",
			"When a task needs a new credential placeholder, include required_credentials with a concrete vault_item_id or vault_item_handle plus a specific why. Do not ask the user to paste raw secrets into chat.",
			"Task lifetime defaults to session. Use lifetime=session with expires_in_seconds for temporary permission; use lifetime=standing only when the user explicitly wants persistent permission, and never combine standing with expires_in_seconds.",
		},
		"list_tasks": map[string]any{
			"method":  "GET",
			"path":    "/control/tasks",
			"purpose": "List active tasks for this agent, including the currently checked-out task focus.",
		},
		"create_task": map[string]any{
			"method": "POST",
			"path":   "/control/tasks",
			"body": map[string]any{
				"purpose": "Briefly explain the user-visible work you need permission to do.",
				"expected_tools": []map[string]any{{
					"tool_name": "bash",
					"why":       "Describe the exact command pattern or operation you need, e.g. run curl to POST JSON to https://api.example.com/widgets.",
				}},
				"required_credentials": []map[string]any{{
					"vault_item_id": "google.gmail",
					"why":           "Use the selected Gmail credential to send the requested message.",
				}},
				"intent_verification_mode": "strict",
				"lifetime":                 "session",
				"expires_in_seconds":       600,
			},
		},
		"expand_task": map[string]any{
			"method": "POST",
			"path":   "/control/tasks/{id}/expand",
			"body": map[string]any{
				"service":      "github",
				"action":       "create_issue",
				"auto_execute": true,
				"reason":       "Explain why the existing task scope is insufficient.",
			},
		},
		"checkout_task": map[string]any{
			"method": "POST",
			"path":   "/control/task/checkout",
			"body": map[string]any{
				"task_id": "The active task id to prefer for later tool calls.",
			},
		},
	})
}

func (h *LLMControlHandler) Failure(w http.ResponseWriter, r *http.Request) {
	reason := strings.TrimSpace(r.URL.Query().Get("reason"))
	if reason == "" {
		reason = "malformed_control_command"
	}
	var body struct {
		OriginalTool    string `json:"original_tool,omitempty"`
		OriginalCommand string `json:"original_command,omitempty"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error":            "control_command_rejected",
		"reason":           reason,
		"message":          "Clawvisor control-plane calls must be a single foreground curl to the synthetic control URL, with no pipes, subshells, redirects to output files, or extra shell commands.",
		"original_tool":    body.OriginalTool,
		"original_command": body.OriginalCommand,
		"next_step":        "Retry the control-plane request as one plain curl. For credential discovery, run: curl -sS 'https://clawvisor.local/control/vault/items'. If you already have an autovault_ placeholder, create the task instead of rediscovering vault items.",
	})
}

type controlTaskSummary struct {
	ID                string                   `json:"id"`
	Purpose           string                   `json:"purpose"`
	Status            string                   `json:"status"`
	Lifetime          string                   `json:"lifetime,omitempty"`
	ExpiresAt         *time.Time               `json:"expires_at,omitempty"`
	AuthorizedActions []store.TaskAction       `json:"authorized_actions,omitempty"`
	PlannedCalls      []store.PlannedCall      `json:"planned_calls,omitempty"`
	ExpectedTools     json.RawMessage          `json:"expected_tools,omitempty"`
	ExpectedEgress    json.RawMessage          `json:"expected_egress,omitempty"`
	Placeholders      []controlTaskPlaceholder `json:"placeholders,omitempty"`
	CheckedOut        bool                     `json:"checked_out"`
}

// controlTaskPlaceholder is the per-task autovault_* handle list returned
// alongside a discovered task, so an agent that finds a credentialed
// standing task from a prior conversation can use the placeholder
// directly instead of having to re-POST the task to mint a fresh one.
// The placeholder itself is not a secret — Clawvisor substitutes the
// real credential at proxy time — so it is safe to surface here on the
// same channel that already returns task scope.
type controlTaskPlaceholder struct {
	Placeholder string     `json:"placeholder"`
	ServiceID   string     `json:"service_id,omitempty"`
	VaultItemID string     `json:"vault_item_id,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

func (h *LLMControlHandler) ListTasks(w http.ResponseWriter, r *http.Request) {
	agent := middleware.AgentFromContext(r.Context())
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":   "unauthorized",
			"message": "missing agent context",
		})
		return
	}
	if h.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":   "task_list_unavailable",
			"message": "task store is not configured",
		})
		return
	}

	tasks, _, err := h.Store.ListTasks(r.Context(), agent.UserID, store.TaskFilter{Status: "active"})
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":   "task_list_failed",
			"message": "could not list active tasks",
		})
		return
	}

	// Pull live placeholders for this user once and bucket them by
	// task_id, filtered to the calling agent and to handles that are
	// still good (not revoked, not expired). Errors here are
	// non-fatal — the listing still returns task scope; the agent
	// just won't see placeholders this turn.
	placeholdersByTask := map[string][]controlTaskPlaceholder{}
	if phs, err := h.Store.ListRuntimePlaceholders(r.Context(), agent.UserID); err == nil {
		now := time.Now().UTC()
		for _, ph := range phs {
			if ph == nil || ph.TaskID == "" {
				continue
			}
			if ph.AgentID != "" && ph.AgentID != agent.ID {
				continue
			}
			if ph.RevokedAt != nil {
				continue
			}
			if ph.ExpiresAt != nil && !ph.ExpiresAt.After(now) {
				continue
			}
			placeholdersByTask[ph.TaskID] = append(placeholdersByTask[ph.TaskID], controlTaskPlaceholder{
				Placeholder: ph.Placeholder,
				ServiceID:   ph.ServiceID,
				VaultItemID: ph.VaultItemID,
				ExpiresAt:   ph.ExpiresAt,
			})
		}
	}

	checkoutID := ""
	checkoutUnavailable := false
	key := llmproxy.TaskCheckoutKey{UserID: agent.UserID, AgentID: agent.ID}
	if h.TaskCheckouts != nil {
		if checkout, ok, err := h.TaskCheckouts.Get(r.Context(), key); err != nil {
			checkoutUnavailable = true
		} else if ok {
			checkoutID = strings.TrimSpace(checkout.TaskID)
		}
	}

	now := time.Now()
	summaries := make([]controlTaskSummary, 0, len(tasks))
	checkoutStillActive := checkoutID == ""
	for _, task := range tasks {
		if task == nil || task.AgentID != agent.ID || task.Status != "active" {
			continue
		}
		if task.ExpiresAt != nil && task.ExpiresAt.Before(now) {
			_ = h.Store.UpdateTaskStatus(r.Context(), task.ID, "expired")
			continue
		}
		checkedOut := task.ID == checkoutID
		if checkedOut {
			checkoutStillActive = true
		}
		summaries = append(summaries, controlTaskSummary{
			ID:                task.ID,
			Purpose:           task.Purpose,
			Status:            task.Status,
			Lifetime:          task.Lifetime,
			ExpiresAt:         task.ExpiresAt,
			AuthorizedActions: task.AuthorizedActions,
			PlannedCalls:      task.PlannedCalls,
			ExpectedTools:     task.ExpectedTools,
			ExpectedEgress:    task.ExpectedEgress,
			Placeholders:      placeholdersByTask[task.ID],
			CheckedOut:        checkedOut,
		})
	}
	if !checkoutStillActive && h.TaskCheckouts != nil {
		_ = h.TaskCheckouts.Clear(r.Context(), key)
		checkoutID = ""
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"active_task_id":       checkoutID,
		"checkout_unavailable": checkoutUnavailable,
		"total":                len(summaries),
		"tasks":                summaries,
		"next_step":            "If a listed task's expected_tools, authorized_actions, and expected_egress already cover what you need, use it directly — do NOT POST a new task. Each task includes any minted autovault_* placeholders bound to it: use those handles verbatim in subsequent curls without re-creating the task. When multiple tasks match, POST /control/task/checkout with the target task_id to focus one (checkout is routing only; it does not grant new permission). If nothing here matches, POST /control/tasks for fresh approval.",
	})
}

func (h *LLMControlHandler) CheckoutTask(w http.ResponseWriter, r *http.Request) {
	agent := middleware.AgentFromContext(r.Context())
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":   "unauthorized",
			"message": "missing agent context",
		})
		return
	}
	if h.Store == nil || h.TaskCheckouts == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":   "task_checkout_unavailable",
			"message": "task checkout store is not configured",
		})
		return
	}
	var body struct {
		TaskID string `json:"task_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "invalid_json",
			"message": err.Error(),
		})
		return
	}
	taskID := strings.TrimSpace(body.TaskID)
	if taskID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "task_id_required",
			"message": "task_id is required",
		})
		return
	}
	task, err := h.Store.GetTask(r.Context(), taskID)
	if err != nil {
		status := http.StatusServiceUnavailable
		code := "task_lookup_failed"
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
			code = "task_not_found"
		}
		writeJSON(w, status, map[string]any{
			"error":   code,
			"message": "could not find an active task with that id for this agent",
		})
		return
	}
	if task.UserID != agent.UserID || task.AgentID != agent.ID || task.Status != "active" {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":   "task_not_active_for_agent",
			"message": "task_id must name an active task owned by this agent",
		})
		return
	}
	ttl := 24 * time.Hour
	if task.ExpiresAt != nil {
		untilExpiry := time.Until(*task.ExpiresAt)
		if untilExpiry <= 0 {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error":   "task_expired",
				"message": "task is expired and cannot be checked out",
			})
			return
		}
		ttl = untilExpiry
	}
	if err := h.TaskCheckouts.Set(r.Context(), llmproxy.TaskCheckoutKey{
		UserID:  agent.UserID,
		AgentID: agent.ID,
	}, task.ID, ttl); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":   "task_checkout_failed",
			"message": err.Error(),
		})
		return
	}
	if h.Audit != nil {
		h.Audit.LogEndpointCall(r.Context(), agent, uuid.NewString(), "clawvisor.control", "task.checkout", http.StatusOK, "allow", "checked_out", "", 0, map[string]any{
			"task_id": task.ID,
			"purpose": task.Purpose,
		}, llmproxy.EndpointCallExtras{TaskID: task.ID})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "checked_out",
		"task_id":    task.ID,
		"purpose":    task.Purpose,
		"expires_at": task.ExpiresAt,
		"message":    "Task is checked out as the current focus. Clawvisor will prefer it only when it is a valid match for later tool calls.",
		"next_step":  "Continue with the requested work using normal tool calls. Do not add task_id or extra fields to tool inputs.",
	})
}

var errSentinelHold = &llmproxy.PendingLiteApproval{ID: "err-sentinel"}

func (h *LLMControlHandler) WaitForApproval(w http.ResponseWriter, r *http.Request) {
	agent := middleware.AgentFromContext(r.Context())
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":   "unauthorized",
			"message": "missing agent context",
		})
		return
	}

	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "id_required",
			"message": "approval id or task id is required",
		})
		return
	}

	var pending *llmproxy.PendingLiteApproval
	var err error
	if h.PendingApprovals != nil {
		pending, err = h.PendingApprovals.PeekByID(r.Context(), id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error":   "internal_error",
				"message": "failed to peek pending approval cache: " + err.Error(),
			})
			return
		}
	}

	if pending != nil {
		if pending.UserID != agent.UserID || pending.AgentID != agent.ID {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error":   "forbidden",
				"message": "approval hold belongs to a different agent/user",
			})
			return
		}
	}

	taskID := id
	if pending != nil && pending.PendingTaskID != "" {
		taskID = pending.PendingTaskID
	}

	if h.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":   "store_unavailable",
			"message": "task store is not configured",
		})
		return
	}

	var task *store.Task
	if pending == nil || pending.PendingTaskID != "" {
		task, err = h.Store.GetTask(r.Context(), taskID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				if pending == nil {
					writeJSON(w, http.StatusNotFound, map[string]any{
						"error":   "not_found",
						"message": "approval or task not found",
					})
					return
				}
				writeDeniedJSON(w, nil)
				return
			} else {
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error":   "internal_error",
					"message": "failed to lookup task: " + err.Error(),
				})
				return
			}
		}
	}

	if task != nil {
		if task.UserID != agent.UserID || task.AgentID != agent.ID {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error":   "forbidden",
				"message": "task belongs to a different agent/user",
			})
			return
		}

		if task.Status != "pending_approval" {
			if task.Status == "active" {
				writeJSON(w, http.StatusOK, map[string]any{"status": "approved"})
				return
			}
			writeDeniedJSON(w, task)
			return
		}
	}

	timeout := longPollDeadline(r)

	if h.EventHub != nil {
		if task != nil {
			resolvedTask := events.WaitFor(r.Context(), h.EventHub, agent.UserID, timeout, []string{"tasks"}, func(c context.Context, evt *events.Event) (*store.Task, bool) {
				if evt != nil && evt.ID != "" && evt.ID != taskID {
					return nil, false
				}
				t, err := h.Store.GetTask(c, taskID)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return &store.Task{ID: taskID, Status: "deleted"}, true
					}
					return &store.Task{ID: taskID}, false
				}
				return t, t.Status != "pending_approval"
			})
			if resolvedTask.Status == "active" {
				writeJSON(w, http.StatusOK, map[string]any{"status": "approved"})
				return
			} else if resolvedTask.Status != "pending_approval" {
				if resolvedTask.Status == "deleted" {
					writeDeniedJSON(w, nil)
				} else {
					writeDeniedJSON(w, resolvedTask)
				}
				return
			}
		} else if pending != nil && h.PendingApprovals != nil {
			resolvedHold := events.WaitFor(r.Context(), h.EventHub, agent.UserID, timeout, []string{"audit", "queue"}, func(c context.Context, evt *events.Event) (*llmproxy.PendingLiteApproval, bool) {
				if evt != nil && evt.ID != "" && evt.ID != id && (pending.PendingTaskID == "" || evt.ID != pending.PendingTaskID) {
					return nil, false
				}
				p, err := h.PendingApprovals.PeekByID(c, id)
				if err != nil {
					return errSentinelHold, false
				}
				return p, p == nil
			})
			if resolvedHold == nil {
				writeJSON(w, http.StatusOK, map[string]any{"status": "approved"})
				return
			}
		}
	} else {
		deadline := time.Now().Add(timeout)
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for time.Now().Before(deadline) {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				if task != nil {
					t, err := h.Store.GetTask(r.Context(), taskID)
					if err != nil {
						if errors.Is(err, store.ErrNotFound) {
							writeDeniedJSON(w, nil)
							return
						}
					} else if t.Status != "pending_approval" {
						if t.Status == "active" {
							writeJSON(w, http.StatusOK, map[string]any{"status": "approved"})
							return
						}
						if t.Status == "deleted" {
							writeDeniedJSON(w, nil)
						} else {
							writeDeniedJSON(w, t)
						}
						return
					}
				} else if pending != nil && h.PendingApprovals != nil {
					p, err := h.PendingApprovals.PeekByID(r.Context(), id)
					if err == nil && p == nil {
						writeJSON(w, http.StatusOK, map[string]any{"status": "approved"})
						return
					}
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "pending",
		"retry_after": 1000,
	})
}

func writeDeniedJSON(w http.ResponseWriter, task *store.Task) {
	resp := map[string]any{
		"status": "denied",
	}
	if task != nil {
		reason := "user_rejected"
		switch task.Status {
		case "expired", "cancelled", "revoked", "completed", "pending_scope_expansion":
			reason = task.Status
		}
		resp["reason"] = reason

		if len(task.ApprovalRationale) > 0 {
			var rationale struct {
				Reason   string `json:"reason"`
				Message  string `json:"message"`
				Feedback string `json:"feedback"`
			}
			if err := json.Unmarshal(task.ApprovalRationale, &rationale); err == nil {
				if rationale.Reason != "" {
					resp["reason"] = rationale.Reason
				}
				if rationale.Message != "" {
					resp["message"] = rationale.Message
				}
				if rationale.Feedback != "" {
					resp["feedback"] = rationale.Feedback
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *LLMControlHandler) NotFound(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"error":   "control_endpoint_not_found",
		"path":    r.URL.Path,
		"message": "This Clawvisor control endpoint does not exist.",
		"available_endpoints": []string{
			"GET /control/skill",
			"GET /control/vault/items",
			"GET /control/vault/items/{id}",
			"GET /control/tasks",
			"POST /control/tasks",
			"POST /control/task/checkout",
			"GET /control/tasks/{id}",
			"POST /control/tasks/{id}/expand",
		},
		"hint": "For new placeholders, /control/vault/items returns the complete list of vault item IDs. If you already have an autovault_ placeholder, create the task and use that placeholder after approval.",
	})
}
