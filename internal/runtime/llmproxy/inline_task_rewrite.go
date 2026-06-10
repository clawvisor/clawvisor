package llmproxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

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
	ConversationID  string
	PendingApproval PendingApprovalCache
	// Creator is the handlers-side helper that creates the task in
	// the store with surface=inline_chat. Required for approve;
	// optional for deny (deny doesn't touch the store).
	Creator InlineTaskCreator
	Audit   *AuditEmitter
	// RequestID is forwarded into the audit row produced when the
	// rewrite resolves an inline task.
	RequestID string
	// Outcomes records the success/failure of each approval keyed by
	// the inner hold's approval ID so the history augmenter on later
	// turns can re-inject the correct context (success vs. failure)
	// instead of blindly claiming the task was created.
	Outcomes InlineApprovalOutcomeStore
	// Checkouts stores the created task as the agent's current focus on
	// successful inline approval. Checkout is a routing preference only.
	Checkouts TaskCheckoutStore
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
	// Credentials are the placeholders minted for task credential
	// access on a successful approve.
	Credentials []InlineTaskCredentialPlaceholder
	// ApprovalRecordID is the canonical approval_records row id
	// created at the same time as the task. Useful for audit traces.
	ApprovalRecordID string
	// CheckedOut reports whether the task was stored as current focus.
	CheckedOut bool
}

// RewriteInlineTaskApprovalReply consumes an awaiting_task_approval
// hold when the user's most recent message is yes/no,
// creates the task (on approve), drops the linked outer tool hold,
// and rewrites the user message to include task-creation context.
//
// This replaces the prior synth-tool_use approach. By rewriting the
// user message and letting the request flow to the LLM, we:
//
//   - Avoid fabricating an assistant tool_use the LLM never authored
//     (which previously confused the model into re-POSTing /api/control/tasks).
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
	editor, ok := newApprovalBodyEditor(req.HTTPRequest, req.Provider, req.Body)
	if !ok {
		return InlineApprovalRewriteResult{Body: req.Body}, nil
	}
	verb, approvalID, ok := editor.LatestApprovalReply()
	if !ok || (verb != "approve" && verb != "deny") {
		return InlineApprovalRewriteResult{Body: req.Body}, nil
	}

	action, err := resolveApprovalReplyAction(ctx, approvalReplyRoutingRequest{
		UserID:          req.Agent.UserID,
		AgentID:         req.Agent.ID,
		Provider:        req.Provider,
		ConversationID:  req.ConversationID,
		PendingApproval: req.PendingApproval,
		Verb:            verb,
		ApprovalID:      approvalID,
	})
	if err != nil {
		return InlineApprovalRewriteResult{Body: req.Body}, err
	}
	switch action.Kind {
	case approvalReplyActionApproveInlineTask,
		approvalReplyActionDenyInlineTask,
		approvalReplyActionApproveInlineExpansion,
		approvalReplyActionDenyInlineExpansion:
		// handled below
	default:
		// Most recent hold isn't an inline-task or inline-expansion
		// hold (or the named approval isn't one). Defer to
		// TryReleasePendingApproval.
		return InlineApprovalRewriteResult{Body: req.Body}, nil
	}
	if action.Hold == nil {
		return InlineApprovalRewriteResult{Body: req.Body}, nil
	}

	// Pre-flight: confirm we can rewrite the body BEFORE touching
	// ANY mutable state (cache, store). A "yes verb, no rewritable
	// shape" outcome (unsupported provider, body parsed but shape
	// unexpected) used to consume the inner hold and drop the outer
	// before failing — stranding the user with no recoverable cache
	// entries. Probe up front; if the shape can't be rewritten,
	// fail closed without disturbing the cache so a fixed retry can
	// drive the flow.
	out := InlineApprovalRewriteResult{Body: req.Body}
	// Probe with the same ApprovalID expectation we'll enforce on the
	// real replacement below — without this, a probe that succeeded
	// against a verb-matching but ID-mismatching message would
	// proceed past the can-rewrite gate and fail at the second
	// rewrite, mid-side-effect.
	expectedApprovalID := ""
	if action.Hold != nil {
		expectedApprovalID = action.Hold.ID
	}
	_, canRewrite, probeErr := editor.ReplaceLatestUserText(verb, expectedApprovalID, "")
	if probeErr != nil {
		return out, probeErr
	}
	if !canRewrite {
		out.Decision = "deny"
		out.Outcome = "inline_task_body_rewrite_unsupported"
		out.Reason = "could not rewrite user message in current request body shape"
		// Deliberately DO NOT record an outcome: the hold is still in
		// the cache for retry, and recording a failure under inner.ID
		// would poison the augmenter on the next turn. The augmenter
		// would look up the same ID, find the stale failure, and inject
		// "task creation was NOT completed" onto a fresh approval that
		// might still succeed. No outcome → augmenter skips → the
		// retry runs clean.
		return out, nil
	}

	resolved, err := consumeApprovalActionHold(ctx, req.PendingApproval, req.Agent, req.Provider, req.ConversationID, action)
	if err != nil {
		return InlineApprovalRewriteResult{Body: req.Body}, err
	}
	if resolved == nil {
		return InlineApprovalRewriteResult{Body: req.Body}, nil
	}

	// Drop the linked outer tool hold so it doesn't sit in the cache
	// re-matching subsequent approval prompts. The model will re-emit
	// the original tool naturally now that the task scope covers it.
	dropLinkedToolHold(ctx, req.PendingApproval, req.Agent, req.Provider, req.ConversationID, resolved)
	var replacement string
	switch action.Kind {
	case approvalReplyActionApproveInlineExpansion, approvalReplyActionDenyInlineExpansion:
		replacement, out = resolveInlineExpansionApproval(ctx, req, resolved, verb)
	default:
		replacement, out = resolveInlineTaskApproval(ctx, req, resolved, verb)
	}

	// Record the outcome before returning. The augmenter on later
	// turns reads this to decide whether to inject success or failure
	// context — without it, every "approve" in conversation history
	// would be augmented as success even when creation failed.
	if req.Outcomes != nil {
		req.Outcomes.Record(InlineApprovalOutcomeKey{
			UserID:     req.Agent.UserID,
			AgentID:    req.Agent.ID,
			ApprovalID: resolved.ID,
		}, inlineApprovalOutcomeFromRewrite(req.RequestID, out))
	}

	resolvedApprovalID := ""
	if resolved != nil {
		resolvedApprovalID = resolved.ID
	}
	rewritten, ok, err := editor.ReplaceLatestUserText(verb, resolvedApprovalID, replacement)
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

