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
			Description: "Create a new task for scoped authorization. Use wait=true (recommended) to block until the user approves or denies. Must include at least one of: authorized_actions, expected_tools, or expected_egress.",
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
					"expected_tools": {
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
					"expected_egress": {
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
					"chain_extraction_mode": {"type": "string", "enum": ["full", "builtins_only"], "description": "Async chain-context extraction mode for this task. \"full\" runs the LLM Phase-2 extraction pass; \"builtins_only\" skips it and relies only on the synchronous builtin regex patterns. Omit to defer to the system default."},
					"expected_use": {"type": "string", "description": "Top-level intended use summary for runtime-backed tasks"},
					"schema_version": {"type": "integer", "enum": [1, 2], "description": "Task schema version. Use 2 when sending expected_tools, expected_egress, intent_verification_mode, or expected_use."},
					"expires_in_seconds": {"type": "integer", "description": "Session task expiry in seconds (default 1800)"},
					"lifetime": {"type": "string", "enum": ["session", "sliding", "standing"], "description": "Task lifetime: session (fixed expiry), sliding (expiry auto-extends 10 min on each authorized tool_use), or standing (no expiry)"},
					"wait": {"type": "boolean", "description": "Block until the task is approved or denied (default true)"},
					"timeout": {"type": "integer", "description": "Long-poll timeout in seconds (default 120, max 120)"}
				},
				"required": ["purpose"]
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
			Description: "Request expanding an existing task's scope. The body mirrors create_task's envelope: list the new expected_tools / expected_egress / required_credentials (each with a why). Tool names shaped as 'service:action' (e.g. 'github:create_issue') also grant gateway scope for that action. Use this when the work is the same body of work but you discovered you need additional capabilities mid-task. Create a NEW task instead if the new ask is a genuinely different goal. Use wait=true (recommended) to block until the user approves or denies.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"task_id": {"type": "string", "description": "The task ID to expand"},
					"expected_tools": {
						"type": "array",
						"description": "New tools the task needs. Each entry: {tool_name, why} plus optional input_shape/input_regex constraints (same shape as create_task). For agent-routed gateway calls, use 'service:action' (e.g. 'github:create_issue'); for local tools, use the bare name (e.g. 'Edit', 'Bash'). Constraints not declared here cannot tighten an existing entry — restate the parent's constraint plus your additional intent if you want to reuse the row's why slot.",
						"items": {
							"type": "object",
							"properties": {
								"tool_name": {"type": "string"},
								"why": {"type": "string"},
								"input_shape": {"type": "object", "description": "Optional. Constraint shape on the tool's input (same semantics as create_task)."},
								"input_regex": {"type": "string", "description": "Optional. Regex matched against the tool's stringified input."}
							},
							"required": ["tool_name", "why"]
						}
					},
					"expected_egress": {
						"type": "array",
						"description": "New non-control hosts the task may reach. Each entry: {host, why} plus optional method/path/path_regex/query_shape/body_shape/headers/credential_alias (same shape as create_task). An addition that names the same host as an existing entry but DIFFERS on a structural field (Method, Path, …) lands as a NEW row — both endpoints coexist. To revise an existing row's why, leave structural fields empty.",
						"items": {
							"type": "object",
							"properties": {
								"host": {"type": "string"},
								"why": {"type": "string"},
								"method": {"type": "string", "description": "Optional. HTTP method (GET / POST / PUT / PATCH / DELETE / HEAD / OPTIONS)."},
								"path": {"type": "string", "description": "Optional. Exact path. Mutually exclusive with path_regex."},
								"path_regex": {"type": "string", "description": "Optional. Path regex. Mutually exclusive with path."},
								"query_shape": {"type": "object", "description": "Optional. Constraint shape on the query string."},
								"body_shape": {"type": "object", "description": "Optional. Constraint shape on the request body."},
								"headers": {"type": "object", "description": "Optional. Constraint shape on the request headers."},
								"credential_alias": {"type": "string", "description": "Optional. Bind a specific credential alias to this egress endpoint."}
							},
							"required": ["host", "why"]
						}
					},
					"required_credentials": {
						"type": "array",
						"description": "New vault credentials the task needs. Each entry: {vault_item_id, why} or {vault_item_handle, why}. Exactly one of vault_item_id / vault_item_handle is required; the API rejects entries with neither.",
						"items": {
							"type": "object",
							"properties": {
								"vault_item_id": {"type": "string", "description": "Account-scoped vault item id (preferred), e.g. 'github:personal'."},
								"vault_item_handle": {"type": "string", "description": "Vault item handle. Use when vault_item_id is not available; one of vault_item_id / vault_item_handle must be set."},
								"why": {"type": "string"}
							},
							"required": ["why"]
						}
					},
					"reason": {"type": "string", "description": "One-line summary of why this expansion is needed. If you are revising the why of an existing entry, write it so it covers BOTH the prior approved purpose AND the new capability — replace-by-name overwrites the old why wholesale."},
					"wait": {"type": "boolean", "description": "Block until the expansion is approved or denied (default true)"},
					"timeout": {"type": "integer", "description": "Long-poll timeout in seconds (default 120, max 120)"}
				},
				"required": ["task_id", "reason"]
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
