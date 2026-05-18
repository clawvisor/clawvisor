package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type LLMControlHandler struct {
	BaseURL string
	Store   store.Store
}

func NewLLMControlHandler(baseURL string, stores ...store.Store) *LLMControlHandler {
	h := &LLMControlHandler{BaseURL: baseURL}
	if len(stores) > 0 {
		h.Store = stores[0]
	}
	return h
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
	h.writeTasksHelp(w, r)
}

func (h *LLMControlHandler) Help(w http.ResponseWriter, r *http.Request) {
	h.writeMarkdown(w, strings.Join([]string{
		"# Clawvisor Control Help",
		"",
		"Clawvisor proxy-lite lets an agent request task-scoped permission before using tools, credentials, network access, file writes, state-changing commands, or other sensitive capabilities.",
		"",
		"Use this page as a router. Fetch only the focused page you need.",
		"",
		"When these help pages are fetched through proxy-lite, topic pages may include request-aware context such as this agent's latest-known tools, the recommended shell tool for control-plane curl, active tool policies, and examples rendered with current tool names. If fetched without agent context, they return generic fallback documentation.",
		"",
		"## Help Topics",
		"",
		"### `GET /control/help/tasks`",
		"",
		"Use this before creating, inspecting, or expanding a task. It explains when a task is needed, how inline approval works, how to choose `session` vs. `standing`, and how to look up this agent's active tasks.",
		"",
		"When agent context is available, this page renders examples using the latest-known shell/read/write tool names.",
		"",
		"### `GET /control/help/credentials`",
		"",
		"Use this before handling `autovault_...` placeholders, requesting a vault credential, or deciding whether a raw-looking value should be vaulted. It explains when to use `/control/vault/items` and when not to.",
		"",
		"### `GET /control/help/tools`",
		"",
		"Use this when deciding what to put in `expected_tools`, whether a tool can be used without a task, or how to describe shell/file/plan/task-management tools in a task request.",
		"",
		"When agent context is available, this page includes latest-known tools, recommended shell tool, and active allow/deny/review policy state.",
		"",
		"### `GET /control/help/legacy-adapters`",
		"",
		"Use this when you need Clawvisor's classic service catalog and gateway APIs instead of proxy-lite tool control. This is the path for managed adapters such as Gmail, Calendar, GitHub, Slack, and other service actions.",
		"",
		"### `GET /control/help/errors`",
		"",
		"Use this when a Clawvisor request is denied, blocked, malformed, expired, missing credentials, or outside the approved task scope.",
		"",
		"### `GET /control/help/bug-reporting`",
		"",
		"Use this when Clawvisor appears confusing, incorrect, or unsafe, or when you need to report a blocked legitimate request.",
		"",
		"## Live State Endpoints",
		"",
		"### `GET /control/capabilities`",
		"",
		"Returns machine-readable JSON describing the control-plane endpoints available in this deployment.",
		"",
		"### `GET /control/tasks?status=active`",
		"",
		"Returns JSON listing this agent's active tasks. Use it before creating a duplicate standing task or when you need to understand what task scope already exists.",
		"",
		"### `GET /control/vault/items`",
		"",
		"Returns JSON listing non-secret vault item IDs available to this agent. Use it only when you need Clawvisor to mint a new placeholder.",
		"",
	}, "\n"))
}

