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
		"direct_url":   strings.TrimRight(h.BaseURL, "/") + "/control/skill",
		"base_path":    "/control",
		"note":         "clawvisor.local is synthetic and is handled inside proxy-lite tool calls. Use direct_url when fetching documentation from a shell.",
		"endpoints": []map[string]string{
			{"method": "GET", "path": "/control/skill", "purpose": "Return schemas and examples for Clawvisor control-plane calls."},
			{"method": "GET", "path": "/control/vault/items", "purpose": "List available vault item IDs that can be requested in a task."},
			{"method": "POST", "path": "/control/tasks?wait=true&timeout=120", "purpose": "Create a task approval request for future tool use and wait for my decision."},
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
		"direct_docs": strings.TrimRight(h.BaseURL, "/") + "/control/skill",
		"rules": []string{
			"clawvisor.local is synthetic. Do not expect DNS lookup for the naked domain to work.",
			"Use direct_docs for reading these schemas from a shell.",
			"Proxy-lite sessions can request task permission through the synthetic Clawvisor control endpoint at https://clawvisor.local/control/tasks?wait=true&timeout=120.",
			"Clawvisor handles the synthetic URL before the shell command runs.",
			"Before creating a task, tell me that you are requesting a Clawvisor task and that I will need to approve it.",
			"Creating or expanding a task requests permission. It does not grant permission until I approve it.",
			"Use /control/vault/items to list available vault item IDs when your task needs a credential and the prompt did not include the item you need. The response is just IDs; do not pipe or shell-filter it.",
			"Use wait=true when creating a task so the command blocks until I approve or deny it.",
			"Prefer expected_tools_json for harness tools such as bash, exec_command, WebFetch, Read, Write, or Edit.",
			"When a task needs a credential, include required_credentials_json with a concrete vault_item_id or vault_item_handle plus a specific why. Do not ask the user to paste raw secrets into chat.",
		},
		"create_task": map[string]any{
			"method": "POST",
			"path":   "/control/tasks?wait=true&timeout=120",
			"body": map[string]any{
				"purpose": "Briefly explain the user-visible work you need permission to do.",
				"expected_tools_json": []map[string]any{{
					"tool_name": "bash",
					"why":       "Describe the exact command pattern or operation you need, e.g. run curl to POST JSON to https://api.example.com/widgets.",
				}},
				"required_credentials_json": []map[string]any{{
					"vault_item_id": "google.gmail",
					"why":           "Use the selected Gmail credential to send the requested message.",
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
		"next_step":        "Retry the control-plane request as one plain curl. For credential discovery, run: curl -sS 'https://clawvisor.local/control/vault/items'",
	})
}
