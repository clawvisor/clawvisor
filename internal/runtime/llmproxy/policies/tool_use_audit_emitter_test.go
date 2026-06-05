package policies_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

func TestEmitToolUseAuditRows_NoOpOnNilInputs(t *testing.T) {
	// Both nil result + nil sink → no panic, no rows.
	called := false
	policies.EmitToolUseAuditRows(context.Background(), nil, nil, nil, func(_ context.Context, _ policies.ToolUseAuditRow) {
		called = true
	})
	if called {
		t.Error("sink invoked on nil result")
	}

	policies.EmitToolUseAuditRows(context.Background(), &pipeline.ToolUseResult{}, nil, nil, nil)
	// no panic
}

// TestEmitToolUseAuditRows_TaskScopeMissingMapping pins the row a
// TaskScopeEvaluator Hold produces — matches the legacy audit row's
// (Decision=block, Outcome=task_scope_missing) tuple.
func TestEmitToolUseAuditRows_TaskScopeMissingMapping(t *testing.T) {
	tu := conversation.ToolUse{ID: "toolu_1", Name: "Bash", Input: json.RawMessage(`{"command":"mkdir /tmp/x"}`)}
	result := &pipeline.ToolUseResult{
		PerToolUse: map[string]pipeline.ToolUseVerdict{
			"toolu_1": {
				Outcome: pipeline.OutcomeHold,
				Reason:  "no active task scope",
				AuditFields: map[string]any{
					"task_scope_reason":  "no active task scope",
					"task_scope_allowed": false,
					"matched_task_id":    "task-123",
				},
				HoldKey: "needs_task_toolu_1",
			},
		},
		Evaluations: []pipeline.ToolUseEvaluation{
			{EvaluatorName: "task_scope", ToolUseID: "toolu_1", Verdict: pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeHold}},
		},
	}
	var rows []policies.ToolUseAuditRow
	policies.EmitToolUseAuditRows(context.Background(), result, []conversation.ToolUse{tu}, nil, func(_ context.Context, r policies.ToolUseAuditRow) {
		rows = append(rows, r)
	})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.Decision != "block" {
		t.Errorf("Decision = %q, want block", r.Decision)
	}
	if r.Outcome != "task_scope_missing" {
		t.Errorf("Outcome = %q, want task_scope_missing", r.Outcome)
	}
	if r.TaskID != "task-123" {
		t.Errorf("TaskID = %q, want task-123", r.TaskID)
	}
	if r.Reason != "no active task scope" {
		t.Errorf("Reason = %q", r.Reason)
	}
}

// TestEmitToolUseAuditRows_CredentialRewriteMapping pins the row a
// CredentialRewriteEvaluator success emits — Decision=rewrite,
// Outcome=success.
func TestEmitToolUseAuditRows_CredentialRewriteMapping(t *testing.T) {
	tu := conversation.ToolUse{ID: "toolu_1", Name: "WebFetch", Input: json.RawMessage(`{}`)}
	result := &pipeline.ToolUseResult{
		PerToolUse: map[string]pipeline.ToolUseVerdict{
			"toolu_1": {
				Outcome: pipeline.OutcomeRewrite,
				Reason:  "credentialed call rewritten",
				AuditFields: map[string]any{
					"rewrite_outcome": "success",
					"target_host":     "api.github.com",
					"target_method":   "POST",
				},
			},
		},
		Evaluations: []pipeline.ToolUseEvaluation{
			{EvaluatorName: "credential_rewrite", ToolUseID: "toolu_1", Verdict: pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeRewrite}},
		},
	}
	var rows []policies.ToolUseAuditRow
	policies.EmitToolUseAuditRows(context.Background(), result, []conversation.ToolUse{tu}, nil, func(_ context.Context, r policies.ToolUseAuditRow) {
		rows = append(rows, r)
	})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Decision != "rewrite" {
		t.Errorf("Decision = %q, want rewrite", rows[0].Decision)
	}
	if rows[0].Outcome != "success" {
		t.Errorf("Outcome = %q, want success", rows[0].Outcome)
	}
}

