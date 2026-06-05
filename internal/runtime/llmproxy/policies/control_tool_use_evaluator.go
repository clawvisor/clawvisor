package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// ControlToolUseEvaluator routes control-plane tool_uses (the model's
// curls to /api/control/{skill,tasks,...}) through the proxy's nonce
// minting + URL rewriting. It also dispatches inline-task definitions
// through a separately-injected interceptor.
//
// This is the first stage of the tool_use evaluation chain — control
// tool_uses must be detected and rewritten before any inspector or
// task-scope evaluator runs, because the rewriter changes the call's
// URL/host/path (and the audit row's classification follows).
//
// The struct holds only the host-supplied dependencies via a resolver
// closure; identity carriers (AgentID, request, etc.) stay out of the
// signature.
type ControlToolUseEvaluator struct {
	resolver ControlToolUseResolver
}

// ControlToolUseInputs is the per-call bundle the handler supplies via
// the resolver. The resolver returns nil to signal "no control routing
// for this call" (e.g., no ControlBaseURL configured).
type ControlToolUseInputs struct {
	// ControlBaseURL is the public-facing control endpoint host (the
	// "clawvisor.local" synthetic). Empty disables the evaluator.
	ControlBaseURL string
	// AgentID identifies the caller for nonce minting + audit.
	AgentID string
	// CallerNonces mints + consumes the per-call nonces the rewritten
	// URL embeds in X-Clawvisor-Caller.
	CallerNonces llmproxy.CallerNonceCache
	// InterceptInline, when non-nil, handles inline task-definition
	// interception (model emits POST /api/control/tasks while the user
	// is mid-flight on a "task" gesture). Returns a verdict + true when
	// the call was claimed; otherwise the control rewrite proceeds.
	InterceptInline func(ctx context.Context, tu conversation.ToolUse, call llmproxy.ControlCall) (pipeline.ToolUseVerdict, bool)
}

// ControlToolUseResolver returns the per-tool-use inputs. Returning
// nil makes the evaluator Skip — preserves the "no control configured"
// pass-through path.
type ControlToolUseResolver func(ctx context.Context, tu conversation.ToolUse) *ControlToolUseInputs

// NewControlToolUseEvaluator constructs the evaluator. A nil resolver
// makes it always Skip.
func NewControlToolUseEvaluator(resolver ControlToolUseResolver) *ControlToolUseEvaluator {
	return &ControlToolUseEvaluator{resolver: resolver}
}

// Name returns the audit-friendly identifier.
func (ControlToolUseEvaluator) Name() string { return "control_tool_use" }

// Evaluate routes control-plane tool_uses.
//
// Branches:
//   - resolver returns nil or ControlBaseURL is empty → Skip
//   - tool_use parses as a well-formed control curl → inline-task
//     intercept (when configured), else mint nonce + rewrite URL.
//     Returns OutcomeRewrite on success; OutcomeDeny when nonce mint
//     or rewrite fails.
//   - tool_use mentions the control endpoint but isn't well-formed →
//     mint a failure nonce + rewrite to the synthetic failure path.
//     Returns OutcomeDeny when failure rewrite isn't possible.
//   - tool_use doesn't touch the control plane → Skip
func (e *ControlToolUseEvaluator) Evaluate(ctx context.Context, _ pipeline.ReadOnlyResponse, tu conversation.ToolUse, mut pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	if e.resolver == nil {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	in := e.resolver(ctx, tu)
	if in == nil || in.ControlBaseURL == "" {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}

	call, ok := llmproxy.ParseControlToolUseWithBase(tu, in.ControlBaseURL)
	if ok {
		// Inline task-definition takes priority over the regular rewrite.
		if in.InterceptInline != nil {
			if v, claimed := in.InterceptInline(ctx, tu, call); claimed {
				return v, nil
			}
		}
		return e.rewriteControlCall(ctx, tu, mut, in, call)
	}

	if llmproxy.ControlToolUseMentionsEndpoint(tu, in.ControlBaseURL) {
		return e.rewriteMalformedControlCall(ctx, tu, mut, in)
	}

	// Not a control-plane tool_use.
	return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
}

func (e *ControlToolUseEvaluator) rewriteControlCall(ctx context.Context, tu conversation.ToolUse, mut pipeline.ToolUseMutator, in *ControlToolUseInputs, call llmproxy.ControlCall) (pipeline.ToolUseVerdict, error) {
	if in.CallerNonces == nil {
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  "Clawvisor: caller nonce cache not configured; refusing to embed agent token in control tool_use",
			AuditFields: map[string]any{
				"control_outcome": "caller_nonce_unavailable",
			},
		}, nil
	}
	nonce, err := in.CallerNonces.Mint(ctx, in.AgentID, llmproxy.NonceTarget{
		Host:   call.Verdict.Host,
		Method: call.Verdict.Method,
		Path:   call.Verdict.Path,
	})
	if err != nil {
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  "Clawvisor: caller nonce mint failed — " + err.Error(),
			AuditFields: map[string]any{
				"control_outcome": "caller_nonce_mint_failed",
			},
		}, nil
	}
	rewritten, _, rewriteOK, rewriteErr := llmproxy.RewriteControlToolUse(tu, in.ControlBaseURL, nonce)
	if !rewriteOK {
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  "Clawvisor: control endpoint unavailable",
			AuditFields: map[string]any{
				"control_outcome": "control_unavailable",
			},
		}, nil
	}
	if rewriteErr != nil {
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  "Clawvisor: control endpoint rewrite refused — " + rewriteErr.Error(),
			AuditFields: map[string]any{
				"control_outcome": "control_rewriter_error",
			},
		}, nil
	}
	if mut != nil {
		if err := mut.RewriteArgs(rewritten); err != nil {
			return pipeline.ToolUseVerdict{}, err
		}
	}
	return pipeline.ToolUseVerdict{
		Outcome: pipeline.OutcomeRewrite,
		Reason:  call.Verdict.Reason,
		AuditFields: map[string]any{
			"control_outcome": "clawvisor_control",
			"target_host":     call.Verdict.Host,
			"target_method":   call.Verdict.Method,
			"target_path":     call.Verdict.Path,
		},
	}, nil
}