func inlineApprovalOutcomeFromRewrite(requestID string, out InlineApprovalRewriteResult) InlineApprovalOutcome {
	// Kind is derived from the outcome string the resolver emitted —
	// outcomes starting with "inline_expansion_" came from the
	// expansion path, anything else is the task-creation path. The
	// augmenter dispatches on Kind so a previously-approved expansion
	// renders "scope was expanded" history rather than the task-
	// creation "Task was created" body.
	kind := InlineApprovalOutcomeKindTaskCreate
	if strings.HasPrefix(out.Outcome, "inline_expansion_") {
		kind = InlineApprovalOutcomeKindTaskExpand
	}
	return InlineApprovalOutcome{
		Decision:         out.Decision,
		Outcome:          out.Outcome,
		Succeeded:        out.Decision == "allow",
		TaskID:           out.TaskID,
		Credentials:      out.Credentials,
		ApprovalRecordID: out.ApprovalRecordID,
		CheckedOut:       out.CheckedOut,
		FailureReason:    out.Reason,
		RequestID:        requestID,
		ResolvedAt:       time.Now().UTC(),
		Kind:             kind,
	}
}

// Notice kinds for the user-role injections the proxy substitutes into
// inline-task approval turns. These land in user-role messages the LLM
// reads (not in assistant text the human sees), so the structured
// <clawvisor-notice> envelope is the right wire shape: it lets the
// model recognize the channel as proxy-emitted control-plane state
// distinct from anything the user actually typed.
const (
	NoticeKindTaskApproved NoticeKind = "task-approved"
	NoticeKindTaskDenied   NoticeKind = "task-denied"
	NoticeKindTaskError    NoticeKind = "task-error"
)

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
// context, leading to duplicate /api/control/tasks POSTs and other
// confusions.
//
// This function runs on every request as a no-op-or-augment pass. It
// rewrites in place, idempotent across calls (a previously-augmented
// turn skips on subsequent passes via the augmentation marker).
//
// Returns (body, rewritten, err). When no qualifying turns are found,
// returns the body unchanged with rewritten=false.
//
// outcomes lets the augmenter distinguish a previously-successful
// approval from a previously-failed one. The renderTaskApprovalPrompt
// footer embeds the approval ID; we parse it here and look up the
// outcome RewriteInlineTaskApprovalReply recorded on the original turn.
// Outcomes nil or "unknown" → skip augmentation, since we can't safely
// claim either success or failure.
//
// userID/agentID scope the lookup. Outcomes are recorded under
// (userID, agentID, approvalID); a model in agent B can't replay an
// approval ID from agent A and steer the augmenter — purely a
// model-confusion vector since real authorization runs against the
// task store, but consistent with the rest of the approval scoping.
func AugmentApprovedInlineTasksInHistory(body []byte, provider conversation.Provider, outcomes InlineApprovalOutcomeStore, userID, agentID string) ([]byte, bool, error) {
	editor, ok := newApprovalBodyEditor(nil, provider, body)
	if !ok {
		return body, false, nil
	}
	return editor.AugmentInlineApprovalHistory(outcomes, userID, agentID)
}

