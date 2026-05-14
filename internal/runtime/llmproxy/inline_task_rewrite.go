package llmproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// InlineApprovalRewriteRequest is the input to
// RewriteInlineTaskApprovalReply. Parallel shape to
// TaskReplyRewriteRequest — both run in the request preprocess
// before the LLM call.
type InlineApprovalRewriteRequest struct {
	HTTPRequest     *http.Request
	Provider        conversation.Provider
	Body            []byte
	Agent           *store.Agent
	PendingApproval PendingApprovalCache
	// Creator is the handlers-side helper that creates the task in
	// the store with surface=inline_chat. Required for approve;
	// optional for deny (deny doesn't touch the store).
	Creator InlineTaskCreator
	Audit   *AuditEmitter
	// RequestID is forwarded into the audit row produced when the
	// rewrite resolves an inline task.
	RequestID string
}

// InlineApprovalRewriteResult reports what happened. When Rewritten
// is true, the body has been replaced and the request should flow to
// the LLM with the new body. The Decision/Outcome/Reason fields go
// into the handler's audit_params so the audit row records the
// inline gesture.
type InlineApprovalRewriteResult struct {
	Body      []byte
	Rewritten bool
	// Decision is "allow" on a successful approve, "deny" on any
	// failure path (deny verb, missing creator, creator error).
	Decision string
	// Outcome is the short audit-event tag.
	Outcome string
	// Reason is the human-readable explanation included in the audit
	// row when something went wrong. Empty on success.
	Reason string
	// TaskID is the created task's ID on a successful approve.
	TaskID string
	// ApprovalRecordID is the canonical approval_records row id
	// created at the same time as the task. Useful for audit traces.
	ApprovalRecordID string
}

// RewriteInlineTaskApprovalReply consumes an awaiting_task_approval
// hold when the user's most recent message is "approve" or "deny",
// creates the task (on approve), drops the linked outer tool hold,
// and rewrites the user message to include task-creation context.
//
// This replaces the prior synth-tool_use approach. By rewriting the
// user message and letting the request flow to the LLM, we:
//
//   - Avoid fabricating an assistant tool_use the LLM never authored
//     (which previously confused the model into re-POSTing /control/tasks).
//   - Avoid spoofing the harness into running shell commands the
//     model didn't actually emit.
//   - Give the LLM a clean conversation state with explicit context
//     ("task X created and active; proceed; do NOT re-POST").
//
// When the user's hold isn't an inline-task hold (e.g. a regular
// tool-stage approval), this returns (body, Rewritten=false, nil)
// and the existing TryReleasePendingApproval handles it unchanged.
func RewriteInlineTaskApprovalReply(ctx context.Context, req InlineApprovalRewriteRequest) (InlineApprovalRewriteResult, error) {
	if req.PendingApproval == nil || req.Agent == nil {
		return InlineApprovalRewriteResult{Body: req.Body}, nil
	}
	verb, approvalID := conversation.ApprovalReplyForProvider(req.Provider, req.Body)
	if verb != "approve" && verb != "deny" {
		return InlineApprovalRewriteResult{Body: req.Body}, nil
	}

	// Target the inline-task hold specifically. Without the Stage
	// filter, an older unresolved tool-stage hold (e.g. from a
	// previous intent_refusal the user didn't reply to) would
	// shadow the inline-task hold and the user's bare "approve"
	// would resolve the stale tool hold instead of creating the
	// task. Production trace 2026-05-14T04:03:23 reproduced exactly
	// this — fail-mode in two parts: no task in the dashboard, and
	// the next agent turn hits task_scope_missing.
	inner, err := req.PendingApproval.Peek(ctx, ResolveRequest{
		UserID:     req.Agent.UserID,
		AgentID:    req.Agent.ID,
		Provider:   req.Provider,
		ApprovalID: approvalID,
		Stage:      StageAwaitingTaskApproval,
	})
	if err != nil {
		return InlineApprovalRewriteResult{Body: req.Body}, err
	}
	if inner == nil {
		// No matching inline-task hold; this is a regular
		// tool-stage approve/deny and TryReleasePendingApproval
		// will handle it.
		return InlineApprovalRewriteResult{Body: req.Body}, nil
	}

	// Consume the inner hold so TryReleasePendingApproval doesn't
	// double-handle it.
	resolved, err := req.PendingApproval.Resolve(ctx, ResolveRequest{
		UserID:     req.Agent.UserID,
		AgentID:    req.Agent.ID,
		Provider:   req.Provider,
		ApprovalID: inner.ID,
	})
	if err != nil {
		return InlineApprovalRewriteResult{Body: req.Body}, err
	}
	if resolved == nil {
		return InlineApprovalRewriteResult{Body: req.Body}, nil
	}

	// Drop the linked outer tool hold so it doesn't sit in the cache
	// re-matching subsequent approval prompts. The model will re-emit
	// the original tool naturally now that the task scope covers it.
	if resolved.AwaitingTaskFor != "" {
		_ = req.PendingApproval.Drop(ctx, ResolveRequest{
			UserID:     req.Agent.UserID,
			AgentID:    req.Agent.ID,
			Provider:   req.Provider,
			ApprovalID: resolved.AwaitingTaskFor,
		})
	}

	var replacement string
	out := InlineApprovalRewriteResult{Body: req.Body}

	if verb == "deny" {
		replacement = renderInlineTaskDenyReply()
		out.Decision = "deny"
		out.Outcome = "inline_task_denied"
		out.Reason = "user denied inline task"
	} else {
		// approve
		switch {
		case req.Creator == nil:
			replacement = renderInlineTaskCreatorErrorReply("inline task creation is not available on this daemon")
			out.Decision = "deny"
			out.Outcome = "inline_task_creator_missing"
			out.Reason = "no inline task creator configured"
		case resolved.TaskDefinition == nil:
			replacement = renderInlineTaskCreatorErrorReply("missing task definition on approval")
			out.Decision = "deny"
			out.Outcome = "inline_task_definition_missing"
			out.Reason = "missing task definition on approval"
		default:
			originalToolUseID := resolved.AwaitingTaskFor
			created, createErr := req.Creator.CreateInlineApprovedTask(ctx, req.Agent, resolved.TaskDefinition, originalToolUseID)
			if createErr != nil {
				replacement = renderInlineTaskCreatorErrorReply(createErr.Error())
				out.Decision = "deny"
				out.Outcome = "inline_task_create_failed"
				out.Reason = "create failed: " + createErr.Error()
			} else {
				// Use the SAME text the persistent augmenter
				// produces on subsequent turns. If turn 1 said
				// "task 7827... purpose=..." and turn 2+ said
				// "task was created and approved...", the model
				// sees the same user message appear with DIFFERENT
				// content across turns — measurable drift that
				// invites the model to second-guess prior state.
				// One canonical rendering, no drift.
				replacement = inlineApprovedReplyAugmentation()
				out.Decision = "allow"
				out.Outcome = "inline_task_approved"
				out.TaskID = created.ID
				out.ApprovalRecordID = created.ApprovalRecordID
				if req.Audit != nil {
					req.Audit.LogInlineTaskApproved(ctx, req.Agent, req.RequestID, resolved, created)
				}
			}
		}
	}

	rewritten, ok, err := replaceApprovalReplyForProvider(req.HTTPRequest, req.Provider, req.Body, verb, replacement)
	if err != nil {
		return out, err
	}
	if !ok {
		// Couldn't rewrite (unsupported provider or unexpected body
		// shape). Hold is already consumed; return the original body
		// but mark the outcome so the audit row records what happened.
		return out, nil
	}
	out.Body = rewritten
	out.Rewritten = true
	return out, nil
}

