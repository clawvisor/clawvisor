package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// ToolUseAuditSink receives one typed conversation.AuditEvent per
// tool_use. The host wires this to AuditEmitter.WriteAuditEvent.
type ToolUseAuditSink func(ctx context.Context, ev conversation.AuditEvent)

// EmitToolUseAuditRows walks the per-tool verdicts in result and emits
// one typed AuditEvent per tool_use via sink. Outcome name derives
// from typed Facts (Phase 2); InspectorVerdict is re-derived via the
// supplied inspector (idempotent).
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
	// Consume the pipeline's typed AuditEvents stream rather than
	// reconstructing winner-evaluator mappings + fact aggregation
	// inline. AuditEvent carries Winning, Decision, Facts, and the
	// EvaluatorName per (tool_use × evaluator) row.
	events := result.AuditEvents(toolUses)

	// Group typed facts by tool_use across the full trail so observation
	// flowing through Skip evaluators (TaskScopeFact's MatchedTaskID on
	// a credentialed-rewrite path, etc.) reaches the winning row.
	factsByTU := make(map[string][]pipeline.EvaluationFact, len(toolUses))
	for _, ev := range events {
		factsByTU[ev.ToolUse.ID] = append(factsByTU[ev.ToolUse.ID], ev.Facts...)
	}

	emittedFor := make(map[string]bool, len(toolUses))
	for _, ev := range events {
		if !ev.Winning {
			continue
		}
		if emittedFor[ev.ToolUse.ID] {
			continue
		}
		emittedFor[ev.ToolUse.ID] = true

		// Pull the canonical reason from PerToolUse (the winning verdict
		// the orchestrator recorded) rather than the AuditEvent — the
		// trail Evaluations may store an abbreviated Reason while
		// PerToolUse carries the final richer form.
		winningV := result.PerToolUse[ev.ToolUse.ID]
		out := conversation.AuditEvent{
			ToolUse:       ev.ToolUse,
			EvaluatorName: ev.EvaluatorName,
			Outcome:       ev.Outcome,
			Decision:      ev.Decision,
			Reason:        winningV.Reason,
			Facts:         ev.Facts,
			Winning:       true,
		}
		if out.Reason == "" {
			out.Reason = ev.Reason
		}
		if insp != nil {
			out.InspectorVerdict = insp.Inspect(ctx, inspector.ToolUse{
				ID:    ev.ToolUse.ID,
				Name:  ev.ToolUse.Name,
				Input: ev.ToolUse.Input,
			})
		}
		out.OutcomeName = outcomeNameFor(ev.EvaluatorName, winningV, ev.Facts)
		out.TaskID = matchedTaskIDFromFacts(factsByTU[ev.ToolUse.ID])
		sink(ctx, out)
	}
}

// matchedTaskIDFromFacts walks a tool_use's accumulated facts looking
// for the first TaskScopeFact carrying a MatchedTaskID. TaskScope
// evaluators may emit the fact on Skip paths (e.g., credentialed
// rewrite where TaskScope sees the match but CredentialRewrite claims
// the verdict).
func matchedTaskIDFromFacts(facts []pipeline.EvaluationFact) string {
	for _, f := range facts {
		if tf, ok := f.(pipeline.TaskScopeFact); ok && tf.MatchedTaskID != "" {
			return tf.MatchedTaskID
		}
	}
	return ""
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
// winning verdict's typed Facts. Each evaluator's Fact carries the
// outcome string directly; the type switch here mirrors the per-stage
// outcome naming the legacy newToolUseEvaluator's audit calls used.
func outcomeNameFor(evaluatorName string, v pipeline.ToolUseVerdict, facts []pipeline.EvaluationFact) string {
	for _, f := range v.Facts {
		switch ff := f.(type) {
		case pipeline.ControlFact:
			if ff.Outcome != "" {
				return ff.Outcome
			}
		case pipeline.RewriteFact:
			if ff.Outcome != "" {
				return ff.Outcome
			}
		case pipeline.ScriptSessionFact:
			if ff.Outcome != "" {
				return ff.Outcome
			}
		case pipeline.TaskScopeFact:
			if ff.Reason != "" {
				if ff.Allowed {
					return "matched_task_scope"
				}
				return "task_scope_missing"
			}
		case pipeline.BoundaryFact:
			if !ff.Passed {
				return "boundary_check_failed"
			}
		}
	}
	// Generic fallback per outcome — used when the winning verdict
	// carried no stage-specific fact (e.g., default Allow fallthrough).
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
