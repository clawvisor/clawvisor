package llmproxy

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// TestAuditEmitter_WriteAuditEvent_PersistsCvReason confirms the
// agent's stated per-call rationale (extracted from cvreason and
// carried on conversation.ToolUse) round-trips into the audit row's
// ParamsSafe JSON. Without persistence, audit consumers can see what
// the agent did but not why it claimed the call fit task scope.
func TestAuditEmitter_WriteAuditEvent_PersistsCvReason(t *testing.T) {
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, nil)

	em.WriteAuditEvent(context.Background(), agent, "req-1", conversation.AuditEvent{
		ToolUse: conversation.ToolUse{
			ID:       "toolu_cv",
			Name:     "Read",
			Input:    json.RawMessage(`{"path":"src/auth.go"}`),
			CvReason: "Inspecting auth handler before renaming.",
		},
		Decision:    conversation.DecisionAllow,
		OutcomeName: "policy_allow",
	})

	rows, _, _ := st.ListAuditEntries(context.Background(), agent.UserID, store.AuditFilter{})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	var params map[string]any
	if err := json.Unmarshal(rows[0].ParamsSafe, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if params["tool_cvreason"] != "Inspecting auth handler before renaming." {
		t.Errorf("tool_cvreason = %v, want extracted cvreason", params["tool_cvreason"])
	}
}

// TestAuditEmitter_WriteAuditEvent_OmitsEmptyCvReason confirms that
// when the agent did not supply a cvreason, the audit row stays sparse
// rather than persisting an empty string. Querying for rows that have
// a stated rationale shouldn't accidentally match rows that simply
// omitted the field.
func TestAuditEmitter_WriteAuditEvent_OmitsEmptyCvReason(t *testing.T) {
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, nil)

	em.WriteAuditEvent(context.Background(), agent, "req-1", conversation.AuditEvent{
		ToolUse: conversation.ToolUse{
			ID:    "toolu_no_cv",
			Name:  "Read",
			Input: json.RawMessage(`{"path":"src/auth.go"}`),
		},
		Decision:    conversation.DecisionAllow,
		OutcomeName: "policy_allow",
	})

	rows, _, _ := st.ListAuditEntries(context.Background(), agent.UserID, store.AuditFilter{})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	var params map[string]any
	if err := json.Unmarshal(rows[0].ParamsSafe, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if _, present := params["tool_cvreason"]; present {
		t.Errorf("tool_cvreason should be absent when empty, got %v", params["tool_cvreason"])
	}
}
