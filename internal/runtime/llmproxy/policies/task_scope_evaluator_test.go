package policies_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

// TestTaskScope_SkipsNilResolver pins the gate.
func TestTaskScope_SkipsNilResolver(t *testing.T) {
	e := policies.NewTaskScopeEvaluator(nil)
	tu := conversation.ToolUse{ID: "x"}
	v, err := e.Evaluate(context.Background(), nil, tu, evalToolUseMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeSkip {
		t.Errorf("nil resolver → Outcome = %q, want Skip", v.Outcome)
	}
}

// TestTaskScope_AllowOnMatchedScope pins the success path.
func TestTaskScope_AllowOnMatchedScope(t *testing.T) {
	resolver := func(_ context.Context, _ conversation.ToolUse) policies.TaskScopeDecision {
		return policies.TaskScopeDecision{
			Allowed: true,
			TaskID:  "task-abc",
			Reason:  "matched",
		}
	}
	e := policies.NewTaskScopeEvaluator(resolver)
	tu := conversation.ToolUse{ID: "toolu_1", Name: "Bash", Input: json.RawMessage(`{}`)}
	v, err := e.Evaluate(context.Background(), nil, tu, evalToolUseMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeAllow {
		t.Errorf("matched scope → Outcome = %q, want Allow", v.Outcome)
	}
	var tf pipeline.TaskScopeFact
	for _, f := range v.Facts {
		if ts, ok := f.(pipeline.TaskScopeFact); ok {
			tf = ts
			break
		}
	}
	if tf.MatchedTaskID != "task-abc" {
		t.Errorf("TaskScopeFact.MatchedTaskID = %v, want task-abc", tf.MatchedTaskID)
	}
	if !tf.Allowed {
		t.Errorf("TaskScopeFact.Allowed = false, want true")
	}
}

// TestTaskScope_HoldOnUnmatchedScope pins the Hold path: needs_new_task.
func TestTaskScope_HoldOnUnmatchedScope(t *testing.T) {
	resolver := func(_ context.Context, _ conversation.ToolUse) policies.TaskScopeDecision {
		return policies.TaskScopeDecision{
			Allowed: false,
			Reason:  "needs_new_task",
		}
	}
	e := policies.NewTaskScopeEvaluator(resolver)
	tu := conversation.ToolUse{ID: "toolu_2"}
	v, err := e.Evaluate(context.Background(), nil, tu, evalToolUseMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeHold {
		t.Errorf("unmatched scope → Outcome = %q, want Hold", v.Outcome)
	}
	if v.HoldKey != "needs_task_toolu_2" {
		t.Errorf("HoldKey = %q, want per-tool needs_task_<id>", v.HoldKey)
	}
	if v.Reason != "needs_new_task" {
		t.Errorf("Reason = %q, want needs_new_task", v.Reason)
	}
}

// TestTaskScope_SkipsWhenEmptyReason pins the "resolver chose not to act"
// signal: a Decision with empty Reason → Skip.
func TestTaskScope_SkipsWhenEmptyReason(t *testing.T) {
	resolver := func(_ context.Context, _ conversation.ToolUse) policies.TaskScopeDecision {
		return policies.TaskScopeDecision{} // empty Reason
	}
	e := policies.NewTaskScopeEvaluator(resolver)
	tu := conversation.ToolUse{ID: "x"}
	v, err := e.Evaluate(context.Background(), nil, tu, evalToolUseMutator{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Outcome != pipeline.OutcomeSkip {
		t.Errorf("empty Reason → Outcome = %q, want Skip", v.Outcome)
	}
}
