package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/shellpolicy"
	"github.com/clawvisor/clawvisor/pkg/runtime/toolnames"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// ReadOnlyShellPassthroughPolicy allows read-only shell commands
// (e.g., `ls`, `cat`, `find … -name …`) without going through full
// task-scope authorization, when the agent has the
// read-only-shell-commands-allowed rule. Sensitive-path commands are
// left unclaimed so AuthorizationPolicy can route them through the
// task-scope approval flow.
type ReadOnlyShellPassthroughPolicy struct {
	inspector *inspector.Inspector
	resolver  ReadOnlyShellResolver
}

// ReadOnlyShellResolver returns the per-call inputs (agent ID + tool
// rules) the policy needs to consult its allow/deny rule. Returning
// nil makes the policy Skip (no agent context → can't decide).
type ReadOnlyShellResolver func(ctx context.Context, tu conversation.ToolUse) *ReadOnlyShellInputs

// ReadOnlyShellInputs is the per-call bundle the host supplies.
type ReadOnlyShellInputs struct {
	AgentID   string
	ToolRules []*store.RuntimePolicyRule
}

// NewReadOnlyShellPassthroughPolicy constructs the policy. Nil
// inspector or resolver → Skip-always.
func NewReadOnlyShellPassthroughPolicy(insp *inspector.Inspector, resolver ReadOnlyShellResolver) *ReadOnlyShellPassthroughPolicy {
	return &ReadOnlyShellPassthroughPolicy{inspector: insp, resolver: resolver}
}

// Name returns the audit-friendly evaluator identifier.
func (ReadOnlyShellPassthroughPolicy) Name() string { return "readonly_shell_passthrough" }

// Evaluate returns Allow when the tool_use is a read-only shell
// command for which the agent has the readonly-shell-allow rule.
// Skip otherwise (downstream stages handle).
func (p *ReadOnlyShellPassthroughPolicy) Evaluate(ctx context.Context, _ pipeline.ReadOnlyResponse, tu conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
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
	if !shellpolicy.ReadOnlyShellCommandsAllowed(tu.Name, in.AgentID, in.ToolRules) {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	cmd := shellpolicy.ShellCommandFromInput(tu.Input)
	if cmd == "" {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	// Sensitive-path commands are this policy's responsibility to NOT
	// claim — SensitivePathPolicy (upstream) emits the Deny.
	if toolnames.SensitiveFileGuardEnabled(tu.Name, in.AgentID, in.ToolRules) {
		if _, _, hit := inspector.CommandReferencesSensitivePath(cmd); hit {
			return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
		}
	}
	if ok, _ := inspector.IsReadOnlyBashCommand(cmd); !ok {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	return pipeline.ToolUseVerdict{
		Outcome: pipeline.OutcomeAllow,
		Reason:  "read-only shell command",
		Facts: []pipeline.EvaluationFact{
			pipeline.AuthorizationFact{Outcome: "readonly_shell_pass_through"},
		},
	}, nil
}

var _ pipeline.ToolUseEvaluator = (*ReadOnlyShellPassthroughPolicy)(nil)