// TestEmitToolUseAuditRows_ControlOutcomeMapping pins the row a
// ControlToolUseEvaluator block emits — pulls Outcome from
// control_outcome AuditField.
func TestEmitToolUseAuditRows_ControlOutcomeMapping(t *testing.T) {
	tu := conversation.ToolUse{ID: "toolu_1", Name: "Bash"}
	result := &pipeline.ToolUseResult{
		PerToolUse: map[string]pipeline.ToolUseVerdict{
			"toolu_1": {
				Outcome: pipeline.OutcomeDeny,
				Reason:  "Clawvisor: caller nonce cache not configured",
				AuditFields: map[string]any{
					"control_outcome": "caller_nonce_unavailable",
				},
			},
		},
		Evaluations: []pipeline.ToolUseEvaluation{
			{EvaluatorName: "control_tool_use", ToolUseID: "toolu_1", Verdict: pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeDeny}},
		},
	}
	var rows []policies.ToolUseAuditRow
	policies.EmitToolUseAuditRows(context.Background(), result, []conversation.ToolUse{tu}, nil, func(_ context.Context, r policies.ToolUseAuditRow) {
		rows = append(rows, r)
	})
	if rows[0].Decision != "block" {
		t.Errorf("Decision = %q, want block", rows[0].Decision)
	}
	if rows[0].Outcome != "caller_nonce_unavailable" {
		t.Errorf("Outcome = %q, want caller_nonce_unavailable", rows[0].Outcome)
	}
}

// TestEmitToolUseAuditRows_ScriptSessionPassthroughMapping pins the
// row a ScriptSessionEvaluator allow emits — Outcome reads from the
// path AuditField (script_session_passthrough).
func TestEmitToolUseAuditRows_ScriptSessionPassthroughMapping(t *testing.T) {
	tu := conversation.ToolUse{ID: "toolu_1", Name: "WebFetch"}
	result := &pipeline.ToolUseResult{
		PerToolUse: map[string]pipeline.ToolUseVerdict{
			"toolu_1": {
				Outcome: pipeline.OutcomeAllow,
				Reason:  "tool_use carries a script-session caller token",
				AuditFields: map[string]any{
					"path":           "script_session_passthrough",
					"verdict_source": "script_session",
				},
			},
		},
		Evaluations: []pipeline.ToolUseEvaluation{
			{EvaluatorName: "script_session", ToolUseID: "toolu_1", Verdict: pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeAllow}},
		},
	}
	var rows []policies.ToolUseAuditRow
	policies.EmitToolUseAuditRows(context.Background(), result, []conversation.ToolUse{tu}, nil, func(_ context.Context, r policies.ToolUseAuditRow) {
		rows = append(rows, r)
	})
	if rows[0].Decision != "allow" {
		t.Errorf("Decision = %q, want allow", rows[0].Decision)
	}
	if rows[0].Outcome != "script_session_passthrough" {
		t.Errorf("Outcome = %q, want script_session_passthrough", rows[0].Outcome)
	}
}

// TestEmitToolUseAuditRows_InspectorVerdictRederived pins that when
// an Inspector is provided, the emitter re-runs Inspect to populate
// the row's Verdict field (rather than requiring callers to thread it
// through pipeline state).
func TestEmitToolUseAuditRows_InspectorVerdictRederived(t *testing.T) {
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	tu := conversation.ToolUse{
		ID:   "toolu_1",
		Name: "WebFetch",
		Input: json.RawMessage(`{
			"url":"https://api.github.com/repos/x/y/issues",
			"method":"POST",
			"headers":{"Authorization":"Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}
		}`),
	}
	result := &pipeline.ToolUseResult{
		PerToolUse: map[string]pipeline.ToolUseVerdict{
			"toolu_1": {Outcome: pipeline.OutcomeAllow},
		},
	}
	var rows []policies.ToolUseAuditRow
	policies.EmitToolUseAuditRows(context.Background(), result, []conversation.ToolUse{tu}, insp, func(_ context.Context, r policies.ToolUseAuditRow) {
		rows = append(rows, r)
	})
	if rows[0].Verdict.Host != "api.github.com" {
		t.Errorf("Verdict.Host = %q, want api.github.com", rows[0].Verdict.Host)
	}
	if !rows[0].Verdict.IsAPICall {
		t.Error("expected IsAPICall=true")
	}
}