// augmentationContextForOutcome maps an outcome lookup to the bracketed
// context the augmenter should inject after the user's "approve". The
// store argument is treated nil-safe so call sites in tests and any
// transitional code without an outcome store still compile cleanly —
// nil store always returns ok=false (skip augmentation).
func augmentationContextForOutcome(key InlineApprovalOutcomeKey, store InlineApprovalOutcomeStore) (string, bool) {
	if store == nil || key.ApprovalID == "" {
		return "", false
	}
	outcome, ok := store.Lookup(key)
	if !ok {
		return "", false
	}
	if outcome.Succeeded {
		// Dispatch on Kind so a previously-approved expansion
		// renders the "scope was expanded" body (which tells the
		// model the parent task still exists with broader scope)
		// rather than the task-creation body (which would mislead
		// the model into thinking a fresh task was just minted).
		if outcome.Kind == InlineApprovalOutcomeKindTaskExpand {
			return inlineExpansionApprovedReplyAugmentationContext(outcome.TaskID, outcome.Credentials), true
		}
		return inlineApprovedReplyAugmentationContext(outcome.TaskID, outcome.CheckedOut, outcome.Credentials), true
	}
	return inlineFailedReplyAugmentationContext(outcome.FailureReason), true
}

// inlineFailedReplyAugmentationContext is the persistent-history
// counterpart to renderInlineTaskCreatorErrorReply. Tells the model
// the previously-approved task was NOT created so it doesn't proceed
// as if scope were granted.
func inlineFailedReplyAugmentationContext(reason string) string {
	reason = sanitizeInlineTaskNoticeBody(reason)
	if reason == "" {
		reason = "creation failed"
	}
	body := "Task creation was NOT completed (" + reason + "). No task is active; the originally-requested tool call is still out of scope. Acknowledge the failure to the user; do not retry without changes."
	return Render(NoticeKindTaskError, body)
}

