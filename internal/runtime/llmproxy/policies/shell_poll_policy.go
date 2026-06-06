package policies

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// ShellPollPassthroughPolicy recognizes harness polls on a background
// shell — Codex's `write_stdin` with empty `chars` — and allows them
// through without going through full task-scope authorization. The
// background shell is read-equivalent: polling it is a no-op.
//
// Claims the verdict only when the inspector classifies the call as
// SourceTriggerMiss and the tool shape matches the poll pattern.
type ShellPollPassthroughPolicy struct {
	inspector *inspector.Inspector
}

// NewShellPollPassthroughPolicy constructs the policy. Nil inspector
// degrades to Skip-always.
func NewShellPollPassthroughPolicy(insp *inspector.Inspector) *ShellPollPassthroughPolicy {
	return &ShellPollPassthroughPolicy{inspector: insp}
}

// Name returns the audit-friendly evaluator identifier.
func (ShellPollPassthroughPolicy) Name() string { return "shell_poll_passthrough" }

// Evaluate returns Allow when the tool_use is a recognized background-
// shell poll. Otherwise Skip.
func (p *ShellPollPassthroughPolicy) Evaluate(ctx context.Context, _ pipeline.ReadOnlyResponse, tu conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	if p.inspector == nil {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	v := p.inspector.Inspect(ctx, inspector.ToolUse{
		ID:    tu.ID,
		Name:  tu.Name,
		Input: tu.Input,
	})
	// Only credentialed calls are claimed by downstream stages; a
	// background-shell poll is a trigger-miss + write_stdin-with-empty-
	// chars shape.
	if v.Source != inspector.SourceTriggerMiss {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	if !isShellPollTool(tu.Name, tu.Input) {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	return pipeline.ToolUseVerdict{
		Outcome: pipeline.OutcomeAllow,
		Reason:  "background-shell poll (" + tu.Name + ")",
		Facts: []pipeline.EvaluationFact{
			pipeline.ScriptSessionFact{Outcome: "shell_poll_pass_through"},
		},
	}, nil
}

var _ pipeline.ToolUseEvaluator = (*ShellPollPassthroughPolicy)(nil)

// isShellPollTool reports whether a tool_use is a harness poll on a
// background shell — Codex's `write_stdin` with empty `chars`.
// Mirrors the helper in llmproxy/shell_helpers.go but duplicated here
// so the policy package owns the gate.
func isShellPollTool(name string, raw json.RawMessage) bool {
	if name != "write_stdin" || len(raw) == 0 {
		return false
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return false
	}
	chars, ok := input["chars"].(string)
	if !ok {
		return false
	}
	return strings.TrimSpace(chars) == ""
}
