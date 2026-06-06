package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// CredentialRewriteEvaluator rewrites a credentialed tool_use's URL +
// caller-token header so the call routes through the proxy's resolver.
//
//   - Mint a per-tool nonce bound to (agent, host, method, path).
//   - Run inspector.Rewrite to substitute the URL prefix + inject
//     X-Clawvisor-Caller.
//   - On success → OutcomeRewrite with the rewritten bytes queued via
//     mutator.RewriteArgs.
//   - On nonce or rewrite failure → OutcomeDeny with a Clawvisor
//     refusal message (and ContinueWithToolResult-equivalent payload
//     via the bridge's Continue translation).
//
// Runs after InspectorChain + TaskScopeEvaluator + IntentVerifyEvaluator
// in the credentialed chain. By the time this evaluator fires, the
// upstream stages have decided the call is allowed; this is purely the
// transport-layer rewrite.
//
// The evaluator runs its own inspector.Inspect call (idempotent with
// the InspectorChain's call) to recover the credentialed Verdict —
// pipeline.ToolUseVerdict doesn't carry typed inspector state between
// evaluators. The Inspector pass is the parser; re-running it adds
// negligible work compared with the rewrite + nonce mint itself.
type CredentialRewriteEvaluator struct {
	resolver CredentialRewriteResolver
}

// CredentialRewriteInputs is the per-call bundle the host supplies.
// Returning nil from the resolver — or any nil dependency — makes the
// evaluator Skip so the orchestrator's default-Allow path handles it.
type CredentialRewriteInputs struct {
	Inspector    *inspector.Inspector
	CallerNonces llmproxy.CallerNonceCache
	AgentID      string
	RewriteOpts  inspector.RewriteOpts
}

// CredentialRewriteResolver returns the per-call inputs. nil resolver
// → always Skip.
type CredentialRewriteResolver func(ctx context.Context, tu conversation.ToolUse) *CredentialRewriteInputs

// NewCredentialRewriteEvaluator constructs the evaluator.
func NewCredentialRewriteEvaluator(resolver CredentialRewriteResolver) *CredentialRewriteEvaluator {
	return &CredentialRewriteEvaluator{resolver: resolver}
}

// Name returns the audit-friendly identifier.
func (CredentialRewriteEvaluator) Name() string { return "credential_rewrite" }

// Evaluate runs inspector.Inspect to recover the credentialed verdict,
// then mints a nonce + rewrites the tool_use input.
func (e *CredentialRewriteEvaluator) Evaluate(ctx context.Context, _ pipeline.ReadOnlyResponse, tu conversation.ToolUse, mut pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	if e.resolver == nil {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	in := e.resolver(ctx, tu)
	if in == nil || in.Inspector == nil {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}

	v := in.Inspector.Inspect(ctx, inspector.ToolUse{
		ID:    tu.ID,
		Name:  tu.Name,
		Input: tu.Input,
	})

	// Only credentialed, non-ambiguous, non-stub calls get rewritten.
	// The InspectorChain has already returned Deny/Hold for the
	// failure paths; here we Skip so non-rewrite tool_uses fall
	// through to default-Allow.
	if v.Source == inspector.SourceTriggerMiss || v.Ambiguous || !v.IsAPICall {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	if inspector.AllPlaceholdersAreStubs(v.Placeholders) {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}

	if in.CallerNonces == nil {
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  "Clawvisor: caller nonce cache not configured; refusing to embed agent token in tool_use",
			Facts: []pipeline.EvaluationFact{pipeline.RewriteFact{
				Outcome:      "caller_nonce_unavailable",
				TargetHost:   v.Host,
				TargetMethod: v.Method,
				TargetPath:   v.Path,
			}},
		}, nil
	}
	nonce, mintErr := in.CallerNonces.Mint(ctx, in.AgentID, llmproxy.NonceTarget{
		Host:   v.Host,
		Method: v.Method,
		Path:   v.Path,
	})
	if mintErr != nil {
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  "Clawvisor: caller nonce mint failed — " + mintErr.Error(),
			Facts: []pipeline.EvaluationFact{pipeline.RewriteFact{
				Outcome:      "caller_nonce_mint_failed",
				TargetHost:   v.Host,
				TargetMethod: v.Method,
				TargetPath:   v.Path,
			}},
		}, nil
	}

	opts := in.RewriteOpts
	opts.CallerToken = nonce
	rewritten, err := inspector.Rewrite(inspector.ToolUse{
		ID:    tu.ID,
		Name:  tu.Name,
		Input: tu.Input,
	}, v, opts)
	if err != nil {
		reason := llmproxy.CredentialedRewriteRecoveryReason(v, err)
		return pipeline.ToolUseVerdict{
			Outcome:                pipeline.OutcomeDeny,
			Reason:                 reason,
			ContinueWithToolResult: reason,
			Facts: []pipeline.EvaluationFact{pipeline.RewriteFact{
				Outcome:      "rewriter_error",
				TargetHost:   v.Host,
				TargetMethod: v.Method,
				TargetPath:   v.Path,
			}},
		}, nil
	}
	if mut != nil {
		if err := mut.RewriteArgs(rewritten); err != nil {
			return pipeline.ToolUseVerdict{}, err
		}
	}
	return pipeline.ToolUseVerdict{
		Outcome: pipeline.OutcomeRewrite,
		Reason:  v.Reason,
		Facts: []pipeline.EvaluationFact{pipeline.RewriteFact{
			Outcome:      "success",
			TargetHost:   v.Host,
			TargetMethod: v.Method,
			TargetPath:   v.Path,
		}},
	}, nil
}

var _ pipeline.ToolUseEvaluator = (*CredentialRewriteEvaluator)(nil)
