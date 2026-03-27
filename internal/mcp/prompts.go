package mcp

// Prompt is an MCP prompt definition.
type Prompt struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// PromptMessage is a message in a prompt response.
type PromptMessage struct {
	Role    string        `json:"role"`
	Content ToolContent   `json:"content"`
}

// promptDefs returns the static list of MCP prompts.
func promptDefs() []Prompt {
	return []Prompt{
		{
			Name:        "clawvisor-workflow",
			Description: "How to use Clawvisor's task-scoped authorization, gateway requests, and service catalog. Read this before making your first request.",
		},
	}
}

// promptContent returns the messages for the named prompt.
func promptContent(name string) (string, []PromptMessage, bool) {
	switch name {
	case "clawvisor-workflow":
		return "Clawvisor workflow instructions", []PromptMessage{
			{
				Role: "user",
				Content: ToolContent{Type: "text", Text: workflowPrompt},
			},
		}, true
	default:
		return "", nil, false
	}
}

const workflowPrompt = `# Clawvisor Workflow

You are connected to Clawvisor via MCP. Clawvisor is a gatekeeper between you and external services (Gmail, Calendar, Drive, GitHub, Slack, iMessage, etc.). Every action goes through Clawvisor, which checks restrictions, validates task scopes, injects credentials, optionally routes to the user for approval, and returns a clean result. You never hold API keys.

## Authorization Model

Two layers, applied in order:
1. **Restrictions** — hard blocks the user sets. If a restriction matches, the action is blocked immediately.
2. **Tasks** — scopes you declare. Every request must be attached to an approved task. Actions with ` + "`auto_execute: true`" + ` run without per-request approval. Actions with ` + "`auto_execute: false`" + ` still require per-request approval within the task.

## Typical Flow

1. **Fetch the catalog** — use the ` + "`fetch_catalog`" + ` tool to see available services, actions, and restrictions
2. **Create a task** — use ` + "`create_task`" + ` to declare your purpose and the actions you need
3. **Wait for approval** — use ` + "`get_task`" + ` with ` + "`wait: true`" + ` to long-poll until the user approves (or denies) the task
4. **Make gateway requests** — use ` + "`gateway_request`" + ` for each action, under the approved task scope
5. **Complete the task** — use ` + "`complete_task`" + ` when done

## Creating Tasks

When calling ` + "`create_task`" + `:
- **` + "`purpose`" + `** — shown to the user during approval. Be specific: "Review last 30 iMessage threads and classify reply status" is better than "Use iMessage."
- **` + "`authorized_actions`" + `** — list each service + action pair you need, with:
  - ` + "`auto_execute: true`" + ` for read-only actions you want to run immediately
  - ` + "`auto_execute: false`" + ` for destructive actions (send, delete, etc.) that should require per-request approval
  - ` + "`expected_use`" + ` — describe how you'll use each action. Shown during approval and verified against your actual requests.
- **` + "`lifetime`" + `** — ` + "`\"session\"`" + ` (default, expires) or ` + "`\"standing\"`" + ` (no expiry, persists until revoked)
- **` + "`expires_in_seconds`" + `** — TTL for session tasks (default 1800)

All tasks start as ` + "`pending_approval`" + `. Long-poll with ` + "`get_task`" + ` (` + "`wait: true`" + `) until status changes to ` + "`active`" + ` or ` + "`denied`" + `.

### Standing Tasks

For recurring workflows, set ` + "`lifetime: \"standing\"`" + `. Standing tasks require a ` + "`session_id`" + ` in gateway requests to enable chain context verification across related requests.

### Scope Expansion

If you need an action not in the original task scope, use ` + "`expand_task`" + ` with the new action and a reason. The user will be prompted to approve.

## Gateway Requests

When calling ` + "`gateway_request`" + `:
- **` + "`service`" + `** and **` + "`action`" + `** — from the catalog
- **` + "`params`" + `** — action-specific parameters (documented in the catalog)
- **` + "`reason`" + `** — one sentence explaining why. Be specific.
- **` + "`request_id`" + `** — a unique ID you generate (e.g. UUID). Must be unique across all your requests.
- **` + "`task_id`" + `** — the approved task ID this request belongs to

## Handling Responses

Every gateway response has a ` + "`status`" + ` field:

| Status | Meaning | What to do |
|---|---|---|
| ` + "`executed`" + ` | Action completed | Use the result. Report to the user. |
| ` + "`pending`" + ` | Awaiting human approval | Tell the user. Re-send same request_id to poll. |
| ` + "`blocked`" + ` | A restriction blocks this | Tell the user. Do not retry or work around it. |
| ` + "`restricted`" + ` | Intent verification rejected | Your params/reason were inconsistent with the task purpose. Adjust and retry with a new request_id. |
| ` + "`pending_task_approval`" + ` | Task not yet approved | Long-poll get_task until approved. |
| ` + "`pending_scope_expansion`" + ` | Action outside task scope | Use expand_task. |
| ` + "`task_expired`" + ` | Task TTL exceeded | Expand or create a new task. |
| ` + "`error`" + ` | Something went wrong | Report the error to the user. Do not silently retry. |

## Important Rules

- Always fetch the catalog first to know what's available and restricted
- Never attempt to bypass restrictions — they are hard blocks set by the user
- Always create a task before making gateway requests
- Use ` + "`auto_execute: false`" + ` for any action that sends, modifies, or deletes data
- Generate unique request_ids for every gateway request
- Complete tasks when done to clean up authorization scope
`
