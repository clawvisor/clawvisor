package handlers

import "net/http"

type LLMControlHandler struct {
	BaseURL string
}

func NewLLMControlHandler(baseURL string) *LLMControlHandler {
	return &LLMControlHandler{BaseURL: baseURL}
}

func (h *LLMControlHandler) Capabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"control_host": "https://clawvisor.local",
		"base_path":    "/control",
		"endpoints": []map[string]string{
			{"method": "GET", "path": "/control/skill", "purpose": "Return schemas and examples for Clawvisor control-plane calls."},
			{"method": "POST", "path": "/control/tasks", "purpose": "Create a task approval request for future tool use."},
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
		"rules": []string{
			"Use the synthetic https://clawvisor.local/control URLs exactly; Clawvisor rewrites them to the local daemon.",
			"Creating or expanding a task requests permission. It does not grant permission until the user approves it.",
			"Prefer expected_tools_json for harness tools such as bash, exec_command, WebFetch, Read, Write, or Edit.",
		},
		"create_task": map[string]any{
			"method": "POST",
			"path":   "/control/tasks",
			"body": map[string]any{
				"purpose": "Briefly explain the user-visible work you need permission to do.",
				"expected_tools_json": []map[string]any{{
					"tool_name": "bash",
					"why":       "Run curl to POST a task request to Clawvisor.",
				}},
				"intent_verification_mode": "strict",
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
	})
}