// InlineApprovalSubstitutedPromptMarker is the leading phrase of the
// assistant text we substitute in place of a model-emitted POST
// /control/tasks tool_use. The persistent-history rewriter looks for
// this marker to find user "approve" turns that need their context
// re-injected on every subsequent request.
const InlineApprovalSubstitutedPromptMarker = "Clawvisor wants to create a task to cover this work:"

// InlineApprovalAugmentationMarker is a tag we embed in the rewritten
// user message so subsequent passes can detect that a turn was
// already augmented and skip it. Avoids double-augmentation across
// retries / multi-step preprocess pipelines.
const InlineApprovalAugmentationMarker = "[Clawvisor: inline task"

// AugmentApprovedInlineTasksInHistory walks the conversation history
// and re-injects the "[Clawvisor: ... task approved inline ...]"
// context onto every user "approve" turn that follows the substituted
// task-approval prompt.
//
// Why this is needed: our one-shot rewrite in
// RewriteInlineTaskApprovalReply only persists for a single LLM call
// — the harness records what the user actually typed ("approve"), not
// our transit-rewritten version. On subsequent turns the conversation
// history shows bare "approve" and the model loses the task-creation
// context, leading to duplicate /control/tasks POSTs and other
// confusions.
//
// This function runs on every request as a no-op-or-augment pass. It
// rewrites in place, idempotent across calls (a previously-augmented
// turn skips on subsequent passes via the augmentation marker).
//
// Returns (body, rewritten, err). When no qualifying turns are found,
// returns the body unchanged with rewritten=false.
func AugmentApprovedInlineTasksInHistory(body []byte, provider conversation.Provider) ([]byte, bool, error) {
	switch provider {
	case conversation.ProviderAnthropic:
		return augmentAnthropicApprovedInlineTasks(body)
	case conversation.ProviderOpenAI:
		// OpenAI Chat / Responses can share the same persistence work
		// once we have a reproducer there. For now we keep the
		// rewrite Anthropic-only — Claude Code is the harness this
		// matters for and it's the one we've observed losing the
		// context.
		return body, false, nil
	default:
		return body, false, nil
	}
}

