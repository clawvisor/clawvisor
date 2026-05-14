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
	// Outcomes records the success/failure of each approval keyed by
	// the inner hold's approval ID so the history augmenter on later
	// turns can re-inject the correct context (success vs. failure)
	// instead of blindly claiming the task was created.
	Outcomes InlineApprovalOutcomeStore
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

	// Peek the MOST RECENT hold (LIFO, no stage filter). The user's
	// bare "approve" lands on whatever prompt the harness most
	// recently rendered — if that's an inline-task prompt, we
	// handle it here; if it's a regular tool prompt that just
	// happened to land after an older inline gesture, the release
	// path is the right place to consume it.
	//
	// Stage-filtering this Peek used to cause the inverse race:
	// an OLDER inline hold could steal a bare "approve" intended
	// for a NEWER tool prompt the user actually just saw.
	inner, err := req.PendingApproval.Peek(ctx, ResolveRequest{
		UserID:     req.Agent.UserID,
		AgentID:    req.Agent.ID,
		Provider:   req.Provider,
		ApprovalID: approvalID,
	})
	if err != nil {
		return InlineApprovalRewriteResult{Body: req.Body}, err
	}
	if inner == nil {
		return InlineApprovalRewriteResult{Body: req.Body}, nil
	}
	if inner.Stage != StageAwaitingTaskApproval {
		// Most recent hold isn't an inline-task hold (or the named
		// approval isn't one). Defer to TryReleasePendingApproval.
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
	_, canRewrite, probeErr := replaceApprovalReplyForProvider(req.HTTPRequest, req.Provider, req.Body, verb, "")
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

	// Record the outcome before returning. The augmenter on later
	// turns reads this to decide whether to inject success or failure
	// context — without it, every "approve" in conversation history
	// would be augmented as success even when creation failed.
	if req.Outcomes != nil {
		req.Outcomes.Record(resolved.ID, InlineApprovalOutcome{
			Succeeded:     out.Decision == "allow",
			TaskID:        out.TaskID,
			FailureReason: out.Reason,
		})
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
//
// outcomes lets the augmenter distinguish a previously-successful
// approval from a previously-failed one. The renderTaskApprovalPrompt
// footer embeds the approval ID; we parse it here and look up the
// outcome RewriteInlineTaskApprovalReply recorded on the original turn.
// Outcomes nil or "unknown" → skip augmentation, since we can't safely
// claim either success or failure.
func AugmentApprovedInlineTasksInHistory(body []byte, provider conversation.Provider, outcomes InlineApprovalOutcomeStore) ([]byte, bool, error) {
	switch provider {
	case conversation.ProviderAnthropic:
		return augmentAnthropicApprovedInlineTasks(body, outcomes)
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

func augmentAnthropicApprovedInlineTasks(body []byte, outcomes InlineApprovalOutcomeStore) ([]byte, bool, error) {
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

		// Decide the augmentation text by looking up the per-approval
		// outcome the rewrite path recorded on the original turn.
		// Failed approvals must NOT be re-rendered as success on later
		// turns — that would falsely tell the model the task is active.
		approvalID := extractApprovalIDFromPrompt(priorText)
		note, ok := augmentationContextForOutcome(approvalID, outcomes)
		if !ok {
			// Unknown outcome: prompt is from before the footer was
			// added, or the outcome cache evicted/never recorded.
			// Skip rather than guess.
			continue
		}

		// Rewrite this user message's content. Two shapes from the
		// harness:
		//
		//   - Bare string: replace with verb + "\n\n" + note. Verb is
		//     preserved at the start because the LLM should still see
		//     the user's actual reply.
		//
		//   - Array of blocks: any text block whose text parses as a
		//     bare verb is collapsed — the bare-verb LINE is stripped
		//     and the augmentation note takes its place. We do NOT
		//     leave the bare verb on its own line: a downstream parse
		//     of the augmented body would otherwise find "approve" and
		//     treat it as a fresh, unattributed approval (mirror of
		//     the same defense the handler's release-skip provides for
		//     the one-shot path). Non-text blocks (images,
		//     tool_results) and text blocks that DON'T parse as a verb
		//     are preserved.
		updated, ok := augmentUserContent(messages[i]["content"], verb, note)
		if !ok {
			// Shape we don't know how to edit safely. Skip rather
			// than risk losing image/tool_result blocks.
			continue
		}
		messages[i]["content"] = updated
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

// augmentUserContent rewrites a user message's content with the
// approval augmentation while preserving non-text blocks (images,
// tool_results) the harness may have included alongside.
//
// Both content shapes have the verb STRIPPED — neither string nor
// array form leaves a bare "approve" line behind. A downstream
// re-parse (release path, future augmenter pass, defensive scan) must
// never find a fresh-looking approval gesture in already-augmented
// content. The bracketed note conveys what happened ("created and
// approved by the user inline" / "creation was NOT completed").
//
//   - Bare string: replaced wholesale with note.
//   - Array of blocks: every text block whose text parses as a verb
//     has its bare-verb lines STRIPPED; the note is spliced in at the
//     first verb-bearing block's position. Non-text blocks and text
//     blocks that don't carry the verb pass through unchanged.
//
// Returns (encoded, true) on success; (_, false) when the content
// shape isn't one we can edit safely (e.g., an array with no
// verb-bearing text block).
func augmentUserContent(content json.RawMessage, verb, note string) (json.RawMessage, bool) {
	_ = verb // historical signature; verb is no longer prepended

	if len(content) == 0 {
		encoded, err := json.Marshal(note)
		return encoded, err == nil
	}
	// Bare string content.
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		encoded, marshalErr := json.Marshal(note)
		return encoded, marshalErr == nil
	}
	// Array-of-blocks content. Multi-block user messages can carry
	// verb-bearing lines in more than one text block (e.g., an
	// earlier "deny cv-stale" plus a later bare "approve"). Strip
	// bare-verb lines from EVERY text block so none of them remains
	// parseable as a fresh approval; splice the augmentation note in
	// at the first verb-bearing block's position.
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, false
	}
	spliceAt := -1
	for i, blk := range blocks {
		var t string
		if err := json.Unmarshal(blk["type"], &t); err != nil {
			continue
		}
		if t != "text" {
			continue
		}
		var text string
		if err := json.Unmarshal(blk["text"], &text); err != nil {
			continue
		}
		if v, _ := conversation.ParseApprovalReplyText(text); v == "" {
			continue
		}
		if spliceAt < 0 {
			spliceAt = i
		}
		stripped := stripBareApprovalLines(text)
		encoded, err := json.Marshal(stripped)
		if err != nil {
			return nil, false
		}
		blocks[i]["text"] = encoded
	}
	if spliceAt < 0 {
		return nil, false
	}
	// Splice the note into the first verb-bearing block. If that
	// block is now empty (its content was only verb lines), the note
	// becomes its content; otherwise the note is appended to whatever
	// non-verb prose remained.
	var spliceText string
	_ = json.Unmarshal(blocks[spliceAt]["text"], &spliceText)
	newSpliceText := note
	if spliceText != "" {
		newSpliceText = spliceText + "\n\n" + note
	}
	encoded, err := json.Marshal(newSpliceText)
	if err != nil {
		return nil, false
	}
	blocks[spliceAt]["text"] = encoded

	out, err := json.Marshal(blocks)
	if err != nil {
		return nil, false
	}
	return out, true
}

// stripBareApprovalLines removes lines that match the bare or
// verb+cv-id approval shape — exactly the lines ParseApprovalReplyText
// would have flagged. Used to ensure the augmented message doesn't
// leave a parseable "approve" / "approve cv-xxx" line behind in the
// content, which a downstream re-parse could interpret as a fresh
// approval gesture.
func stripBareApprovalLines(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		probe := strings.TrimSpace(line)
		if probe == "" {
			kept = append(kept, line)
			continue
		}
		// Use the same parser that decides whether a line is an
		// approval — anything it would match, we drop.
		if verb, _ := conversation.ParseApprovalReplyText(probe); verb != "" {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// augmentationContextForOutcome maps an outcome lookup to the bracketed
// context the augmenter should inject after the user's "approve". The
// store argument is treated nil-safe so call sites in tests and any
// transitional code without an outcome store still compile cleanly —
// nil store always returns ok=false (skip augmentation).
func augmentationContextForOutcome(approvalID string, store InlineApprovalOutcomeStore) (string, bool) {
	if store == nil || approvalID == "" {
		return "", false
	}
	outcome, ok := store.Lookup(approvalID)
	if !ok {
		return "", false
	}
	if outcome.Succeeded {
		return inlineApprovedReplyAugmentationContext(), true
	}
	return inlineFailedReplyAugmentationContext(outcome.FailureReason), true
}

// inlineFailedReplyAugmentationContext is the persistent-history
// counterpart to renderInlineTaskCreatorErrorReply. Tells the model
// the previously-approved task was NOT created so it doesn't proceed
// as if scope were granted.
func inlineFailedReplyAugmentationContext(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "creation failed"
	}
	return InlineApprovalAugmentationMarker + " creation was NOT completed (" + reason + "). No task is active; the originally-requested tool call is still out of scope. Acknowledge the failure to the user; do not retry without changes.]"
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
// conveys what happened ("created and approved by the user inline")
// without that sharp edge.
func inlineApprovedReplyAugmentation() string {
	return inlineApprovedReplyAugmentationContext()
}

// inlineApprovedReplyAugmentationContext is the bracketed body shared
// between the one-shot rewrite and the persistent augmenter.
func inlineApprovedReplyAugmentationContext() string {
	return InlineApprovalAugmentationMarker + " was created and approved by the user inline. Approval source: inline_chat. The task covers the originally requested work; proceed by emitting your next tool_use(s). Do NOT POST /control/tasks again for the same work — that would create a duplicate task. If your earlier tool_use already completed successfully (you can see a successful tool_result above), do NOT re-emit it; move on to the next step.]"
}

// renderInlineTaskDenyReply is the user-message text the LLM sees
// in place of bare "deny cv-xxx". Tells the model to stop and not
// retry the task-creation request. No leading verb — see the
// inlineApprovedReplyAugmentation comment for why.
func renderInlineTaskDenyReply() string {
	return "[Clawvisor: the user denied the task-creation request. Do not retry. Acknowledge the denial; stop unless the user issues a new request.]"
}

// renderInlineTaskCreatorErrorReply is used when the user approved
// but the task could not be created (validation error, store error,
// missing creator wiring). The LLM should treat this as a denial and
// surface the failure to the user.
func renderInlineTaskCreatorErrorReply(msg string) string {
	return fmt.Sprintf("[Clawvisor: inline task creation failed — %s. Acknowledge the failure to the user; do not retry without changes.]", msg)
}
