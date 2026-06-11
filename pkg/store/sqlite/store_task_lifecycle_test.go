package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// TestTaskLifecycleEvents_RoundTrip exercises CreateTaskLifecycleEvent,
// GetTaskLifecycleEventByApprovalID, and ListTaskLifecycleEvents
// against a real sqlite-backed store. The events table is the audit
// log for task creation / expansion / denial; both the body editor
// (for conversation reconstruction on cache miss) and audit UIs read
// from it, so round-trip fidelity on tool_input_json and the
// nullable ID fields matters.
func TestTaskLifecycleEvents_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := New(ctx, filepath.Join(t.TempDir(), "clawvisor.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := NewStore(db)

	user, err := st.CreateUser(ctx, "lifecycle@example.com", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "lifecycle-agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	task := &store.Task{
		UserID:        user.ID,
		AgentID:       agent.ID,
		Purpose:       "Lifecycle test task",
		Status:        "active",
		Lifetime:      "session",
		ExpectedTools: []byte(`[{"tool_name":"Bash","why":"test"}]`),
		SchemaVersion: 2,
		CreatedAt:     time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	t0 := time.Now().UTC().Truncate(time.Millisecond)
	approvalID := "cv-test-approval-1"
	pending := &store.TaskLifecycleEvent{
		TaskID:          task.ID,
		UserID:          user.ID,
		AgentID:         agent.ID,
		EventType:       store.TaskLifecycleEventTaskCreatePending,
		OccurredAt:      t0,
		ApprovalID:      approvalID,
		ApprovalSurface: "inline_chat",
		ConversationID:  "conv-1",
		RequestID:       "req-1",
		ToolUseID:       "toolu_test_1",
		ToolName:        "Bash",
		ToolInputJSON:   json.RawMessage(`{"command":"curl -X POST .../control/tasks --data ..."}`),
		PayloadJSON:     json.RawMessage(`{"purpose":"Lifecycle test task"}`),
	}
	if err := st.CreateTaskLifecycleEvent(ctx, pending); err != nil {
		t.Fatalf("CreateTaskLifecycleEvent(pending): %v", err)
	}
	if pending.ID == "" {
		t.Fatalf("CreateTaskLifecycleEvent should populate ID; got empty")
	}

	approved := &store.TaskLifecycleEvent{
		TaskID:          task.ID,
		UserID:          user.ID,
		AgentID:         agent.ID,
		EventType:       store.TaskLifecycleEventTaskCreateApproved,
		OccurredAt:      t0.Add(2 * time.Second),
		ApprovalID:      approvalID,
		ApprovalSurface: "inline_chat",
		PayloadJSON:     json.RawMessage(`{"resolution":"approved"}`),
	}
	if err := st.CreateTaskLifecycleEvent(ctx, approved); err != nil {
		t.Fatalf("CreateTaskLifecycleEvent(approved): %v", err)
	}

	got, err := st.GetTaskLifecycleEventByApprovalID(ctx, approvalID)
	if err != nil {
		t.Fatalf("GetTaskLifecycleEventByApprovalID: %v", err)
	}
	if got.EventType != store.TaskLifecycleEventTaskCreateApproved {
		t.Errorf("GetByApprovalID should return most-recent; got %q want %q",
			got.EventType, store.TaskLifecycleEventTaskCreateApproved)
	}

	list, err := st.ListTaskLifecycleEvents(ctx, user.ID, task.ID)
	if err != nil {
		t.Fatalf("ListTaskLifecycleEvents: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListTaskLifecycleEvents: got %d, want 2", len(list))
	}
	if list[0].EventType != store.TaskLifecycleEventTaskCreatePending {
		t.Errorf("list[0] = %q, want %q (ascending order)",
			list[0].EventType, store.TaskLifecycleEventTaskCreatePending)
	}
	if list[1].EventType != store.TaskLifecycleEventTaskCreateApproved {
		t.Errorf("list[1] = %q, want %q", list[1].EventType, store.TaskLifecycleEventTaskCreateApproved)
	}

	// Round-trip the agent-side fields on the pending row.
	if list[0].ToolUseID != "toolu_test_1" {
		t.Errorf("ToolUseID = %q, want toolu_test_1", list[0].ToolUseID)
	}
	if list[0].ToolName != "Bash" {
		t.Errorf("ToolName = %q, want Bash", list[0].ToolName)
	}
	if len(list[0].ToolInputJSON) == 0 {
		t.Errorf("ToolInputJSON should round-trip, got empty")
	} else {
		var input map[string]any
		if err := json.Unmarshal(list[0].ToolInputJSON, &input); err != nil {
			t.Errorf("ToolInputJSON not valid JSON: %v", err)
		} else if _, ok := input["command"]; !ok {
			t.Errorf("ToolInputJSON missing command field: %s", string(list[0].ToolInputJSON))
		}
	}

	// Approved row has no agent-side fields — null columns should
	// round-trip as zero-value strings, NOT nil-pointer panics.
	if list[1].ToolUseID != "" {
		t.Errorf("approved.ToolUseID = %q, want empty", list[1].ToolUseID)
	}
	if list[1].ConversationID != "" {
		t.Errorf("approved.ConversationID = %q, want empty", list[1].ConversationID)
	}

	// Unknown approval_id returns ErrNotFound (not panic, not empty struct).
	_, err = st.GetTaskLifecycleEventByApprovalID(ctx, "cv-does-not-exist")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing approval_id should return ErrNotFound, got: %v", err)
	}
}
