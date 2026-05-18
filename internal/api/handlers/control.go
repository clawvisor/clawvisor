package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
)

type LLMControlHandler struct {
	BaseURL string
}

func NewLLMControlHandler(baseURL string) *LLMControlHandler {
	return &LLMControlHandler{BaseURL: baseURL}
}

func (h *LLMControlHandler) Capabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"control_host": "https://clawvisor.local",
		"direct_url":   strings.TrimRight(h.BaseURL, "/") + "/control/help",
		"base_path":    "/control",
		"note":         "clawvisor.local is synthetic and is handled inside proxy-lite tool calls. Use direct_url when fetching documentation from a shell.",
		"endpoints": []map[string]string{
			{"method": "GET", "path": "/control/help", "purpose": "Return a router index for Clawvisor control-plane help."},
			{"method": "GET", "path": "/control/help/{topic}", "purpose": "Return focused help for tasks, credentials, tools, legacy adapters, errors, or bug reporting."},
			{"method": "GET", "path": "/control/skill", "purpose": "Legacy alias for task schema help."},
			{"method": "GET", "path": "/control/vault/items", "purpose": "List available vault item IDs that can be requested in a task."},
			{"method": "GET", "path": "/control/vault/items/{id}", "purpose": "Return compact, non-secret metadata for one vault item ID."},
			{"method": "POST", "path": "/control/tasks", "purpose": "Create a task approval request for future tool use."},
			{"method": "GET", "path": "/control/tasks?status=active", "purpose": "List this agent's own active tasks."},
			{"method": "GET", "path": "/control/tasks/{id}", "purpose": "Fetch task status."},
			{"method": "POST", "path": "/control/tasks/{id}/expand", "purpose": "Request additional scope for an existing task."},
		},
	})
}

func (h *LLMControlHandler) Skill(w http.ResponseWriter, r *http.Request) {
	h.writeTasksHelp(w)
}

func (h *LLMControlHandler) Help(w http.ResponseWriter, r *http.Request) {
	base := "https://clawvisor.local"
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        "clawvisor-control-help",
		"description": "Router for focused Clawvisor proxy-lite control-plane help. Fetch only the topic you need.",
		"base_url":    base,
		"direct_docs": strings.TrimRight(h.BaseURL, "/") + "/control/help",
		"topics": []map[string]string{
			{"path": "/control/help/tasks", "purpose": "Create, inspect, and expand task-scoped approvals."},
			{"path": "/control/help/credentials", "purpose": "Use vault placeholders and request credential placeholders safely."},
			{"path": "/control/help/tools", "purpose": "Describe expected_tools and allowed-without-task behavior."},
			{"path": "/control/help/legacy-adapters", "purpose": "Use the classic service catalog and gateway adapter API when needed."},
			{"path": "/control/help/errors", "purpose": "Recover from common control-plane and gateway errors."},
			{"path": "/control/help/bug-reporting", "purpose": "Report confusing or incorrect Clawvisor behavior."},
		},
		"live_state": []map[string]string{
			{"method": "GET", "path": "/control/capabilities", "purpose": "Show available control-plane endpoints."},
			{"method": "GET", "path": "/control/tasks?status=active", "purpose": "List this agent's own active tasks."},
			{"method": "GET", "path": "/control/vault/items", "purpose": "List available non-secret vault item IDs."},
		},
	})
}

func (h *LLMControlHandler) HelpTopic(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimSpace(r.PathValue("topic")) {
	case "tasks":
		h.writeTasksHelp(w)
	case "credentials":
		h.writeCredentialsHelp(w)
	case "tools":
		h.writeToolsHelp(w)
	case "legacy-adapters":
		h.writeLegacyAdaptersHelp(w)
	case "errors":
		h.writeErrorsHelp(w)
	case "bug-reporting":
		h.writeBugReportingHelp(w)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "help_topic_not_found",
			"available_topics": []string{
				"tasks",
				"credentials",
				"tools",
				"legacy-adapters",
				"errors",
				"bug-reporting",
			},
		})
	}
}

func (h *LLMControlHandler) writeTasksHelp(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        "clawvisor-control-tasks",
		"description": "Create and manage proxy-lite task-scoped approvals.",
		"base_url":    "https://clawvisor.local",
		"direct_docs": strings.TrimRight(h.BaseURL, "/") + "/control/help/tasks",
		"rules": []string{
			"clawvisor.local is synthetic. Do not expect DNS lookup for the naked domain to work.",
			"Use direct_docs for reading these schemas from a shell.",
			"Proxy-lite sessions can request task permission through the synthetic Clawvisor control endpoint at https://clawvisor.local/control/tasks.",
			"Clawvisor handles the synthetic URL before the shell command runs.",
			"Before creating a task, tell me that you are requesting a Clawvisor task and that I will need to approve it.",
			"Creating or expanding a task requests permission. It does not grant permission until I approve it.",
			"Prefer expected_tools for harness tools such as bash, exec_command, WebFetch, Read, Write, or Edit.",
			"Task lifetime defaults to session. Use lifetime=session with expires_in_seconds for temporary permission; use lifetime=standing only when the user explicitly wants persistent permission, and never combine standing with expires_in_seconds.",
			"Standing tasks persist until the user revokes them. Use session tasks for normal one-off work.",
			"Use task expansion when an already-approved task needs additional scope.",
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
		"inspect_tasks": map[string]any{
			"method":  "GET",
			"path":    "/control/tasks?status=active",
			"purpose": "List this agent's own active tasks before creating a duplicate standing or session task.",
		},
	})
}

