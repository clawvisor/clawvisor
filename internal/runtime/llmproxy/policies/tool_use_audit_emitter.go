package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// ToolUseAuditRow is the per-tool-use audit shape the host emits to
// the store. Translation from pipeline.ToolUseResult to this shape
// lives here so the evaluator chain stays free of audit-emit concerns,
// while the caller stays free of pipeline-shape concerns.
//
// Field semantics match store.AuditEntry's per-tool-use row:
//   - Decision: "allow" | "block" | "rewrite"
//   - Outcome: stage-specific outcome name (task_scope_missing,
//     caller_nonce_unavailable, success, etc.)
//   - Reason: human-readable explanation; surfaced as the audit row's
//     Reason and as part of any user-visible refusal text upstream.
//   - Verdict: the inspector.Verdict for this tool_use. Re-derived
//     inside Emit by running the inspector (idempotent), so callers
//     don't have to thread it through pipeline state. Empty when the
//     emitter has no inspector configured.
//   - TaskID: matched task ID when known (TaskScopeEvaluator surfaces
//     this via the matched_task_id AuditField).
type ToolUseAuditRow struct {
	ToolUse  conversation.ToolUse
	Verdict  inspector.Verdict
	Decision string
	Outcome  string
	Reason   string
	TaskID   string
}

// ToolUseAuditSink receives one audit row per tool_use. The host
// implementation typically wraps llmproxy.AuditEmitter.LogToolUseInspected
// with the row's fields.
type ToolUseAuditSink func(ctx context.Context, row ToolUseAuditRow)

// EmitToolUseAuditRows walks the per-tool verdicts in result and emits
// one audit row per tool_use via sink. The translation is:
//
//   - OutcomeAllow / OutcomeRewrite → Decision="allow" / "rewrite"
//   - OutcomeDeny / OutcomeHold → Decision="block"
//   - Outcome name reads from AuditFields keys (control_outcome,
//     rewrite_outcome, path, task_scope_*) — falls back to generic
//     names when no key is present.
//   - The Reason on the pipeline verdict becomes the row Reason.
//   - The matched_task_id AuditField becomes TaskID.
//   - Verdict is re-derived by running insp.Inspect (idempotent).
//
// Sink and result may both be nil — the function no-ops.
func EmitToolUseAuditRows(
	ctx context.Context,
	result *pipeline.ToolUseResult,
	toolUses []conversation.ToolUse,
	insp *inspector.Inspector,
	sink ToolUseAuditSink,
) {
	if result == nil || sink == nil {
		return
	}
	// Build a quick lookup from tool_use ID → which evaluator's
	// non-Skip verdict won. The orchestrator's PerToolUse already
	// stores the winner; we walk Evaluations to find the producing
	// evaluator so the row's Outcome can be evaluator-aware.
	winnerEvaluator := make(map[string]string, len(toolUses))
	for _, ev := range result.Evaluations {
		if ev.Verdict.Outcome == pipeline.OutcomeSkip {
			continue
		}
		if _, claimed := winnerEvaluator[ev.ToolUseID]; claimed {
			continue
		}
		winnerEvaluator[ev.ToolUseID] = ev.EvaluatorName
	}

	for _, tu := range toolUses {
		v, ok := result.PerToolUse[tu.ID]
		if !ok {
			continue
		}
		row := ToolUseAuditRow{
			ToolUse: tu,
			Reason:  v.Reason,
		}
		if insp != nil {
			row.Verdict = insp.Inspect(ctx, inspector.ToolUse{
				ID:    tu.ID,
				Name:  tu.Name,
				Input: tu.Input,
			})
		}
		row.Decision = decisionFromOutcome(v.Outcome)
		row.Outcome = outcomeNameFor(winnerEvaluator[tu.ID], v)
		row.TaskID = taskIDFromAuditFields(v.AuditFields)
		if row.TaskID == "" {
			// matched_task_id may have been recorded on an earlier
			// Skip evaluation (e.g. TaskScopeEvaluator on a credentialed
			// rewrite path returns Skip + matched_task_id, and the
			// downstream CredentialRewriteEvaluator wins the verdict).
			// Walk the trail and take the first matched_task_id found.
			for _, ev := range result.Evaluations {
				if ev.ToolUseID != tu.ID {
					continue
				}
				if id := taskIDFromAuditFields(ev.Verdict.AuditFields); id != "" {
					row.TaskID = id
					break
				}
			}
		}
		sink(ctx, row)
	}
}

// decisionFromOutcome maps the pipeline Outcome enum to the legacy
// audit-row Decision string. The legacy code emits one of three
// values; the pipeline has more states, so Hold collapses into block
// (user-facing: held tool_use renders as a refusal with an approval
// prompt).
func decisionFromOutcome(o pipeline.Outcome) string {
	switch o {
	case pipeline.OutcomeAllow:
		return "allow"
	case pipeline.OutcomeRewrite:
		return "rewrite"
	case pipeline.OutcomeDeny, pipeline.OutcomeHold:
		return "block"
	default:
		// Skip / ShortCircuit shouldn't reach the emit path; default to
		// "allow" so accidental cases don't false-alarm in audit.
		return "allow"
	}
}

// outcomeNameFor extracts the stage-specific outcome name from the
// pipeline verdict's AuditFields. Different evaluators emit the name
// under different keys (control_outcome, rewrite_outcome, path); the
// fallback ladder mirrors the legacy newToolUseEvaluator's audit calls.
func outcomeNameFor(evaluatorName string, v pipeline.ToolUseVerdict) string {
	if v.AuditFields != nil {
		// Stage-specific keys, checked in evaluator-priority order.
		for _, key := range []string{
			"control_outcome",
			"rewrite_outcome",
			"path",
		} {
			if s, ok := stringField(v.AuditFields, key); ok && s != "" {
				return s
			}
		}
		// TaskScopeEvaluator emits task_scope_* fields; pick a concrete
		// outcome based on Allowed.
		if _, hasReason := v.AuditFields["task_scope_reason"]; hasReason {
			allowed, _ := boolField(v.AuditFields, "task_scope_allowed")
			if allowed {
				return "matched_task_scope"
			}
			return "task_scope_missing"
		}
		// Boundary-check failure path.
		if passed, ok := boolField(v.AuditFields, "boundary_check_passed"); ok && !passed {
			return "boundary_check_failed"
		}
	}
	// Generic fallback per outcome.
	switch v.Outcome {
	case pipeline.OutcomeAllow:
		switch evaluatorName {
		case "inspector_chain":
			return "boundary_check_passed"
		case "script_session":
			return "script_session_passthrough"
		default:
			return "pass_through"
		}
	case pipeline.OutcomeRewrite:
		return "success"
	case pipeline.OutcomeDeny:
		return "deny"
	case pipeline.OutcomeHold:
		return "approval_required"
	default:
		return ""
	}
}

func taskIDFromAuditFields(fields map[string]any) string {
	s, _ := stringField(fields, "matched_task_id")
	return s
}

func stringField(fields map[string]any, key string) (string, bool) {
	if fields == nil {
		return "", false
	}
	v, ok := fields[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func boolField(fields map[string]any, key string) (bool, bool) {
	if fields == nil {
		return false, false
	}
	v, ok := fields[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}