func augmentAnthropicApprovedInlineTasks(body []byte) ([]byte, bool, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body, false, err
	}
	rawMessages, ok := raw["messages"]
	if !ok {
		return body, false, nil
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(rawMessages, &messages); err != nil {
		return body, false, err
	}

	persistentNote := inlineApprovedReplyAugmentationContext()
	changed := false

	for i := 1; i < len(messages); i++ {
		// Current must be user role.
		var role string
		if err := json.Unmarshal(messages[i]["role"], &role); err != nil || role != "user" {
			continue
		}
		// Current content must parse to bare "approve" (the harness
		// records exactly what the user typed — bare "approve" or
		// "approve cv-xxx").
		userText := flattenAnthropicTaskReplyText(messages[i]["content"])
		verb, _ := conversation.ParseApprovalReplyText(userText)
		if verb != "approve" {
			continue
		}
		// Skip if we've already augmented this turn — the bracketed
		// context contains a recognizable marker. Idempotency.
		if strings.Contains(userText, InlineApprovalAugmentationMarker) {
			continue
		}

		// Prior message must be assistant whose text starts with the
		// substituted-prompt marker. That's how we know this approve
		// was an inline task gesture (vs. a regular tool approval).
		var priorRole string
		if err := json.Unmarshal(messages[i-1]["role"], &priorRole); err != nil || priorRole != "assistant" {
			continue
		}
		priorText := flattenAnthropicTaskReplyText(messages[i-1]["content"])
		if !strings.Contains(priorText, InlineApprovalSubstitutedPromptMarker) {
			continue
		}

		// Rewrite this user message's content with the persistent
		// note appended. Preserve the original "approve" verb so the
		// model can still see the user's actual reply.
		newText := strings.TrimRight(userText, " \t\n") + "\n\n" + persistentNote
		encoded, _ := json.Marshal(newText)
		messages[i]["content"] = encoded
		changed = true
	}

	if !changed {
		return body, false, nil
	}
	updatedMessages, err := json.Marshal(messages)
	if err != nil {
		return body, false, err
	}
	raw["messages"] = updatedMessages
	out, err := json.Marshal(raw)
	if err != nil {
		return body, false, err
	}
	return out, true, nil
}

// inlineApprovedReplyAugmentation is the SINGLE canonical bracketed
// context that both the one-shot RewriteInlineTaskApprovalReply and
// the persistent AugmentApprovedInlineTasksInHistory inject. Both
// must produce byte-identical output so the model never sees the
// same user "approve" turn render differently across calls.
//
// We intentionally omit per-task specifics (task_id, purpose,
// lifetime). The augmenter scans conversation history without DB
// access, so it can't reconstruct those fields without a store
// lookup; the one-shot path COULD include them on turn 1, but then
// turn 2+ would diverge. Drift hurts the model more than the missing
// specifics help — the model doesn't need the task_id to behave
// correctly, only "task is active, don't re-POST, don't re-emit
// successful tool_uses".
//
// Returns just the bracketed context. Callers prepend "approve\n\n"
// (or "deny\n\n") to keep the verb intact for the parser.
func inlineApprovedReplyAugmentation() string {
	return "approve\n\n" + inlineApprovedReplyAugmentationContext()
}

// inlineApprovedReplyAugmentationContext is the bracketed body,
// without the leading "approve" verb. Shared with the augmenter so
// it can prepend the verb-as-typed (always "approve" today, but
// kept symmetrical with the deny path for parser robustness).
func inlineApprovedReplyAugmentationContext() string {
	return InlineApprovalAugmentationMarker + " was created and approved by the user inline. Approval source: inline_chat. The task covers the originally requested work; proceed by emitting your next tool_use(s). Do NOT POST /control/tasks again for the same work — that would create a duplicate task. If your earlier tool_use already completed successfully (you can see a successful tool_result above), do NOT re-emit it; move on to the next step.]"
}

// renderInlineTaskDenyReply is the user-message text the LLM sees
// in place of bare "deny cv-xxx". Tells the model to stop and not
// retry the task-creation request.
func renderInlineTaskDenyReply() string {
	return "deny\n\n[Clawvisor: the user denied the task-creation request. Do not retry. Acknowledge the denial; stop unless the user issues a new request.]"
}

// renderInlineTaskCreatorErrorReply is used when the user approved
// but the task could not be created (validation error, store error,
// missing creator wiring). The LLM should treat this as a denial and
// surface the failure to the user.
func renderInlineTaskCreatorErrorReply(msg string) string {
	return fmt.Sprintf("deny\n\n[Clawvisor: inline task creation failed — %s. Acknowledge the failure to the user; do not retry without changes.]", msg)
}
