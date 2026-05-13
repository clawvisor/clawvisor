package llmproxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
)

// inlineTaskApprovalTTL is how long the user has to type approve/deny
// after the model has emitted the task definition. Bounded so the second
// gesture sits within the same approval cache window as the first; if
// the user walks away mid-flight both holds expire together and the
// model's next turn naturally re-prompts.
const inlineTaskApprovalTTL = 10 * time.Minute

// maybeInterceptInlineTaskDefinition is the postprocess hook for step 4
// of the inline task approval state machine:
//
//	(prior turn) user typed "task" → original tool hold is now in
//	             StageAwaitingTaskDefinition.
//	(this turn) model emitted POST /control/tasks tool_use carrying the
//	             task body.
//
// If we find a matching awaiting-definition hold, we:
//  1. parse the task body
//  2. register a NEW hold (StageAwaitingTaskApproval) linking back to
//     the original via AwaitingTaskFor, carrying the parsed task body
//  3. refresh the original hold's TTL so the two-step gesture sits in a
//     single approval-cache window
//  4. substitute the tool_use's response with the rendered approve/deny
//     prompt — the model never actually POSTs to /control/tasks
//
// Returns (verdict, true) when the interception fired. Returns
// (_, false) when no matching hold exists, the body fails to parse,
// or the path isn't POST /control/tasks — callers should fall through
// to the regular control-rewrite path so async (non-inline) task
// creation routes through the dashboard handler unchanged.
func maybeInterceptInlineTaskDefinition(
	req *http.Request,
	cfg PostprocessConfig,
	audit func(decision, outcome, reason string),
	trace func(event string, kv ...any),
	provider conversation.Provider,
	tu conversation.ToolUse,
	call ControlCall,
) (conversation.ToolUseVerdict, bool) {
	if cfg.PendingApprovals == nil {
		return conversation.ToolUseVerdict{}, false
	}
	// Only intercept POSTs to /control/tasks; the dashboard handler
	// covers GETs (skill catalog) and other control paths.
	if !strings.EqualFold(call.Method, "POST") || !strings.HasSuffix(call.URL.Path, "/control/tasks") {
		return conversation.ToolUseVerdict{}, false
	}

	pending, err := cfg.PendingApprovals.Peek(req.Context(), ResolveRequest{
		UserID:   cfg.AgentUserID,
		AgentID:  cfg.AgentID,
		Provider: provider,
	})
	if err != nil || pending == nil || pending.Stage != StageAwaitingTaskDefinition {
		return conversation.ToolUseVerdict{}, false
	}

	bodyBytes, ok := controlTaskBodyFromInput(tu.Input)
	if !ok || len(bodyBytes) == 0 {
		audit("block", "inline_task_body_missing", "POST /control/tasks had no body")
		return conversation.ToolUseVerdict{}, false
	}
	parsed := &runtimetasks.TaskCreateRequest{}
	if err := json.Unmarshal(bodyBytes, parsed); err != nil {
		audit("block", "inline_task_body_malformed", err.Error())
		return conversation.ToolUseVerdict{}, false
	}
	if strings.TrimSpace(parsed.Purpose) == "" {
		audit("block", "inline_task_missing_purpose", "task body missing purpose")
		return conversation.ToolUseVerdict{}, false
	}

	// Refresh the original hold's TTL so the second gesture has the
	// full inlineTaskApprovalTTL window. Without this, an original hold
	// created near the end of its TTL could expire before the user
	// finishes the second approve.
	now := time.Now().UTC()
	if _, err := cfg.PendingApprovals.Update(req.Context(), UpdateRequest{
		UserID:     cfg.AgentUserID,
		AgentID:    cfg.AgentID,
		Provider:   provider,
		ApprovalID: pending.ID,
		ExpiresAt:  now.Add(inlineTaskApprovalTTL),
	}); err != nil {
		audit("block", "inline_task_refresh_failed", err.Error())
		return conversation.ToolUseVerdict{}, false
	}

	innerHold, holdErr := cfg.PendingApprovals.Hold(req.Context(), PendingLiteApproval{
		UserID:          cfg.AgentUserID,
		AgentID:         cfg.AgentID,
		Provider:        provider,
		ToolUse:         tu,
		Reason:          "inline task creation awaiting user approval",
		Stage:           StageAwaitingTaskApproval,
		AwaitingTaskFor: pending.ID,
		TaskDefinition:  parsed,
		CreatedAt:       now,
		ExpiresAt:       now.Add(inlineTaskApprovalTTL),
	})
	if holdErr != nil {
		audit("block", "inline_task_hold_failed", holdErr.Error())
		return conversation.ToolUseVerdict{}, false
	}

	audit("block", "inline_task_pending_approval", "awaiting user approve/deny on inline task definition")
	trace("inline_task.held",
		"approval_id", innerHold.Pending.ID,
		"awaiting_task_for", pending.ID,
		"purpose", parsed.Purpose,
	)
	return conversation.ToolUseVerdict{
		Allowed:        false,
		Reason:         "Clawvisor: awaiting inline task approval",
		SubstituteWith: renderTaskApprovalPrompt(parsed),
	}, true
}

// controlTaskBodyFromInput extracts the POST body from the tool_use's
// structured or command form. Mirrors ParseControlToolUseWithBase's
// reachable shapes but returns just the body bytes — the URL has
// already been classified by the caller. Routes through the shared
// parseControlCmd helper so both single-statement (curl with stdin
// heredoc) and multi-statement (cat-heredoc + curl --data @file)
// shapes resolve to the actual body bytes.
func controlTaskBodyFromInput(in json.RawMessage) ([]byte, bool) {
	if len(in) == 0 {
		return nil, false
	}
	// Structured form: { "url": "...", "method": "POST", "body": ... }
	var structured struct {
		Body json.RawMessage `json:"body,omitempty"`
	}
	if err := json.Unmarshal(in, &structured); err == nil && len(structured.Body) > 0 {
		var bodyString string
		if json.Unmarshal(structured.Body, &bodyString) == nil {
			return []byte(bodyString), true
		}
		return structured.Body, true
	}
	// Bash form: { "cmd"/"command": "..." }. Re-use the same parser the
	// rewrite path uses so single-stmt and cat-then-curl resolve
	// identically; controlPartsFromCommandInput already handles
	// @path → heredoc body substitution.
	if _, _, body, ok := controlPartsFromCommandInput(in, ""); ok && len(body) > 0 {
		return body, true
	}
	return nil, false
}
