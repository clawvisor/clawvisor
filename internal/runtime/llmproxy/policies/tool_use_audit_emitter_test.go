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
	policies.EmitToolUseAuditRows(context.Background(), nil, nil, nil, func(_ context.Context, _ conversation.AuditEvent) {
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
	scopeFact := pipeline.TaskScopeFact{Reason: "no active task scope", Allowed: false, MatchedTaskID: "task-123"}
	result := &pipeline.ToolUseResult{
		PerToolUse: map[string]pipeline.ToolUseVerdict{
			"toolu_1": {
				Outcome: pipeline.OutcomeHold,
				Reason:  "no active task scope",
				HoldKey: "needs_task_toolu_1",
				Facts:   []pipeline.EvaluationFact{scopeFact},
			},
		},
		Evaluations: []pipeline.ToolUseEvaluation{
			{EvaluatorName: "task_scope", ToolUseID: "toolu_1", Verdict: pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeHold, Facts: []pipeline.EvaluationFact{scopeFact}}},
		},
	}
	var rows []conversation.AuditEvent
	policies.EmitToolUseAuditRows(context.Background(), result, []conversation.ToolUse{tu}, nil, func(_ context.Context, r conversation.AuditEvent) {
		rows = append(rows, r)
	})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.Decision != "block" {
		t.Errorf("Decision = %q, want block", r.Decision)
	}
	if r.OutcomeName != "task_scope_missing" {
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
	rewriteFact := pipeline.RewriteFact{Outcome: "success", TargetHost: "api.github.com", TargetMethod: "POST"}
	result := &pipeline.ToolUseResult{
		PerToolUse: map[string]pipeline.ToolUseVerdict{
			"toolu_1": {
				Outcome: pipeline.OutcomeRewrite,
				Reason:  "credentialed call rewritten",
				Facts:   []pipeline.EvaluationFact{rewriteFact},
			},
		},
		Evaluations: []pipeline.ToolUseEvaluation{
			{EvaluatorName: "credential_rewrite", ToolUseID: "toolu_1", Verdict: pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeRewrite, Facts: []pipeline.EvaluationFact{rewriteFact}}},
		},
	}
	var rows []conversation.AuditEvent
	policies.EmitToolUseAuditRows(context.Background(), result, []conversation.ToolUse{tu}, nil, func(_ context.Context, r conversation.AuditEvent) {
		rows = append(rows, r)
	})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Decision != "rewrite" {
		t.Errorf("Decision = %q, want rewrite", rows[0].Decision)
	}
	if rows[0].OutcomeName != "success" {
		t.Errorf("Outcome = %q, want success", rows[0].Outcome)
	}
}

// TestEmitToolUseAuditRows_ControlOutcomeMapping pins the row a
// ControlToolUseEvaluator block emits — pulls Outcome from
// control_outcome AuditField.
func TestEmitToolUseAuditRows_ControlOutcomeMapping(t *testing.T) {
	tu := conversation.ToolUse{ID: "toolu_1", Name: "Bash"}
	controlFact := pipeline.ControlFact{Outcome: "caller_nonce_unavailable"}
	result := &pipeline.ToolUseResult{
		PerToolUse: map[string]pipeline.ToolUseVerdict{
			"toolu_1": {
				Outcome: pipeline.OutcomeDeny,
				Reason:  "Clawvisor: caller nonce cache not configured",
				Facts:   []pipeline.EvaluationFact{controlFact},
			},
		},
		Evaluations: []pipeline.ToolUseEvaluation{
			{EvaluatorName: "control_tool_use", ToolUseID: "toolu_1", Verdict: pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeDeny, Facts: []pipeline.EvaluationFact{controlFact}}},
		},
	}
	var rows []conversation.AuditEvent
	policies.EmitToolUseAuditRows(context.Background(), result, []conversation.ToolUse{tu}, nil, func(_ context.Context, r conversation.AuditEvent) {
		rows = append(rows, r)
	})
	if rows[0].Decision != "block" {
		t.Errorf("Decision = %q, want block", rows[0].Decision)
	}
	if rows[0].OutcomeName != "caller_nonce_unavailable" {
		t.Errorf("Outcome = %q, want caller_nonce_unavailable", rows[0].Outcome)
	}
}

// TestEmitToolUseAuditRows_ScriptSessionPassthroughMapping pins the
// row a ScriptSessionEvaluator allow emits — Outcome reads from the
// path AuditField (script_session_passthrough).
func TestEmitToolUseAuditRows_ScriptSessionPassthroughMapping(t *testing.T) {
	tu := conversation.ToolUse{ID: "toolu_1", Name: "WebFetch"}
	scriptFact := pipeline.ScriptSessionFact{Outcome: "script_session_passthrough"}
	result := &pipeline.ToolUseResult{
		PerToolUse: map[string]pipeline.ToolUseVerdict{
			"toolu_1": {
				Outcome: pipeline.OutcomeAllow,
				Reason:  "tool_use carries a script-session caller token",
				Facts:   []pipeline.EvaluationFact{scriptFact},
			},
		},
		Evaluations: []pipeline.ToolUseEvaluation{
			{EvaluatorName: "script_session", ToolUseID: "toolu_1", Verdict: pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeAllow, Facts: []pipeline.EvaluationFact{scriptFact}}},
		},
	}
	var rows []conversation.AuditEvent
	policies.EmitToolUseAuditRows(context.Background(), result, []conversation.ToolUse{tu}, nil, func(_ context.Context, r conversation.AuditEvent) {
		rows = append(rows, r)
	})
	if rows[0].Decision != "allow" {
		t.Errorf("Decision = %q, want allow", rows[0].Decision)
	}
	if rows[0].OutcomeName != "script_session_passthrough" {
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
	var rows []conversation.AuditEvent
	policies.EmitToolUseAuditRows(context.Background(), result, []conversation.ToolUse{tu}, insp, func(_ context.Context, r conversation.AuditEvent) {
		rows = append(rows, r)
	})
	if rows[0].InspectorVerdict.Host != "api.github.com" {
		t.Errorf("Verdict.Host = %q, want api.github.com", rows[0].InspectorVerdict.Host)
	}
	if !rows[0].InspectorVerdict.IsAPICall {
		t.Error("expected IsAPICall=true")
	}
}
