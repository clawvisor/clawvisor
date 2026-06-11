package llmproxy

import (
	"context"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// recoveredApproval is the data we hydrate from task_lifecycle_events
// when the in-memory hold has been lost (proxy restart between hold
// consumption and response delivery). The body editor uses this to
// reconstruct the missing assistant turn the same way it would with
// a live hold.
//
// TaskID is the event's task_id; Original is built from the event's
// agent-side fields (tool_use_id, name, input). Verb is the approval
// verb that landed the event ("approve" for *_approved, "deny" for
// *_denied) so the caller can drive the matching rewrite branch
// without re-parsing the body.
type recoveredApproval struct {
	TaskID   string
	Verb     string
	Kind     string // "task_create" or "task_expand"
	Original *InlineApprovalOriginalCall
}

// tryRecoverApprovalFromLifecycle reads the most recent lifecycle
// event for approvalID and decides whether it's recoverable. The
// happy-path call site is the rewrite path's cache-miss branch:
// when Peek returns no hold but the user just typed "approve" /
// "deny", a previously-running proxy may have already consumed the
// hold, written the resolution event, then crashed before
// delivering the rewritten response.
//
// Returns nil when:
//   - st is nil (caller didn't wire a store).
//   - approvalID is empty.
//   - no lifecycle event exists for this approvalID.
//   - the most-recent event is *_pending (the original hold is
//     still in-flight on this approval; the caller's cache miss
//     reflects a different bug, not a restart).
//   - the event lacks the agent-side fields needed to reconstruct
//     (very old rows from before this audit was wired).
//
// expectedVerb is the verb the body claims. Recovery only succeeds
// when it matches the resolution direction recorded in the event —
// a body claiming "deny" against an *_approved event is more likely
// a corrupt retry than a legitimate restart and we let it fall
// through unchanged.
func tryRecoverApprovalFromLifecycle(ctx context.Context, st store.Store, approvalID, expectedVerb string) *recoveredApproval {
	if st == nil || strings.TrimSpace(approvalID) == "" {
		return nil
	}
	event, err := st.GetTaskLifecycleEventByApprovalID(ctx, approvalID)
	if err != nil || event == nil {
		return nil
	}
	var (
		verb string
		kind string
	)
	switch event.EventType {
	case store.TaskLifecycleEventTaskCreateApproved:
		verb, kind = "approve", InlineApprovalOutcomeKindTaskCreate
	case store.TaskLifecycleEventTaskExpandApproved:
		verb, kind = "approve", InlineApprovalOutcomeKindTaskExpand
	case store.TaskLifecycleEventTaskCreateDenied:
		verb, kind = "deny", InlineApprovalOutcomeKindTaskCreate
	case store.TaskLifecycleEventTaskExpandDenied:
		verb, kind = "deny", InlineApprovalOutcomeKindTaskExpand
	default:
		// *_pending or other non-terminal events are not recoverable
		// — the in-flight hold is the source of truth, not us.
		return nil
	}
	if verb != expectedVerb {
		return nil
	}
	// Find the matching *_pending row so we can grab the original
	// tool_use snapshot. The terminal row's agent-side fields are
	// empty by design (logTaskLifecycleEventResolution doesn't
	// repeat them); the pending row has them.
	original := lookupOriginalCallForApproval(ctx, st, event.UserID, event.TaskID, approvalID)
	if original == nil {
		// Pending row missing or stripped — without the original
		// tool_use we can't reconstruct, so let the caller's
		// fall-through handle it. The deny case is OK without the
		// original (no reconstruction needed), so allow recovery
		// for deny even when the original is missing.
		if verb != "deny" {
			return nil
		}
	}
	return &recoveredApproval{
		TaskID:   event.TaskID,
		Verb:     verb,
		Kind:     kind,
		Original: original,
	}
}

// rewriteFromRecoveredApproval drives the body rewrite from
// lifecycle-event-recovered data instead of a live hold. Mirrors the
// happy path's editor.ReplaceLatestUserText call shape so the
// resulting body is byte-identical (modulo the recovery's slightly
// less-specific notice text) to what the original proxy would have
// produced before crashing.
//
// On success returns InlineApprovalRewriteResult{Body: <rewritten>,
// Rewritten: true, Decision/Outcome reflecting the recovery}. On
// failure to rewrite (provider doesn't support reconstruction,
// unexpected body shape) returns the original body and leaves the
// recovered approval's side effects as the final state.
func rewriteFromRecoveredApproval(
	req InlineApprovalRewriteRequest,
	editor approvalBodyEditor,
	verb, approvalID string,
	recovered *recoveredApproval,
) (InlineApprovalRewriteResult, error) {
	out := InlineApprovalRewriteResult{
		Body:   req.Body,
		TaskID: recovered.TaskID,
	}
	replacement := recoveredApprovalReplacementText(recovered)
	switch verb {
	case "approve":
		out.Decision = "allow"
		if recovered.Kind == InlineApprovalOutcomeKindTaskExpand {
			out.Outcome = "inline_expansion_approved_recovered"
		} else {
			out.Outcome = "inline_task_approved_recovered"
		}
	case "deny":
		out.Decision = "deny"
		if recovered.Kind == InlineApprovalOutcomeKindTaskExpand {
			out.Outcome = "inline_expansion_denied_recovered"
		} else {
			out.Outcome = "inline_task_denied_recovered"
		}
	default:
		// Unknown verb: bail.
		return InlineApprovalRewriteResult{Body: req.Body}, nil
	}
	rewritten, ok, err := editor.ReplaceLatestUserText(verb, approvalID, replacement, recovered.Original)
	if err != nil {
		return out, err
	}
	if !ok {
		// Editor refused (unsupported provider, body shape didn't
		// match). The DB has the canonical resolution; the model
		// just doesn't get the reconstructed pair on this retry.
		return out, nil
	}
	out.Body = rewritten
	out.Rewritten = true
	return out, nil
}

// recoveredApprovalReplacementText renders the augmentation notice
// text the body editor splices in as the synthetic tool_result
// content. Recovery loses the credential placeholders and the
// rendered envelope detail (those were on the original hold, not
// the lifecycle event), so the recovered notice is a simplified
// form. It still tells the model: "your call was approved, the
// task state advanced, don't re-emit." The augmenter on later
// turns will see the reconstructed pair in history and skip
// (no substituted-prompt marker), so the simplified text only
// shows up on this one recovery turn.
func recoveredApprovalReplacementText(recovered *recoveredApproval) string {
	if recovered == nil {
		return ""
	}
	if recovered.Verb == "deny" {
		if recovered.Kind == InlineApprovalOutcomeKindTaskExpand {
			return renderInlineExpansionDenyReply()
		}
		return renderInlineTaskDenyReply()
	}
	switch recovered.Kind {
	case InlineApprovalOutcomeKindTaskExpand:
		return inlineExpansionApprovedReplyAugmentationContext(recovered.TaskID, nil)
	default:
		return inlineApprovedReplyAugmentationContext(recovered.TaskID, false, nil)
	}
}

// lookupOriginalCallForApproval walks the task's lifecycle events
// looking for the *_pending row that captured the agent's original
// tool_use. The pending row is written at hold-creation time before
// the terminal row, so it's always older — list-by-task is ascending
// by occurred_at, so we walk forward and return the first matching
// approval_id with a non-empty tool_use_id. Returns nil when no
// pending row exists (older approvals written before the lifecycle
// audit was wired) or the pending row stripped the tool input.
func lookupOriginalCallForApproval(ctx context.Context, st store.Store, userID, taskID, approvalID string) *InlineApprovalOriginalCall {
	events, err := st.ListTaskLifecycleEvents(ctx, userID, taskID)
	if err != nil {
		return nil
	}
	for _, ev := range events {
		if ev == nil || ev.ApprovalID != approvalID {
			continue
		}
		if ev.ToolUseID == "" || ev.ToolName == "" {
			continue
		}
		return &InlineApprovalOriginalCall{
			ToolUseID: ev.ToolUseID,
			ToolName:  ev.ToolName,
			Input:     append([]byte(nil), ev.ToolInputJSON...),
		}
	}
	return nil
}