func (h *LLMControlHandler) writeCredentialsHelp(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        "clawvisor-control-credentials",
		"description": "Use Clawvisor vault placeholders without exposing raw secrets.",
		"base_url":    "https://clawvisor.local",
		"rules": []string{
			"Values beginning with autovault_ are placeholders, not raw credentials.",
			"Use an existing autovault_ placeholder directly after the relevant task is approved; Clawvisor swaps it for the real secret at proxy time.",
			"If you already have a placeholder, do not call /control/vault/items just to identify it. Create a task for the intended API call and omit required_credentials.",
			"Use GET /control/vault/items only when you need Clawvisor to mint a new placeholder from an available vault item.",
			"If you need one credential, include required_credentials with vault_item_id or vault_item_handle and a concrete why.",
			"Never ask the user to paste raw secrets into chat.",
		},
		"endpoints": []map[string]string{
			{"method": "GET", "path": "/control/vault/items", "purpose": "List available non-secret vault item IDs."},
			{"method": "GET", "path": "/control/vault/items/{id}", "purpose": "Return compact non-secret metadata for one vault item."},
		},
	})
}

func (h *LLMControlHandler) writeToolsHelp(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        "clawvisor-control-tools",
		"description": "Describe tool scope in task requests.",
		"rules": []string{
			"Use expected_tools for harness tools such as bash, exec_command, WebFetch, Read, Write, Edit, plan-mode tools, and task-management tools.",
			"List plausible tools up front. Include verification/read commands in the same tool why when they are part of the workflow.",
			"Single-step, non-destructive inspection may be allowed without a task only when active policy allowlists that tool.",
			"If a tool is not allowlisted and the work is non-trivial, create a task before using it.",
		},
		"expected_tools_example": []map[string]string{
			{"tool_name": "Bash", "why": "Run curl to call the approved API and run ls/wc checks to verify local output."},
			{"tool_name": "Read", "why": "Read local files needed to complete the approved task."},
		},
	})
}

func (h *LLMControlHandler) writeLegacyAdaptersHelp(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        "clawvisor-legacy-adapters",
		"description": "Use the classic Clawvisor service catalog and gateway API when a workflow needs managed service adapters instead of proxy-lite tool controls.",
		"rules": []string{
			"Fetch GET /api/skill/catalog with the agent token to see activated services, actions, parameters, and restrictions.",
			"Create a task with POST /api/tasks before POST /api/gateway/request.",
			"Use service IDs exactly as shown in the catalog, including account aliases such as google.gmail:work.",
			"Use auto_execute=false for destructive actions that should still require per-request approval.",
			"Set context.data_origin when acting on external content such as email, web pages, or GitHub issues.",
		},
		"endpoints": []map[string]string{
			{"method": "GET", "path": "/api/skill/catalog", "purpose": "Personalized service/action catalog."},
			{"method": "POST", "path": "/api/tasks", "purpose": "Create adapter task scope."},
			{"method": "POST", "path": "/api/gateway/request", "purpose": "Execute one adapter action under an approved task."},
			{"method": "POST", "path": "/api/gateway/batch", "purpose": "Execute independent adapter actions concurrently."},
		},
	})
}

func (h *LLMControlHandler) writeErrorsHelp(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        "clawvisor-control-errors",
		"description": "Common statuses and recovery actions.",
		"errors": []map[string]string{
			{"code": "control_command_rejected", "meaning": "The control curl used pipes, redirects, extra commands, dynamic URLs, or another unsafe shape.", "recovery": "Retry as one foreground curl to https://clawvisor.local/control/... with JSON via --data @-."},
			{"code": "task_denied", "meaning": "The user denied the task.", "recovery": "Stop or ask the user how to proceed; do not work around the denial."},
			{"code": "task_expired", "meaning": "The session task TTL elapsed.", "recovery": "Create a new task or expand the existing one when supported."},
			{"code": "scope_mismatch", "meaning": "The requested action is outside the approved task.", "recovery": "Request task expansion with a specific reason."},
			{"code": "missing_credential", "meaning": "The task needs a vault placeholder not currently granted.", "recovery": "Fetch /control/vault/items and create or expand a task with required_credentials."},
			{"code": "blocked_or_restricted", "meaning": "Runtime policy or intent verification blocked the action.", "recovery": "Report the block to the user; do not bypass it."},
		},
	})
}

func (h *LLMControlHandler) writeBugReportingHelp(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        "clawvisor-bug-reporting",
		"description": "Report confusing or incorrect Clawvisor behavior.",
		"when_to_report": []string{
			"Clawvisor blocked a legitimate request.",
			"Clawvisor approved or suggested something unsafe.",
			"An error or instruction was confusing.",
			"A task approval flow behaved unexpectedly.",
		},
		"report_shape": map[string]string{
			"description": "What happened, what you expected, and why it mattered.",
			"request_id":  "Optional request ID, if available.",
			"task_id":     "Optional task ID, if available.",
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

func (h *LLMControlHandler) NotFound(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"error":   "control_endpoint_not_found",
		"path":    r.URL.Path,
		"message": "This Clawvisor control endpoint does not exist.",
		"available_endpoints": []string{
			"GET /control/skill",
			"GET /control/help",
			"GET /control/help/{topic}",
			"GET /control/vault/items",
			"GET /control/vault/items/{id}",
			"POST /control/tasks",
			"GET /control/tasks?status=active",
			"GET /control/tasks/{id}",
			"POST /control/tasks/{id}/expand",
		},
		"hint": "For new placeholders, /control/vault/items returns the complete list of vault item IDs. If you already have an autovault_ placeholder, create the task and use that placeholder after approval.",
	})
}
