package pipeline

import (
	"context"
	"encoding/json"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// capturedToolMutations records per-tool-use mutations queued by
// ToolUseEvaluators so the bridge can translate them back into the
// conversation.ToolUseVerdict shape the legacy response rewriters
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

// BridgeToolUseEvaluator runs the supplied pipeline evaluators against
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
// Continuation: ContinueSignal carries structured synthetic turn
// blocks, which the conversation.ToolUseVerdict surfaces as
// stringified ContinueWithToolResult. The bridge concatenates the
// continuation's tool-result block JSON — matching how the legacy
// newToolUseEvaluator emits continuation text. The orchestrator
// guarantees only one tool_use carries Continue, so siblings fall back
// to Allowed.
func BridgeToolUseEvaluator(
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
		cv := conversation.ToolUseVerdict{
			Allowed: v.Outcome == OutcomeAllow || v.Outcome == OutcomeRewrite,
			Reason:  v.Reason,
		}
		if mu, ok := mutations[tu.ID]; ok && mu != nil {
			if len(mu.rewrittenInput) > 0 {
				cv.RewriteInput = mu.rewrittenInput
			}
			if mu.replacementText != "" {
				cv.SubstituteWith = mu.replacementText
			}
		}
		if v.Continue != nil && len(v.Continue.SyntheticToolResults) > 0 {
			var combined []byte
			for _, blk := range v.Continue.SyntheticToolResults {
				combined = append(combined, blk...)
			}
			cv.ContinueWithToolResult = string(combined)
			cv.PrependAssistantNotice = v.Continue.PrependNotice
		}
		// ContinueWithToolResultText is the flat-text variant: refusal
		// paths use it to feed recovery guidance back to the model via
		// a synthetic tool_result without building a full assistant
		// turn (which Continue/SyntheticToolResults would).
		if v.ContinueWithToolResultText != "" {
			cv.ContinueWithToolResult = v.ContinueWithToolResultText
		}
		return cv
	}
	return eval, result, nil
}
