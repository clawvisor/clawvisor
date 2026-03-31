package mcp

import "encoding/json"

// Tool is an MCP tool definition with JSON Schema input.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ToolResult is returned from a tool/call invocation.
type ToolResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ToolContent is one piece of tool output.
type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toolDefs returns the static list of MCP tools exposed by Clawvisor.
func toolDefs() []Tool {
	return []Tool{
		{
			Name:        "fetch_catalog",
			Description: "Fetch the service catalog. Returns an overview of all activated services with compact parameter signatures. Pass a service ID to get detailed parameter docs for that service.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"service":{"type":"string","description":"Optional service ID (e.g. google.gmail) to get detailed parameter documentation for a single service"}}}`),
		},
		{
			Name:        "create_task",
			Description: "Create a new task for scoped authorization. Use wait=true (recommended) to block until the user approves or denies.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"purpose": {"type": "string", "description": "Human-readable description of what this task will do"},
					"authorized_actions": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"service": {"type": "string", "description": "Service ID (e.g. google.gmail, github)"},
								"action": {"type": "string", "description": "Action name or * for all"},
								"auto_execute": {"type": "boolean", "description": "Execute without per-request approval"},
								"expected_use": {"type": "string", "description": "Optional explanation of intended use"}
							},
							"required": ["service", "action"]
						},
						"description": "Actions this task is authorized to perform"
					},
					"expires_in_seconds": {"type": "integer", "description": "Session task expiry in seconds (default 1800)"},
					"lifetime": {"type": "string", "enum": ["session", "standing"], "description": "Task lifetime: session (expires) or standing (no expiry)"},
					"wait": {"type": "boolean", "description": "Block until the task is approved or denied (default true)"},
					"timeout": {"type": "integer", "description": "Long-poll timeout in seconds (default 120, max 120)"}
				},
				"required": ["purpose", "authorized_actions"]
			}`),
		},
		{
			Name:        "get_task",
			Description: "Get the current status and details of a task. Use wait=true to long-poll until the task is approved or denied instead of returning immediately.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"task_id": {"type": "string", "description": "The task ID to look up"},
					"wait": {"type": "boolean", "description": "Long-poll until the task leaves pending state (default false)"},
					"timeout": {"type": "integer", "description": "Long-poll timeout in seconds (default 120, max 120)"}
				},
				"required": ["task_id"]
			}`),
		},
		{
			Name:        "complete_task",
			Description: "Mark a task as completed when you are done with it.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"task_id": {"type": "string", "description": "The task ID to complete"}
				},
				"required": ["task_id"]
			}`),
		},
		{
			Name:        "expand_task",
			Description: "Request adding a new action to an existing task's scope. Use wait=true (recommended) to block until the user approves or denies.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"task_id": {"type": "string", "description": "The task ID to expand"},
					"service": {"type": "string", "description": "Service ID for the new action"},
					"action": {"type": "string", "description": "Action name for the new action"},
					"auto_execute": {"type": "boolean", "description": "Execute without per-request approval"},
					"reason": {"type": "string", "description": "Why this action is needed"},
					"wait": {"type": "boolean", "description": "Block until the expansion is approved or denied (default true)"},
					"timeout": {"type": "integer", "description": "Long-poll timeout in seconds (default 120, max 120)"}
				},
				"required": ["task_id", "service", "action", "reason"]
			}`),
		},
		{
			Name:        "gateway_request",
			Description: "Execute a service action through the gateway. Requires an active task with matching scope. Use wait=true (recommended) to block until approval and return the result in one call.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"service": {"type": "string", "description": "Service ID (e.g. google.gmail, github)"},
					"action": {"type": "string", "description": "Action to perform (e.g. send_email, list_repos)"},
					"params": {"type": "object", "description": "Action-specific parameters"},
					"reason": {"type": "string", "description": "Why this action is being performed"},
					"request_id": {"type": "string", "description": "Unique request ID for idempotency"},
					"task_id": {"type": "string", "description": "Task ID authorizing this request"},
					"context": {"type": "object", "description": "Optional context (source, data_origin, callback_url)"},
					"session_id": {"type": "string", "description": "Consistent UUID for chain context on standing tasks"},
					"wait": {"type": "boolean", "description": "Block until approved and return executed result (default true)"},
					"timeout": {"type": "integer", "description": "Long-poll timeout in seconds (default 120, max 120)"}
				},
				"required": ["service", "action", "params", "reason", "request_id", "task_id"]
			}`),
		},
		{
			Name:        "execute_request",
			Description: "Execute a previously approved gateway request and return the result. Use this when a gateway_request returned status=pending and has since been approved. Supports wait=true to block until approved.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"request_id": {"type": "string", "description": "The request ID from the original gateway_request"},
					"wait": {"type": "boolean", "description": "Block until the request is approved, then execute (default true)"},
					"timeout": {"type": "integer", "description": "Long-poll timeout in seconds (default 120, max 120)"}
				},
				"required": ["request_id"]
			}`),
		},
	}
}