// sanitizeInlineTaskNoticeBody trims and length-caps model-controlled
// strings before they're rendered inside a <clawvisor-notice> body.
// Newlines collapse to spaces so the parser's line-by-line scan can't
// pick up a stray verb line from an embedded paragraph break, and the
// length cap keeps one runaway error from drowning the model in noise.
// XML escaping of `<`, `>`, `&` is handled by Render; this sanitizer
// is purely about layout / size.
func sanitizeInlineTaskNoticeBody(reason string) string {
	reason = strings.TrimSpace(reason)
	reason = strings.ReplaceAll(reason, "\r", " ")
	reason = strings.ReplaceAll(reason, "\n", " ")
	const maxLen = 256
	if len(reason) > maxLen {
		reason = reason[:maxLen] + "…"
	}
	return strings.TrimSpace(reason)
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
// The verb itself is NOT included — the rewrite replaces the user's
// "approve" message wholesale with this bracketed context. Leaving
// "approve" on its own line would still parse as a fresh bare
// approval to a downstream re-parse; the bracketed text fully
// conveys what happened ("created and approved by the user")
// without that sharp edge.
func inlineApprovedReplyAugmentation() string {
	return inlineApprovedReplyAugmentationContext("", false, nil)
}

// inlineApprovedReplyAugmentationContext is the body shared between
// the one-shot rewrite and the persistent augmenter. Wrapped in a
// <clawvisor-notice kind="task-approved"> envelope by Render so the
// LLM recognizes the substituted user-role turn as proxy-emitted
// control-plane state rather than a fresh user instruction.
func inlineApprovedReplyAugmentationContext(taskID string, checkedOut bool, credentials []InlineTaskCredentialPlaceholder) string {
	var b strings.Builder
	b.WriteString("Task was created and approved by the user. The task covers the originally requested work; proceed by emitting your next tool_use(s). Do NOT POST /control/tasks to CREATE another task for the same body of work. If an earlier tool_use already completed successfully, do NOT re-emit it; move on to the next step using the results above.")
	if strings.TrimSpace(taskID) != "" {
		b.WriteString(" Task ID: ")
		b.WriteString(strings.TrimSpace(taskID))
		b.WriteString(".")
		// Action-oriented expand pointer with the just-created task_id
		// substituted. The system prompt teaches EXPAND vs NEW TASK at
		// the top of the control notice, but the immediate decision
		// point is several thousand tokens away by the time the model
		// reads it on a later turn — this concrete URL with the
		// agent's actual task_id removes the recall-and-substitute
		// burden. The DEFAULT for any follow-up ask MUST be
		// EXPAND-this-task; only a recognizably-different goal earns
		// a new task. The choice is forced because the prior
		// "Do NOT POST /control/tasks" guidance was too easy to
		// satisfy by just emitting another /control/tasks POST anyway.
		b.WriteString(" For the user's NEXT ask in this conversation, CHOOSE between two actions before emitting any tool_use: (A) follow-up in the SAME body of work (additional steps under the task's stated purpose — comments, replies, verifications, further API calls, additional tools/hosts/credentials) → POST https://clawvisor.local/control/tasks/")
		b.WriteString(strings.TrimSpace(taskID))
		b.WriteString("/expand?surface=inline against THIS task; (B) genuinely-different goal (purpose no longer describes the work) → POST a new /control/tasks?surface=inline. Default to (A) when in doubt. Never silently create a second task for related work.")
	}
	if checkedOut && strings.TrimSpace(taskID) != "" {
		b.WriteString(" Task ")
		b.WriteString(strings.TrimSpace(taskID))
		b.WriteString(" is now the active task. To switch active tasks, fetch https://clawvisor.local/control/tasks, then POST https://clawvisor.local/control/task/checkout with the target task_id.")
	}
	if len(credentials) > 0 {
		b.WriteString(" Credential placeholders granted for this task:")
		for _, cred := range credentials {
			if strings.TrimSpace(cred.Placeholder) == "" {
				continue
			}
			name := strings.TrimSpace(cred.VaultItemID)
			if name == "" {
				name = strings.TrimSpace(cred.ServiceID)
			}
			if name == "" {
				name = "credential"
			}
			b.WriteString(" ")
			b.WriteString(name)
			b.WriteString("=")
			b.WriteString(cred.Placeholder)
			b.WriteString(";")
		}
		b.WriteString(" use these exact placeholder values in Authorization headers or curl arguments.")
	}
	return Render(NoticeKindTaskApproved, b.String())
}

// renderInlineTaskDenyReply is the user-message text the LLM sees
// in place of bare "deny cv-xxx". Tells the model to stop and not
// retry the task-creation request. No leading verb — see the
// inlineApprovedReplyAugmentation comment for why.
func renderInlineTaskDenyReply() string {
	return Render(NoticeKindTaskDenied, "The user denied the task-creation request. Do not retry. Acknowledge the denial; stop unless the user issues a new request.")
}

// renderInlineTaskCreatorErrorReply is used when the user approved
// but the task could not be created (validation error, store error,
// missing creator wiring). The LLM should treat this as a denial and
// surface the failure to the user.
func renderInlineTaskCreatorErrorReply(msg string) string {
	msg = sanitizeInlineTaskNoticeBody(msg)
	return Render(NoticeKindTaskError, fmt.Sprintf("Task creation failed — %s. Acknowledge the failure to the user; do not retry without changes.", msg))
}

// renderInlineTaskAlreadyTerminalReply is used when the chat-side
// "approve" reply arrives after the task has already been terminated
// from a non-chat surface (dashboard / notifier Deny, the 24h expiry
// sweep, manual revocation). The model needs to understand it lost a
// race — the user already dismissed this work elsewhere — and surface
// that to the user rather than acting as though the approval landed
// or retrying the same task body.
func renderInlineTaskAlreadyTerminalReply(status string) string {
	verb := "denied"
	switch status {
	case "expired":
		verb = "let lapse"
	case "revoked":
		verb = "revoked"
	}
	return Render(NoticeKindTaskError, fmt.Sprintf("Task creation failed — the user %s this task on another surface (dashboard or notifier) before your approval landed. Acknowledge to the user that the task is gone; if they still want the work done, ask them to issue a fresh request — do NOT retry the same task body.", verb))
}