func (e *ControlToolUseEvaluator) rewriteMalformedControlCall(ctx context.Context, tu conversation.ToolUse, mut pipeline.ToolUseMutator, in *ControlToolUseInputs) (pipeline.ToolUseVerdict, error) {
	const failureReason = "malformed_control_command"
	if in.CallerNonces == nil {
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  "Clawvisor: control endpoint rewrite refused — use a single foreground curl to the control endpoint, with no pipes, subshells, redirects to output files, or extra shell commands",
			AuditFields: map[string]any{
				"control_outcome": "caller_nonce_unavailable",
				"failure_reason":  failureReason,
			},
		}, nil
	}
	nonce, err := in.CallerNonces.Mint(ctx, in.AgentID, llmproxy.NonceTarget{
		Host:   llmproxy.ControlSyntheticHost,
		Method: "POST",
		Path:   "/api/control/failure",
	})
	if err != nil {
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  "Clawvisor: caller nonce mint failed — " + err.Error(),
			AuditFields: map[string]any{
				"control_outcome": "caller_nonce_mint_failed",
				"failure_reason":  failureReason,
			},
		}, nil
	}
	rewritten, ok, rewriteErr := llmproxy.RewriteControlFailureToolUse(tu, in.ControlBaseURL, nonce, failureReason)
	if !ok {
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  "Clawvisor: control endpoint rewrite refused — use a single foreground curl to the control endpoint, with no pipes, subshells, redirects to output files, or extra shell commands",
			AuditFields: map[string]any{
				"control_outcome": "control_rewriter_error",
				"failure_reason":  failureReason,
			},
		}, nil
	}
	if rewriteErr != nil {
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  "Clawvisor: control endpoint failure rewrite refused — " + rewriteErr.Error(),
			AuditFields: map[string]any{
				"control_outcome": "control_rewriter_error",
				"failure_reason":  failureReason,
			},
		}, nil
	}
	if mut != nil {
		if err := mut.RewriteArgs(rewritten); err != nil {
			return pipeline.ToolUseVerdict{}, err
		}
	}
	return pipeline.ToolUseVerdict{
		Outcome: pipeline.OutcomeRewrite,
		Reason:  "malformed control endpoint command",
		AuditFields: map[string]any{
			"control_outcome": "clawvisor_control_failure",
			"failure_reason":  failureReason,
			"target_host":     llmproxy.ControlSyntheticHost,
			"target_method":   "POST",
			"target_path":     "/api/control/failure",
		},
	}, nil
}

var _ pipeline.ToolUseEvaluator = (*ControlToolUseEvaluator)(nil)
