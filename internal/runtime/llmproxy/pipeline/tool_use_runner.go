package pipeline

import (
	"context"
	"encoding/json"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// capturedToolMutations records per-tool-use mutations queued by
// ToolUseEvaluators so the bridge can translate them back into the
// conversation.ToolUseVerdict shape the response rewriters
// (AnthropicResponseRewriter, OpenAIResponseRewriter) consume.
type capturedToolMutations struct {
	rewrittenInput  json.RawMessage
	replacementText string
}

type captureMutator struct {
	mu *capturedToolMutations
}

func (m *captureMutator) RewriteArgs(newInput json.RawMessage) error {
	// Copy — evaluators may reuse the buffer they passed in.
	m.mu.rewrittenInput = append([]byte(nil), newInput...)
	return nil
}

func (m *captureMutator) ReplaceWithText(text string) error {
	m.mu.replacementText = text
	return nil
}

// RunToolUseEvaluators runs the supplied pipeline evaluators against
// the response's tool_uses via EvaluateToolUses and returns a
// conversation.ToolUseEvaluator closure that the existing response
// rewriters can consume. The closure looks up each tool_use's verdict
// from the pre-computed PerToolUse map and surfaces any mutations the
// evaluators queued (RewriteArgs → RewriteInput, ReplaceWithText →
// SubstituteWith).
//
// The returned *ToolUseResult is exposed so the caller can drive
// coalescing decisions (CoalesceHolds, ShouldCoalesce) over the full
// set of per-tool verdicts before emitting audit rows.
//
// Continuation: ContinueSignal stays structured through this bridge.
// Final provider adapters call ToolUseVerdict.ContinuationToolResultContent
// when they need the provider-specific tool_result text payload.
// The orchestrator guarantees only one tool_use carries Continue, so
// siblings fall back to Allowed.
func RunToolUseEvaluators(
	ctx context.Context,
	res ReadOnlyResponse,
	toolUses []conversation.ToolUse,
	evaluators []ToolUseEvaluator,
) (conversation.ToolUseEvaluator, *ToolUseResult, error) {
	mutations := make(map[string]*capturedToolMutations, len(toolUses))
	factory := func(id string) ToolUseMutator {
		m := &capturedToolMutations{}
		mutations[id] = m
		return &captureMutator{mu: m}
	}
	result, err := EvaluateToolUses(ctx, res, toolUses, evaluators, factory)
	if err != nil {
		return nil, nil, err
	}
	eval := func(tu conversation.ToolUse) conversation.ToolUseVerdict {
		v, ok := result.PerToolUse[tu.ID]
		if !ok {
			// Tool_use wasn't in the input set (shouldn't happen for a
			// rewriter reusing the same response object) — default to
			// allow rather than silently substituting.
			return conversation.ToolUseVerdict{Allowed: true}
		}
		// Verdict type is unified across pipeline and rewriters. Set
		// the derived Allowed bool from Outcome so rewriter readers
		// stay correct, then merge any queued mutator state.
		v.Allowed = v.Outcome == conversation.OutcomeAllow || v.Outcome == conversation.OutcomeRewrite
		// Mutator-side mutations (RewriteArgs / ReplaceWithText) take
		// precedence over verdict-side fields. The mutator is the
		// imperative API for evaluators that prefer to queue mutations
		// alongside the verdict return.
		if mu, ok := mutations[tu.ID]; ok && mu != nil {
			if len(mu.rewrittenInput) > 0 {
				v.RewriteInput = mu.rewrittenInput
			}
			if mu.replacementText != "" {
				v.SubstituteWith = mu.replacementText
			}
		}
		return v
	}
	return eval, result, nil
}
