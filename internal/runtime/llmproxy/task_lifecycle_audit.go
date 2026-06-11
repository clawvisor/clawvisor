package llmproxy

import (
	"context"
	"encoding/json"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// taskLifecycleAuditCtx is the call-time data the lifecycle audit
// writer needs to stamp an event row. AgentID/UserID/ConversationID/
// RequestID are pulled from the surrounding PostprocessConfig; the
// rest is event-specific.
//
// st may be nil — callers that don't have a store handle (tests, the
// rare misconfigured daemon path) skip the write silently. Audit is
// best-effort: a failed event write must NEVER block the hold/release
// path. The trace channel is the structured log surface for write
// failures.
type taskLifecycleAuditCtx struct {
	st             store.Store
	trace          func(event string, kv ...any)
	userID         string
	agentID        string
	conversationID string
	requestID      string
}

// logTaskLifecycleEventFromHold writes a lifecycle audit row at the
// moment the proxy holds a pending inline approval. The event
// captures the agent's verbatim tool_use (the body editor uses this
// on the next turn to reconstruct the model's missing assistant
// turn) plus the rendered approval surface.
//
// The write is synchronous but does NOT propagate errors — a store
// outage at this point must not strand the hold. Failures land in
// the trace channel under event="task_lifecycle.write_failed".
func logTaskLifecycleEventFromHold(
	ctx context.Context,
	a taskLifecycleAuditCtx,
	taskID, approvalID, eventType, approvalSurface string,
	tu conversation.ToolUse,
	payload json.RawMessage,
) {
	if a.st == nil || taskID == "" || a.userID == "" || a.agentID == "" {
		return
	}
	var toolInput json.RawMessage
	if len(tu.Input) > 0 {
		toolInput = append(json.RawMessage(nil), tu.Input...)
	}
	event := &store.TaskLifecycleEvent{
		TaskID:          taskID,
		UserID:          a.userID,
		AgentID:         a.agentID,
		EventType:       eventType,
		OccurredAt:      time.Now().UTC(),
		ApprovalID:      approvalID,
		ApprovalSurface: approvalSurface,
		ConversationID:  a.conversationID,
		RequestID:       a.requestID,
		ToolUseID:       tu.ID,
		ToolName:        tu.Name,
		ToolInputJSON:   toolInput,
		PayloadJSON:     payload,
	}
	if err := a.st.CreateTaskLifecycleEvent(ctx, event); err != nil && a.trace != nil {
		a.trace("task_lifecycle.write_failed",
			"event_type", eventType,
			"task_id", taskID,
			"approval_id", approvalID,
			"err", err.Error(),
		)
	}
}

// logTaskLifecycleEventResolution writes the terminal lifecycle event
// after the proxy's approval-rewrite path consumes the hold. No
// tool_use fields are populated — the row's purpose is to mark the
// state transition (approved/denied) and preserve a chain of events
// for audit queries that join by task_id.
func logTaskLifecycleEventResolution(
	ctx context.Context,
	a taskLifecycleAuditCtx,
	taskID, approvalID, eventType, approvalSurface string,
	payload json.RawMessage,
) {
	if a.st == nil || taskID == "" || a.userID == "" || a.agentID == "" {
		return
	}
	event := &store.TaskLifecycleEvent{
		TaskID:          taskID,
		UserID:          a.userID,
		AgentID:         a.agentID,
		EventType:       eventType,
		OccurredAt:      time.Now().UTC(),
		ApprovalID:      approvalID,
		ApprovalSurface: approvalSurface,
		ConversationID:  a.conversationID,
		RequestID:       a.requestID,
		PayloadJSON:     payload,
	}
	if err := a.st.CreateTaskLifecycleEvent(ctx, event); err != nil && a.trace != nil {
		a.trace("task_lifecycle.write_failed",
			"event_type", eventType,
			"task_id", taskID,
			"approval_id", approvalID,
			"err", err.Error(),
		)
	}
}
