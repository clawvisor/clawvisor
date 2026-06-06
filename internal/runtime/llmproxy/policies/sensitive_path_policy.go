package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/pkg/runtime/toolnames"
)

// SensitivePathPolicy denies shell tool_uses that reference a
// sensitive file path (under the sensitive-file guard rule). Sensitive
// paths in shells override the read-only-shell allow gate — the
// command may LOOK read-only but the targeted file is sensitive
// regardless.
//
// Runs UPSTREAM of ReadOnlyShellPassthroughPolicy in the chain so a
// sensitive read-only command is Denied here rather than allowed
// through downstream.
//
// Decomposed from the trigger-miss authorization helper (Phase 6).
type SensitivePathPolicy struct {
	inspector *inspector.Inspector
	resolver  ReadOnlyShellResolver
}

// NewSensitivePathPolicy constructs the policy. nil inspector or
// resolver → Skip-always.
func NewSensitivePathPolicy(insp *inspector.Inspector, resolver ReadOnlyShellResolver) *SensitivePathPolicy {
	return &SensitivePathPolicy{inspector: insp, resolver: resolver}
}

// Name returns the audit-friendly evaluator identifier.
func (SensitivePathPolicy) Name() string { return "sensitive_path" }

// Evaluate returns Deny when the tool_use references a sensitive
// path under the sensitive-file guard. Otherwise Skip.
func (p *SensitivePathPolicy) Evaluate(ctx context.Context, _ pipeline.ReadOnlyResponse, tu conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	if p.inspector == nil || p.resolver == nil {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	if !toolnames.IsShellToolName(tu.Name) {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	v := p.inspector.Inspect(ctx, inspector.ToolUse{
		ID:    tu.ID,
		Name:  tu.Name,
		Input: tu.Input,
	})
	if v.Source != inspector.SourceTriggerMiss {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	in := p.resolver(ctx, tu)
	if in == nil {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	if !toolnames.SensitiveFileGuardEnabled(tu.Name, in.AgentID, in.ToolRules) {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	cmd := shellCommandFromInput(tu.Input)
	if cmd == "" {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	tok, reason, hit := inspector.CommandReferencesSensitivePath(cmd)
	if !hit {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	// Emit a Skip with a typed fact rather than claiming Deny outright.
	// AuthorizationPolicy downstream runs EvaluateAuthorization, which
	// (with no decision-engine inputs configured) yields NeedsApproval
	// + the "no matching task scope" approval prompt — matching the
	// legacy multi-row sensitive-path behavior. AuthorizationPolicy
	// reads the sensitive-path fact off the verdict trail to force
	// authorization even when no policy is configured.
	_, _ = tok, reason
	return pipeline.ToolUseVerdict{
		Outcome: pipeline.OutcomeSkip,
		Facts: []pipeline.EvaluationFact{
			pipeline.ScriptSessionFact{Outcome: "sensitive_path_in_read_only_shell"},
		},
	}, nil
}

var _ pipeline.ToolUseEvaluator = (*SensitivePathPolicy)(nil)