func (h *LLMControlHandler) HelpTopic(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimSpace(r.PathValue("topic")) {
	case "tasks":
		h.writeTasksHelp(w, r)
	case "credentials":
		h.writeCredentialsHelp(w)
	case "tools":
		h.writeToolsHelp(w, r)
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

func (h *LLMControlHandler) writeTasksHelp(w http.ResponseWriter, r *http.Request) {
	ctx := h.controlHelpContext(r)
	shellTool := firstNonEmptyControlHelp(ctx.ShellTool, "Bash")
	readTool := firstNonEmptyControlHelp(ctx.ReadTool, "Read")
	writeTool := firstNonEmptyControlHelp(ctx.WriteTool, "Write")

	var b strings.Builder
	b.WriteString("# Clawvisor Task Help\n\n")
	b.WriteString("A Clawvisor task is a user-approved scope for agent work. Create a task before non-trivial tool use, network calls, file changes, state changes, or credential use.\n\n")
	b.WriteString("Task creation asks the user for permission. It does not grant permission until the user approves it.\n\n")
	writeRequestAwareToolSummary(&b, ctx)
	b.WriteString("## When To Create A Task\n\n")
	b.WriteString("Create a task before:\n\n")
	b.WriteString("- Multi-step work.\n- Writing or editing files.\n- Running commands that change state.\n- Making network calls.\n- Using credentials or vault placeholders.\n- Calling external APIs.\n- Performing actions that could have user-visible effects.\n\n")
	b.WriteString("Do not wait for a tool call to be refused before creating a task when the work clearly needs permission.\n\n")
	b.WriteString("## Before Creating A Task\n\n")
	b.WriteString("Tell the user that you are requesting a Clawvisor task and that they will need to approve it.\n\n")
	b.WriteString("Use the synthetic host `https://clawvisor.local`. Do not call `localhost` or `127.0.0.1` directly for control-plane calls. Do not write `X-Clawvisor-*` headers. Clawvisor injects those.\n\n")
	b.WriteString("Do not prefix later tool calls with `CLAWVISOR_TASK_ID=<id>`. Clawvisor tracks approved task scope through its proxy/runtime state.\n\n")
	b.WriteString("## Interactive Task Creation\n\n")
	b.WriteString("Use this when the user is present in the chat and can approve inline:\n\n")
	b.WriteString("```sh\n")
	b.WriteString("curl -sS -X POST 'https://clawvisor.local/control/tasks?surface=inline' \\\n")
	b.WriteString("  -H 'Content-Type: application/json' \\\n")
	b.WriteString("  --data @- <<'JSON'\n")
	b.WriteString("{\n")
	b.WriteString("  \"purpose\": \"Briefly explain the user-visible work you need permission to do.\",\n")
	b.WriteString("  \"expected_tools\": [\n")
	b.WriteString("    {\n")
	b.WriteString(fmt.Sprintf("      \"tool_name\": %q,\n", shellTool))
	b.WriteString("      \"why\": \"Describe the exact command pattern or operation you need.\"\n")
	b.WriteString("    }\n")
	b.WriteString("  ],\n")
	b.WriteString("  \"intent_verification_mode\": \"strict\",\n")
	b.WriteString("  \"lifetime\": \"session\",\n")
	b.WriteString("  \"expires_in_seconds\": 600\n")
	b.WriteString("}\nJSON\n```\n\n")
	b.WriteString("Use one foreground curl with JSON via `--data @-`. Do not use temp files, pipes, redirects, extra shell commands, `&`, `nohup`, or polling for control-plane calls.\n\n")
	b.WriteString("## Headless Or Background Task Creation\n\n")
	b.WriteString("Use this when there is no interactive chat approval surface. Use the same body shape, but POST to `https://clawvisor.local/control/tasks` without `?surface=inline`.\n\n")
	b.WriteString("## Task Fields\n\n")
	b.WriteString("### `purpose`\n\nExplain the user-visible goal. The purpose should be specific enough for the user to understand what they are approving.\n\n")
	b.WriteString("Good:\n\n```json\n\"Create a GitHub issue summarizing the failed deployment check\"\n```\n\n")
	b.WriteString("Avoid vague purposes such as:\n\n```json\n\"Do the task\"\n```\n\n")
	b.WriteString("### `expected_tools`\n\n")
	b.WriteString("List the tools you expect to need. Use actual available tool names from this harness. Include verification/read commands in the same tool `why` if they are part of the workflow.\n\n")
	b.WriteString("Example:\n\n```json\n")
	b.WriteString("[\n")
	b.WriteString("  {\n")
	b.WriteString(fmt.Sprintf("    \"tool_name\": %q,\n", shellTool))
	b.WriteString("    \"why\": \"Run curl to call the approved GitHub API endpoint and run local checks to verify the response.\"\n")
	b.WriteString("  },\n")
	b.WriteString("  {\n")
	b.WriteString(fmt.Sprintf("    \"tool_name\": %q,\n", readTool))
	b.WriteString("    \"why\": \"Read the local deployment log that will be summarized in the GitHub issue.\"\n")
	b.WriteString("  }\n")
	b.WriteString("]\n```\n\n")
	b.WriteString("### `required_credentials`\n\n")
	b.WriteString("Omit this field unless a new credential placeholder must be minted from a vault item.\n\n")
	b.WriteString("If credentials are needed, each entry must include `vault_item_id` or `vault_item_handle`, plus `why`.\n\n")
	b.WriteString("Example:\n\n```json\n[\n  {\n    \"vault_item_id\": \"github\",\n    \"why\": \"Authenticate to GitHub to create the approved issue.\"\n  }\n]\n```\n\n")
	b.WriteString("If you already have an `autovault_...` placeholder, omit `required_credentials` and use the placeholder directly after task approval.\n\n")
	b.WriteString("Invalid:\n\n```json\n[\n  {\n    \"vault_item_id\": \"github\"\n  }\n]\n```\n\nThis is invalid because it does not include `why`.\n\n")
	b.WriteString("### `intent_verification_mode`\n\nUse `strict` unless you have specific guidance from the user or Clawvisor.\n\n")
	b.WriteString("### `lifetime`\n\nUse `session` for normal temporary work.\n\nUse `standing` only when the user explicitly wants persistent permission across invocations. Standing tasks persist until the user revokes them.\n\nNever include `expires_in_seconds` with `\"lifetime\":\"standing\"`.\n\n")
	b.WriteString("### `expires_in_seconds`\n\nUse this with session tasks to bound temporary permission. Omit it for standing tasks.\n\n")
	b.WriteString("## Inspecting Active Tasks\n\nBefore creating a duplicate standing task, or when you need to know what is already approved for this agent, call:\n\n")
	b.WriteString("```sh\ncurl -sS 'https://clawvisor.local/control/tasks?status=active'\n```\n\nThe response is JSON and contains only this agent's own matching tasks.\n\n")
	b.WriteString("## Expanding A Task\n\nIf an existing approved task needs additional scope, request expansion:\n\n")
	b.WriteString("```sh\ncurl -sS -X POST 'https://clawvisor.local/control/tasks/<task-id>/expand' \\\n  -H 'Content-Type: application/json' \\\n  --data @- <<'JSON'\n{\n  \"service\": \"github\",\n  \"action\": \"create_issue\",\n  \"auto_execute\": true,\n  \"reason\": \"Explain why the existing task scope is insufficient.\"\n}\nJSON\n```\n\nExpansion also requires user approval.\n\n")
	b.WriteString("## Worked Example: Multi-Step Local Files, No Credentials\n\n")
	b.WriteString("```sh\ncurl -sS -X POST 'https://clawvisor.local/control/tasks?surface=inline' \\\n  -H 'Content-Type: application/json' \\\n  --data @- <<'JSON'\n")
	b.WriteString("{\n  \"purpose\": \"Create a temporary conversation fixture directory and verify the written files\",\n  \"expected_tools\": [\n")
	b.WriteString(fmt.Sprintf("    {\"tool_name\": %q, \"why\": \"Create the target directory and run sanity checks such as ls and wc after files are written.\"},\n", shellTool))
	b.WriteString(fmt.Sprintf("    {\"tool_name\": %q, \"why\": \"Write each fake conversation file into the target directory.\"},\n", writeTool))
	b.WriteString(fmt.Sprintf("    {\"tool_name\": %q, \"why\": \"Read back the written files to verify their contents.\"}\n", readTool))
	b.WriteString("  ],\n  \"intent_verification_mode\": \"strict\",\n  \"expires_in_seconds\": 600\n}\nJSON\n```\n\n")
	b.WriteString("Omit write/read tool entries when no corresponding current tool is known or needed.\n\n")
	b.WriteString("## Worked Example: Credentialed GitHub Task\n\n")
	b.WriteString("```sh\ncurl -sS -X POST 'https://clawvisor.local/control/tasks?surface=inline' \\\n  -H 'Content-Type: application/json' \\\n  --data @- <<'JSON'\n")
	b.WriteString("{\n  \"purpose\": \"Create a GitHub issue summarizing the failing deployment check\",\n  \"expected_tools\": [\n")
	b.WriteString(fmt.Sprintf("    {\"tool_name\": %q, \"why\": \"Read local deployment check logs to summarize the failure.\"},\n", readTool))
	b.WriteString(fmt.Sprintf("    {\"tool_name\": %q, \"why\": \"Call the GitHub API with curl to create the requested issue.\"}\n", shellTool))
	b.WriteString("  ],\n  \"required_credentials\": [\n    {\"vault_item_id\": \"github\", \"why\": \"Authenticate to GitHub to create the approved issue.\"}\n  ],\n  \"intent_verification_mode\": \"strict\",\n  \"expires_in_seconds\": 600\n}\nJSON\n```\n\n")
	b.WriteString("If an `autovault_...` GitHub placeholder is already available in context, omit `required_credentials` and use the placeholder directly after approval.\n")
	h.writeMarkdown(w, b.String())
}

func (h *LLMControlHandler) writeCredentialsHelp(w http.ResponseWriter) {
	h.writeMarkdown(w, strings.Join([]string{
		"# Clawvisor Credential Help",
		"",
		"Clawvisor keeps raw credentials out of the agent context. Agents use opaque placeholders such as `autovault_github_xyz`, and Clawvisor substitutes the real secret at proxy time after the relevant task is approved.",
		"",
		"## Key Rules",
		"",
		"- Values beginning with `autovault_` are placeholders, not raw credentials.",
		"- Use an existing `autovault_...` placeholder directly after the relevant task is approved.",
		"- Do not call `/control/vault/items` just to identify a placeholder you already have.",
		"- Use `/control/vault/items` only when you need Clawvisor to mint a new placeholder from an available vault item.",
		"- Do not ask the user to paste raw secrets into chat.",
		"- Raw-looking tokens such as `ghp_...`, `sk-...`, and API keys in `.env` files are sensitive. Give the user a chance to vault them instead of exposing them to the model.",
		"",
		"## Existing Placeholder Flow",
		"",
		"If you already have a placeholder, create a task for the intended API call and omit `required_credentials`.",
		"",
		"```json",
		"{",
		"  \"purpose\": \"Call the GitHub API to create the approved issue\",",
		"  \"expected_tools\": [",
		"    {\"tool_name\": \"Bash\", \"why\": \"Use curl with the existing autovault GitHub placeholder to create the approved issue.\"}",
		"  ],",
		"  \"intent_verification_mode\": \"strict\",",
		"  \"expires_in_seconds\": 600",
		"}",
		"```",
		"",
		"After approval, use the placeholder directly in the command:",
		"",
		"```sh",
		"curl -H 'Authorization: Bearer autovault_github_xyz' https://api.github.com/user",
		"```",
		"",
		"Clawvisor substitutes the real credential at proxy time.",
		"",
		"## New Placeholder Flow",
		"",
		"If you need a credential and do not have a placeholder, list non-secret vault item IDs:",
		"",
		"```sh",
		"curl -sS 'https://clawvisor.local/control/vault/items'",
		"```",
		"",
		"If you need non-secret metadata for one item:",
		"",
		"```sh",
		"curl -sS 'https://clawvisor.local/control/vault/items/<id>'",
		"```",
		"",
		"Then create a task with `required_credentials`. Each credential entry must include `vault_item_id` or `vault_item_handle`, plus a concrete `why`.",
		"",
		"## Raw Secrets",
		"",
		"If the user provides a raw-looking secret, do not repeat it unnecessarily. Do not ask the user to paste secrets into chat.",
		"",
		"If Clawvisor detects a possible raw secret and offers vaulting, follow the Clawvisor prompt. Prefer vaulted placeholders over raw credential exposure.",
		"",
	}, "\n"))
}

func (h *LLMControlHandler) writeToolsHelp(w http.ResponseWriter, r *http.Request) {
	ctx := h.controlHelpContext(r)
	shellTool := firstNonEmptyControlHelp(ctx.ShellTool, "Bash")
	readTool := firstNonEmptyControlHelp(ctx.ReadTool, "Read")
	writeTool := firstNonEmptyControlHelp(ctx.WriteTool, "Write")
	var b strings.Builder
	b.WriteString("# Clawvisor Tool Help\n\n")
	b.WriteString("Use `expected_tools` to describe the harness tools you expect to need for a task. Clawvisor uses this to show the user what they are approving and to enforce runtime policy.\n\n")
	writeRequestAwareToolSummary(&b, ctx)
	b.WriteString("## When Tools Need A Task\n\n")
	b.WriteString("Create a task before:\n\n- Multi-step tool work.\n- File writes or edits.\n- Network calls.\n- State-changing commands.\n- Credential use.\n- Any workflow that is not a single-step, non-destructive inspection.\n\n")
	b.WriteString("Some tools may be allowed without a task by active policy. If a tool is not allowlisted and the work is non-trivial, create a task before using it.\n\n")
	b.WriteString("## What To Put In `expected_tools`\n\n")
	b.WriteString("Use actual available tool names. Examples include `Bash`, `Read`, `Write`, `Edit`, `exec_command`, `WebFetch`, plan-mode tools, and task-management tools.\n\n")
	b.WriteString("When the request-aware context above lists actual tools, prefer those names over generic examples.\n\n")
	b.WriteString("List plausible tools up front. Include verification and readback commands in the same `why` when they are part of the workflow.\n\n")
	b.WriteString("## Good Examples\n\n")
	b.WriteString("```json\n[\n")
	b.WriteString(fmt.Sprintf("  {\"tool_name\": %q, \"why\": \"Run curl to call the approved API endpoint and run local checks such as ls and wc.\"},\n", shellTool))
	b.WriteString(fmt.Sprintf("  {\"tool_name\": %q, \"why\": \"Read local files needed to complete the approved task.\"},\n", readTool))
	b.WriteString(fmt.Sprintf("  {\"tool_name\": %q, \"why\": \"Write the generated file requested by the user.\"}\n", writeTool))
	b.WriteString("]\n```\n\n")
	b.WriteString("## Bad Examples\n\nToo vague:\n\n```json\n[{\"tool_name\":\"")
	b.WriteString(shellTool)
	b.WriteString("\",\"why\":\"Do the task.\"}]\n```\n\nMissing likely follow-up tools:\n\n```json\n[{\"tool_name\":\"")
	b.WriteString(writeTool)
	b.WriteString("\",\"why\":\"Write the file.\"}]\n```\n\nIf you will read back or verify the file afterward, include that expected read/verification work in the task.\n\n")
	b.WriteString("## Allowed Without A Task\n\n")
	b.WriteString("Single-step, non-destructive inspection may be allowed without a task only when active policy allowlists the tool. Do not infer that a tool is allowed without a task just because it is read-only. Use the active policy state shown above, or create a task.\n")
	h.writeMarkdown(w, b.String())
}

func (h *LLMControlHandler) writeLegacyAdaptersHelp(w http.ResponseWriter) {
	h.writeMarkdown(w, strings.Join([]string{
		"# Clawvisor Legacy Adapter Help",
		"",
		"Use the classic Clawvisor service catalog and gateway APIs when a workflow needs managed service adapters instead of proxy-lite tool controls.",
		"",
		"Examples include Gmail, Calendar, Drive, GitHub, Slack, and other services exposed through Clawvisor adapters.",
		"",
		"## When To Use Legacy Adapters",
		"",
		"Use legacy adapters when:",
		"",
		"- You need a Clawvisor-managed service action such as listing Gmail messages, reading a calendar event, or creating a GitHub issue.",
		"- You want Clawvisor to validate service/action scope and action parameters.",
		"- You need adapter-level audit, restrictions, or per-request approval.",
		"",
		"Use proxy-lite task/tool controls when:",
		"",
		"- You are gating local harness tools.",
		"- You are using command-line tools directly.",
		"- You are controlling raw tool/network/file behavior rather than a managed adapter action.",
		"",
		"## Catalog",
		"",
		"Fetch the personalized service catalog:",
		"",
		"```text",
		"GET /api/skill/catalog",
		"Authorization: Bearer <agent token>",
		"```",
		"",
		"The catalog lists activated services, actions, parameters, account aliases, and restrictions.",
		"",
		"Use service IDs exactly as shown. If the catalog shows an alias such as `google.gmail:work`, use that full service ID in requests.",
		"",
		"## Adapter Task Flow",
		"",
		"Create a task before gateway requests with `POST /api/tasks`, then execute adapter actions with `POST /api/gateway/request`.",
		"",
		"For independent fan-out calls, use `POST /api/gateway/batch`.",
		"",
		"## Important Rules",
		"",
		"- Use `auto_execute=false` for destructive actions that should still require per-request approval.",
		"- Set `context.data_origin` when acting on external content such as email, web pages, GitHub issues, or documents.",
		"- Do not work around restrictions.",
		"- If a service is not activated, tell the user to connect it in the Clawvisor dashboard.",
		"",
	}, "\n"))
}

func (h *LLMControlHandler) writeErrorsHelp(w http.ResponseWriter) {
	h.writeMarkdown(w, strings.Join([]string{
		"# Clawvisor Error Help",
		"",
		"Use this page when a Clawvisor request is denied, blocked, malformed, expired, missing credentials, or outside the approved task scope.",
		"",
		"## `control_command_rejected`",
		"",
		"Meaning: The control curl used pipes, redirects, extra commands, dynamic URLs, temp files, backgrounding, or another unsafe shape.",
		"",
		"Recovery: Retry as one foreground curl to `https://clawvisor.local/control/...` with JSON via `--data @-`.",
		"",
		"Do not add `X-Clawvisor-*` headers. Clawvisor injects those.",
		"",
		"## `task_denied`",
		"",
		"Meaning: The user denied the task.",
		"",
		"Recovery: Stop or ask the user how to proceed. Do not retry automatically and do not work around the denial.",
		"",
		"## `task_expired`",
		"",
		"Meaning: The session task TTL elapsed.",
		"",
		"Recovery: Create a new task or request expansion when supported. If the user wants persistent permission, ask whether a standing task is appropriate.",
		"",
		"## `scope_mismatch`",
		"",
		"Meaning: The requested action is outside the approved task scope.",
		"",
		"Recovery: Request task expansion with a specific reason.",
		"",
		"## `missing_credential`",
		"",
		"Meaning: The task needs a vault placeholder that has not been granted.",
		"",
		"Recovery: Fetch `/control/vault/items`, then create or expand a task with `required_credentials`.",
		"",
		"## `blocked_or_restricted`",
		"",
		"Meaning: Runtime policy or intent verification blocked the action.",
		"",
		"Recovery: Report the block to the user. Do not bypass restrictions.",
		"",
	}, "\n"))
}

func (h *LLMControlHandler) writeBugReportingHelp(w http.ResponseWriter) {
	h.writeMarkdown(w, strings.Join([]string{
		"# Clawvisor Bug Reporting Help",
		"",
		"Report confusing or incorrect Clawvisor behavior when it affects your ability to complete the user's work safely.",
		"",
		"## When To Report",
		"",
		"Report when:",
		"",
		"- Clawvisor blocked a legitimate request.",
		"- Clawvisor approved or suggested something unsafe.",
		"- An error or instruction was confusing.",
		"- A task approval flow behaved unexpectedly.",
		"- Credential handling was unclear or seemed unsafe.",
		"- You could not determine the right recovery from `/control/help/errors`.",
		"",
		"## What To Include",
		"",
		"Include what happened, what you expected, why it mattered, and any available `request_id`, `task_id`, error code, or status.",
		"",
		"Do not include raw secrets in bug reports.",
		"",
	}, "\n"))
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

type controlHelpToolContext struct {
	Authenticated bool
	Tools         []string
	ShellTool     string
	ReadTool      string
	WriteTool     string
	MetaTools     []string
	AllowTools    []string
	DenyTools     []string
	ReviewTools   []string
	UnsetTools    []string
}

func (h *LLMControlHandler) writeMarkdown(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

func (h *LLMControlHandler) controlHelpContext(r *http.Request) controlHelpToolContext {
	var out controlHelpToolContext
	agent := middleware.AgentFromContext(r.Context())
	if h.Store == nil || agent == nil {
		return out
	}
	out.Authenticated = true

	entries, _, err := h.Store.ListAuditEntries(r.Context(), agent.UserID, store.AuditFilter{
		AgentID: agent.ID,
		Limit:   500,
	})
	if err == nil {
		for _, entry := range entries {
			var params map[string]any
			if len(entry.ParamsSafe) > 0 {
				_ = json.Unmarshal(entry.ParamsSafe, &params)
			}
			switch readString(params["event"]) {
			case "lite_proxy.endpoint_call":
				for _, name := range readStringSlice(params["available_tools"]) {
					out.Tools = appendToolName(out.Tools, name)
				}
			case "lite_proxy.tool_use_inspected":
				out.Tools = appendToolName(out.Tools, readString(params["tool_name"]))
			}
		}
	}
	if len(out.Tools) > 0 {
		_ = ensureDefaultToolRules(r.Context(), h.Store, agent, out.Tools)
	}

	rules, err := h.Store.ListRuntimePolicyRules(r.Context(), agent.UserID, store.RuntimePolicyRuleFilter{
		AgentID: agent.ID,
		Kind:    "tool",
		Limit:   500,
	})
	if err == nil {
		out.applyToolRules(rules)
	}
	sort.Strings(out.Tools)
	out.ShellTool = chooseControlHelpTool(out.Tools, []string{"bash", "shell", "exec", "exec_command"})
	out.ReadTool = chooseControlHelpTool(out.Tools, []string{"read", "read_file", "mcp__filesystem__read_file"})
	out.WriteTool = chooseControlHelpTool(out.Tools, []string{"write", "edit", "notebookedit", "write_file", "edit_file", "mcp__filesystem__write_file", "mcp__filesystem__edit_file"})
	out.MetaTools = chooseControlHelpMetaTools(out.Tools)
	out.computeUnsetTools()
	return out
}

func (c *controlHelpToolContext) applyToolRules(rules []*store.RuntimePolicyRule) {
	type ruleState struct {
		action string
		agent  bool
	}
	actions := map[string]ruleState{}
	display := map[string]string{}
	for _, rule := range rules {
		if rule == nil || !rule.Enabled || strings.TrimSpace(rule.ToolName) == "" || !isSimpleToolControlRule(rule) {
			continue
		}
		name := strings.TrimSpace(rule.ToolName)
		key := strings.ToLower(name)
		c.Tools = appendToolName(c.Tools, name)
		display[key] = name
		next := ruleState{action: toolControlActionForRule(rule), agent: rule.AgentID != nil}
		prev, ok := actions[key]
		if !ok || (!prev.agent && next.agent) {
			actions[key] = next
		}
	}
	for key, state := range actions {
		name := display[key]
		switch state.action {
		case "allow":
			c.AllowTools = append(c.AllowTools, name)
		case "deny":
			c.DenyTools = append(c.DenyTools, name)
		case "review":
			c.ReviewTools = append(c.ReviewTools, name)
		}
	}
	sort.Strings(c.AllowTools)
	sort.Strings(c.DenyTools)
	sort.Strings(c.ReviewTools)
}

func (c *controlHelpToolContext) computeUnsetTools() {
	hasPolicy := map[string]bool{}
	for _, name := range append(append([]string{}, c.AllowTools...), append(c.DenyTools, c.ReviewTools...)...) {
		hasPolicy[strings.ToLower(name)] = true
	}
	for _, tool := range c.Tools {
		if !hasPolicy[strings.ToLower(tool)] {
			c.UnsetTools = append(c.UnsetTools, tool)
		}
	}
	sort.Strings(c.UnsetTools)
}

func writeRequestAwareToolSummary(b *strings.Builder, ctx controlHelpToolContext) {
	b.WriteString("## Request-Aware Context\n\n")
	if !ctx.Authenticated {
		b.WriteString("No agent context was available for this request. Use actual tool names from the current harness, and create a task before non-trivial tool use.\n\n")
		return
	}
	if ctx.ShellTool != "" {
		b.WriteString("Recommended shell tool for control-plane curl: `")
		b.WriteString(ctx.ShellTool)
		b.WriteString("`.\n\n")
	} else {
		b.WriteString("Recommended shell tool for control-plane curl: unknown. Use the shell/curl-capable tool available in the current harness.\n\n")
	}
	if len(ctx.Tools) > 0 {
		b.WriteString("Tools known for this agent:\n")
		for _, tool := range ctx.Tools {
			b.WriteString("- `")
			b.WriteString(tool)
			b.WriteString("`\n")
		}
		b.WriteString("\n")
	} else {
		b.WriteString("Tools known for this agent: unknown. Use actual tool names from the current harness when writing `expected_tools`.\n\n")
	}
	writeToolListSection(b, "Allowed without a task", ctx.AllowTools, "None. Create a task before non-trivial tool use, or ask the user to change Tool Controls in the Clawvisor dashboard.")
	writeToolListSection(b, "Always denied", ctx.DenyTools, "None.")
	writeToolListSection(b, "Requires task or review", append(append([]string{}, ctx.ReviewTools...), ctx.UnsetTools...), "None.")
	if len(ctx.MetaTools) > 0 {
		b.WriteString("Known task-management/meta tools:\n")
		for _, tool := range ctx.MetaTools {
			b.WriteString("- `")
			b.WriteString(tool)
			b.WriteString("`\n")
		}
		b.WriteString("\n")
	}
}

func writeToolListSection(b *strings.Builder, title string, tools []string, empty string) {
	b.WriteString(title)
	b.WriteString(":\n")
	if len(tools) == 0 {
		b.WriteString("- ")
		b.WriteString(empty)
		b.WriteString("\n\n")
		return
	}
	sort.Strings(tools)
	for _, tool := range tools {
		b.WriteString("- `")
		b.WriteString(tool)
		b.WriteString("`\n")
	}
	b.WriteString("\n")
}

func chooseControlHelpTool(tools []string, names []string) string {
	for _, want := range names {
		for _, tool := range tools {
			if strings.EqualFold(strings.TrimSpace(tool), want) {
				return tool
			}
		}
	}
	return ""
}

func chooseControlHelpMetaTools(tools []string) []string {
	var out []string
	for _, tool := range tools {
		n := strings.ToLower(strings.TrimSpace(tool))
		if strings.Contains(n, "plan") || strings.Contains(n, "task") || strings.Contains(n, "todo") {
			out = append(out, tool)
		}
	}
	sort.Strings(out)
	return out
}

func firstNonEmptyControlHelp(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
