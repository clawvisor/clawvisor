package llmproxy

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/historystrip"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// NewLifecycleReconstructionBuilder returns a builder that the
// SyntheticHistoryStrip policy invokes per-request to get a
// ReconstructionLookup. The returned lookup queries
// task_lifecycle_events by approval_id, picks the *_pending row for
// the agent's original tool_use snapshot, and the latest terminal
// row to decide what notice text to use as the synthetic
// tool_result content.
//
// nil-safe: a nil store yields a nil builder (the policy falls back
// to drop-the-turn behavior).
func NewLifecycleReconstructionBuilder(st store.Store) func(ctx context.Context) historystrip.ReconstructionLookup {
	if st == nil {
		return nil
	}
	return func(ctx context.Context) historystrip.ReconstructionLookup {
		return func(approvalID string) *historystrip.ReconstructedPair {
			return reconstructionPairForApproval(ctx, st, approvalID)
		}
	}
}

// reconstructionPairForApproval is the per-approval lookup body.
// Returns nil when reconstruction is not faithful (no pending row,
// no terminal row, stripped tool_use fields, unrecognized
// event_type). Callers degrade to drop-the-turn behavior on nil.
func reconstructionPairForApproval(ctx context.Context, st store.Store, approvalID string) *historystrip.ReconstructedPair {
	if st == nil || approvalID == "" {
		return nil
	}
	events, err := st.ListTaskLifecycleEventsByApprovalID(ctx, approvalID)
	if err != nil || len(events) == 0 {
		return nil
	}
	var pending *store.TaskLifecycleEvent
	var terminal *store.TaskLifecycleEvent
	for _, ev := range events {
		if ev == nil {
			continue
		}
		switch ev.EventType {
		case store.TaskLifecycleEventTaskCreatePending,
			store.TaskLifecycleEventTaskExpandPending:
			pending = ev
		case store.TaskLifecycleEventTaskCreateApproved,
			store.TaskLifecycleEventTaskCreateDenied,
			store.TaskLifecycleEventTaskExpandApproved,
			store.TaskLifecycleEventTaskExpandDenied,
			store.TaskLifecycleEventTaskExpandExpired,
			store.TaskLifecycleEventTaskRevoked,
			store.TaskLifecycleEventTaskCompleted,
			store.TaskLifecycleEventTaskExpired:
			terminal = ev
		}
	}
	if pending == nil || terminal == nil {
		// In-flight (terminal missing) or pre-audit (pending
		// missing); leave the strip to drop the turn.
		return nil
	}
	if pending.ToolUseID == "" || pending.ToolName == "" || len(pending.ToolInputJSON) == 0 {
		// The pending row is missing the snapshot fields — older
		// rows or a future stripped variant. Without the agent's
		// verbatim input we cannot faithfully reconstruct, so
		// defer to drop-the-turn (see buildReconstructedAssistantBlock's
		// rationale on fabricating empty inputs).
		return nil
	}
	resultText := reconstructionResultText(terminal.EventType, pending.TaskID)
	if resultText == "" {
		// Unrecognized terminal kind (e.g. a future addition
		// without a renderer wired) — degrade rather than
		// surfacing an empty tool_result.
		return nil
	}
	return &historystrip.ReconstructedPair{
		ToolUseID:  pending.ToolUseID,
		ToolName:   pending.ToolName,
		Input:      append([]byte(nil), pending.ToolInputJSON...),
		ResultText: resultText,
	}
}

// reconstructionResultText maps a terminal lifecycle event_type to
// the notice text that goes into the synthetic tool_result content.
// The renderers live in this package because they're tied to the
// approval prompt / notice conventions; keeping the mapping local
// preserves the historystrip package's storage-agnosticism.
//
// Credentials / CheckedOut are not preserved in lifecycle events
// (yet), so the recovered notice omits those — the model gets the
// task ID and the "scope was added" / "task was created" semantics
// either way. A model that needs credential placeholders re-mints
// via /control/vault/items per the existing workflow.
func reconstructionResultText(eventType, taskID string) string {
	switch eventType {
	case store.TaskLifecycleEventTaskCreateApproved:
		return inlineApprovedReplyAugmentationContext(taskID, false, nil)
	case store.TaskLifecycleEventTaskExpandApproved:
		return inlineExpansionApprovedReplyAugmentationContext(taskID, nil)
	case store.TaskLifecycleEventTaskCreateDenied:
		return renderInlineTaskDenyReply()
	case store.TaskLifecycleEventTaskExpandDenied,
		store.TaskLifecycleEventTaskExpandExpired:
		return renderInlineExpansionDenyReply()
	case store.TaskLifecycleEventTaskRevoked,
		store.TaskLifecycleEventTaskCompleted,
		store.TaskLifecycleEventTaskExpired:
		// Treat non-approval terminations as a "task ended" notice
		// via the deny renderer's neutral framing — these
		// shouldn't be reachable via a substituted-prompt path in
		// practice (revoke / complete don't go through chat
		// approval), but the safe default is the same shape.
		return renderInlineTaskDenyReply()
	default:
		return ""
	}
}
