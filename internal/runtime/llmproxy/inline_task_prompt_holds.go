package llmproxy

import (
	"encoding/json"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// taskCreationPromptForHolds builds the inline task-creation prompt the
// agent is told to POST when the user types "task" on a single- or
// multi-tool approval. `expected_tools` enumerates every distinct tool
// name in the batch so the generated task scope covers every held call.
// Without this, typing "task" on a coalesced approval prompt would scope
// only the primary tool and leave sibling reviewed calls to re-prompt on
// the next retry.
// TaskCreationPromptForHolds renders the inline task-creation prompt
// the agent is told to POST when the user types "task" on a single-
// or multi-tool approval. Exported for the postproc package's
// coalesce path.
func TaskCreationPromptForHolds(holds []HeldToolUse) string {
	return taskCreationPromptForHolds(holds)
}

func taskCreationPromptForHolds(holds []HeldToolUse) string {
	if len(holds) == 0 {
		return ""
	}
	// Deduplicate by tool name; keep insertion order so the rendered
	// expected_tools mirrors the model's emit order (matters for
	// dependent sequences readers will recognize). The why for a
	// duplicated tool name comes from the FIRST tool_use of that
	// name — taskToolWhy already produces a description broad enough
	// to cover sibling calls (e.g. "Run shell commands needed for
	// the task, including writes AND verification reads").
	seen := map[string]bool{}
	expected := make([]map[string]any, 0, len(holds))
	for _, held := range holds {
		name := strings.TrimSpace(held.ToolUse.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		expected = append(expected, map[string]any{
			"tool_name": name,
			"why":       taskToolWhy(held.ToolUse),
		})
	}
	if len(expected) == 0 {
		return ""
	}
	payload := map[string]any{
		"purpose":                  "Describe the user-visible task you are trying to complete, including why this tool access is needed.",
		"expected_tools":           expected,
		"intent_verification_mode": "strict",
		"expires_in_seconds":       600,
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return ""
	}
	// The user just typed "task" at the inline prompt — they are
	// definitionally at the chat surface. Pass ?surface=inline so the
	// proxy holds the yes/no gesture inline rather than routing
	// to the dashboard's notification queue.
	//
	// Use the single-curl `--data @- <<JSON` shape. The proxy DOES
	// accept a cat-heredoc-to-file then curl --data @file pattern, but
	// it's strictly more error-prone — keep the prompt to one shape.
	//
	// RUN IT IN THE FOREGROUND. The task-creation curl must block on
	// my decision; backgrounding it makes the agent proceed before
	// approval lands. Avoid Codex-specific parameter names in the
	// prompt — naming yield_time_ms tends to make the model set it
	// to a small default. The proxy clamps the parameter to a safe
	// minimum as a belt-and-suspenders fallback.
	return "Please request a Clawvisor task for this work using the proxy-lite control endpoint. The user will need to approve the task after it is created. Your next assistant message must be exactly one shell tool_use that runs the foreground curl below, then waits for the result. Do not print, describe, or summarize the JSON in chat. Do not answer with a markdown code block. Do not background it, do not split it across shells, do not poll a backgrounded session. POST the task definition to `https://clawvisor.local/control/tasks?surface=inline` so I can approve it without leaving the chat. Include the blocked action and any related tools or commands you expect to need. For normal temporary work, omit `lifetime` or set `\"lifetime\":\"session\"` with `expires_in_seconds`. Use `\"lifetime\":\"standing\"` only when the user explicitly wants persistent permission; standing tasks must not include `expires_in_seconds`.\n\nRun this exact command as one foreground shell tool call (JSON via `--data @-` heredoc, no intermediate file, no trailing `&`, no `nohup`):\n\ncurl -sS -X POST 'https://clawvisor.local/control/tasks?surface=inline' \\\n  -H 'Content-Type: application/json' \\\n  --data @- <<'JSON'\n" + string(raw) + "\nJSON"
}

// taskToolWhy renders a default `why` for the model when the blocked
// tool is being lifted into a fresh task definition. The text is
// intentionally expansive about read/verify follow-ups so the LLM
// intent verifier (which compares each tool_use to the matched
// action's `why`) doesn't refuse the natural after-write inspect
// commands an agent does to confirm its own work.
func taskToolWhy(tu conversation.ToolUse) string {
	switch strings.TrimSpace(tu.Name) {
	case "Bash", "bash", "exec_command":
		if command := toolInputString(tu.Input, "command", "cmd"); command != "" {
			return "Run shell commands needed for the task, including writes AND verification reads (ls, wc, cat, stat) against the resulting files. Initial command: " + command
		}
	case "Read":
		if path := toolInputString(tu.Input, "file_path", "path"); path != "" {
			return "Read files needed for the task, including: " + path
		}
	case "Write", "Edit", "NotebookEdit":
		if path := toolInputString(tu.Input, "file_path", "path"); path != "" {
			return "Create, modify, and read back files needed for the task (verifying writes is part of the workflow), including: " + path
		}
	case "WebFetch", "WebSearch":
		if target := toolInputString(tu.Input, "url", "query"); target != "" {
			return "Use web access needed for the task, including: " + target
		}
	}
	return "Use this tool for the requested task. Include a concise description of the command pattern, file path, URL, or operation; if writing or modifying, also cover the read-back verification you will do afterward."
}

func toolInputString(raw json.RawMessage, keys ...string) string {
	if len(raw) == 0 {
		return ""
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return ""
	}
	for _, key := range keys {
		if v, ok := input[key].(string); ok {
			v = strings.TrimSpace(v)
			if v != "" {
				return v
			}
		}
	}
	return ""
}
