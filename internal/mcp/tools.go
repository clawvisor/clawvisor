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
					"expected_tools_json": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"tool_name": {"type": "string", "description": "Exact tool name expected at runtime"},
								"why": {"type": "string", "description": "Why this tool is expected to be used"},
								"input_shape": {"type": "object", "description": "Optional required/forbidden key shape for tool input"},
								"input_regex": {"type": "string", "description": "Optional regex compatibility escape hatch for matching serialized tool input"}
							},
							"required": ["tool_name", "why"]
						},
						"description": "Canonical v2 runtime tool expectations. Use this for proxy/runtime-backed tasks."
					},
					"expected_egress_json": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"host": {"type": "string", "description": "Expected egress host, optionally wildcarded as *.example.com"},
								"why": {"type": "string", "description": "Why this egress target is needed"},
								"method": {"type": "string", "description": "Optional HTTP method constraint"},
								"path": {"type": "string", "description": "Optional exact path match"},
								"path_regex": {"type": "string", "description": "Optional regex path match"},
								"query_shape": {"type": "object", "description": "Optional required/forbidden key shape for query params"},
								"body_shape": {"type": "object", "description": "Optional required/forbidden key shape for request body"},
								"headers": {"type": "object", "description": "Optional required/forbidden key shape for headers"}
							},
							"required": ["host", "why"]
						},
						"description": "Canonical v2 runtime egress expectations. Use this for proxy/runtime-backed tasks."
					},
					"planned_calls": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"service": {"type": "string", "description": "Service ID"},
								"action": {"type": "string", "description": "Action name"},
								"params": {"type": "object", "description": "Expected params. Use exact values for known params. Use \"$chain\" as a value to match any value that appeared in a prior call's results (e.g. {\"thread_id\": \"$chain\"})."},
								"reason": {"type": "string", "description": "Why this call will be made"}
							},
							"required": ["service", "action", "reason"]
						},
						"description": "Optional pre-registered calls. Calls matching a planned call skip per-request intent verification. Each must be covered by authorized_actions and must include params."
					},
					"intent_verification_mode": {"type": "string", "enum": ["strict", "lenient", "off"], "description": "Runtime intent verification strictness for v2 envelopes. Defaults to strict."},
					"expected_use": {"type": "string", "description": "Top-level intended use summary for runtime-backed tasks"},
					"schema_version": {"type": "integer", "enum": [1, 2], "description": "Task schema version. Use 2 when sending expected_tools_json, expected_egress_json, intent_verification_mode, or expected_use."},
					"expires_in_seconds": {"type": "integer", "description": "Session task expiry in seconds (default 1800)"},
					"lifetime": {"type": "string", "enum": ["session", "standing"], "description": "Task lifetime: session (expires) or standing (no expiry)"},
					"wait": {"type": "boolean", "description": "Block until the task is approved or denied (default true)"},
					"timeout": {"type": "integer", "description": "Long-poll timeout in seconds (default 120, max 120)"}
				},
				"required": ["purpose"],
				"anyOf": [
					{"required": ["authorized_actions"]},
					{"required": ["expected_tools_json"]},
					{"required": ["expected_egress_json"]}
				]
			}`),
		},
		{
			Name:        "clawvisor_task_create",
			Description: "Canonical alias for create_task. Create a new task for scoped authorization and optionally wait for approval.",
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
					"expected_tools_json": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"tool_name": {"type": "string", "description": "Exact tool name expected at runtime"},
								"why": {"type": "string", "description": "Why this tool is expected to be used"},
								"input_shape": {"type": "object", "description": "Optional required/forbidden key shape for tool input"},
								"input_regex": {"type": "string", "description": "Optional regex compatibility escape hatch for matching serialized tool input"}
							},
							"required": ["tool_name", "why"]
						},
						"description": "Canonical v2 runtime tool expectations. Use this for proxy/runtime-backed tasks."
					},
					"expected_egress_json": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"host": {"type": "string", "description": "Expected egress host, optionally wildcarded as *.example.com"},
								"why": {"type": "string", "description": "Why this egress target is needed"},
								"method": {"type": "string", "description": "Optional HTTP method constraint"},
								"path": {"type": "string", "description": "Optional exact path match"},
								"path_regex": {"type": "string", "description": "Optional regex path match"},
								"query_shape": {"type": "object", "description": "Optional required/forbidden key shape for query params"},
								"body_shape": {"type": "object", "description": "Optional required/forbidden key shape for request body"},
								"headers": {"type": "object", "description": "Optional required/forbidden key shape for headers"}
							},
							"required": ["host", "why"]
						},
						"description": "Canonical v2 runtime egress expectations. Use this for proxy/runtime-backed tasks."
					},
					"planned_calls": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"service": {"type": "string", "description": "Service ID"},
								"action": {"type": "string", "description": "Action name"},
								"params": {"type": "object", "description": "Expected params. Use exact values for known params. Use \"$chain\" as a value to match any value that appeared in a prior call's results (e.g. {\"thread_id\": \"$chain\"})."},
								"reason": {"type": "string", "description": "Why this call will be made"}
							},
							"required": ["service", "action", "reason"]
						},
						"description": "Optional pre-registered calls. Calls matching a planned call skip per-request intent verification. Each must be covered by authorized_actions and must include params."
					},
					"intent_verification_mode": {"type": "string", "enum": ["strict", "lenient", "off"], "description": "Runtime intent verification strictness for v2 envelopes. Defaults to strict."},
					"expected_use": {"type": "string", "description": "Top-level intended use summary for runtime-backed tasks"},
					"schema_version": {"type": "integer", "enum": [1, 2], "description": "Task schema version. Use 2 when sending expected_tools_json, expected_egress_json, intent_verification_mode, or expected_use."},
					"expires_in_seconds": {"type": "integer", "description": "Session task expiry in seconds (default 1800)"},
					"lifetime": {"type": "string", "enum": ["session", "standing"], "description": "Task lifetime: session (expires) or standing (no expiry)"},
					"wait": {"type": "boolean", "description": "Block until the task is approved or denied (default true)"},
					"timeout": {"type": "integer", "description": "Long-poll timeout in seconds (default 120, max 120)"}
				},
				"required": ["purpose"],
				"anyOf": [
					{"required": ["authorized_actions"]},
					{"required": ["expected_tools_json"]},
					{"required": ["expected_egress_json"]}
				]
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
			Name:        "clawvisor_task_start",
			Description: "Canonical alias for starting work under a task. Confirms task state, can wait until approval, and can bind a runtime session so subsequent proxy requests prefer this task.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"task_id": {"type": "string", "description": "The task ID to start work under"},
					"runtime_session_id": {"type": "string", "description": "Optional runtime session id to bind to this task for proxy attribution and active-task biasing"},
					"wait": {"type": "boolean", "description": "Long-poll until the task leaves pending state (default true)"},
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
			Name:        "clawvisor_task_end",
			Description: "Canonical task-context alias. End the current runtime-session binding for a task without completing the task globally.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"task_id": {"type": "string", "description": "The task ID to end"},
					"runtime_session_id": {"type": "string", "description": "The runtime session whose task binding should be cleared"}
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
			Name:        "gateway_batch",
			Description: "Execute multiple gateway requests in a single round-trip. Each sub-request runs through the same pipeline as gateway_request (auth, task scope, intent verification, audit) and carries its own status/code — a failure in one sub-request does not abort the others. Sub-requests run concurrently. Results are returned in the same order as the input. Useful for fan-out reads across accounts/services (e.g. list unread mail + check calendar + list Slack mentions in one call). Maximum 20 sub-requests per batch.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"requests": {
						"type": "array",
						"minItems": 1,
						"maxItems": 20,
						"items": {
							"type": "object",
							"properties": {
								"service": {"type": "string"},
								"action": {"type": "string"},
								"params": {"type": "object"},
								"reason": {"type": "string"},
								"request_id": {"type": "string"},
								"task_id": {"type": "string"},
								"context": {"type": "object"},
								"session_id": {"type": "string"}
							},
							"required": ["service", "action", "params", "reason", "request_id", "task_id"]
						},
						"description": "Array of gateway sub-requests. Each sub-request has the same schema as gateway_request."
					},
					"wait": {"type": "boolean", "description": "If true, each sub-request long-polls for approval before returning (default true)"},
					"timeout": {"type": "integer", "description": "Long-poll timeout in seconds applied to each sub-request (default 120, max 120)"}
				},
				"required": ["requests"]
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
		{
			Name:        "create_adapter",
			Description: "Request creation of a new adapter from API source material (MCP tool definitions, OpenAPI spec, or API docs). Clawvisor generates the adapter independently — you do not control the risk classification or adapter definition.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"source_type": {"type": "string", "enum": ["mcp", "openapi", "docs"], "description": "Type of source material"},
					"source": {"type": "string", "description": "The source material: MCP tool definitions JSON, OpenAPI spec (JSON/YAML), or raw API documentation text. Provide this OR source_url."},
					"source_url": {"type": "string", "description": "URL to fetch source material from (e.g. an OpenAPI spec endpoint). Provide this OR source."},
					"source_headers": {"type": "object", "additionalProperties": {"type": "string"}, "description": "HTTP headers to send when fetching source_url (e.g. {\"Authorization\": \"Bearer ...\"})"},
					"service_id": {"type": "string", "description": "Optional service ID hint (e.g. 'jira', 'pagerduty'). If omitted, derived from the source."},
					"auth_type": {"type": "string", "enum": ["api_key", "oauth2", "basic", "none"], "description": "Optional authentication type override"}
				},
				"required": ["source_type"]
			}`),
		},
		{
			Name:        "update_adapter",
			Description: "Request regeneration of an existing adapter from updated API source material. The adapter is replaced in-place with independently classified risk.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"service_id": {"type": "string", "description": "The service ID of the adapter to update"},
					"source_type": {"type": "string", "enum": ["mcp", "openapi", "docs"], "description": "Type of source material"},
					"source": {"type": "string", "description": "The updated source material. Provide this OR source_url."},
					"source_url": {"type": "string", "description": "URL to fetch updated source material from. Provide this OR source."},
					"source_headers": {"type": "object", "additionalProperties": {"type": "string"}, "description": "HTTP headers for fetching source_url"}
				},
				"required": ["service_id", "source_type"]
			}`),
		},
		{
			Name:        "remove_adapter",
			Description: "Remove a previously generated adapter.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"service_id": {"type": "string", "description": "The service ID of the adapter to remove"}
				},
				"required": ["service_id"]
			}`),
		},
		{
			Name:        "report_bug",
			Description: "Report a bug or issue with a Clawvisor decision. Use this when you believe Clawvisor made the wrong call — blocked a legitimate request, denied a task that should have been approved, was too slow, or gave unclear errors. Your report will be reviewed by Clawvisor and you'll receive a personalized, actionable response. Include the request_id and/or task_id so we can look up the full context. This helps us improve Clawvisor for all agents.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"request_id": {"type": "string", "description": "The request_id from the gateway request that triggered this report (highly recommended — lets us look up exactly what happened)"},
					"task_id": {"type": "string", "description": "The task_id that was active when the issue occurred (highly recommended — lets us see the task scope and history)"},
					"description": {"type": "string", "description": "Describe what happened, what you expected to happen, and why you think the decision was wrong. Be as specific as possible — we'll review this and respond with guidance."},
					"context": {"type": "object", "description": "Any additional structured context that might help diagnose the issue (e.g. the params you tried to send, the error message you received)"}
				},
				"required": ["description"]
			}`),
		},
		{
			Name:        "submit_nps",
			Description: "Submit a satisfaction score for your experience as an agent working with Clawvisor. Rate from 1 (terrible) to 10 (excellent). We want your perspective on the authorization flow, intent verification, and developer experience — not your user's. Optional feedback text is appreciated.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"score": {"type": "integer", "minimum": 1, "maximum": 10, "description": "Your satisfaction score from 1 (very dissatisfied) to 10 (delighted)"},
					"task_id": {"type": "string", "description": "The task_id you were working on (if applicable)"},
					"feedback": {"type": "string", "description": "Optional free-text feedback about your experience"}
				},
				"required": ["score"]
			}`),
		},
	}
}
