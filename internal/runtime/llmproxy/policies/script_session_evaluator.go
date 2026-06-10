package policies

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/scriptjudge"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/scriptrecognition"
)

// scriptSessionJudgeCallTimeout bounds each Judge invocation. The
// evaluator runs on the response hot path; an agent that emits
// repeated URL-unrecognized shapes would otherwise serialize judge
// LLM round-trips through this stage. The cap is conservative
// relative to the harness-side LLM client timeout (15s) so a stuck
// upstream short-circuits here without blocking the chain.
//
// A full circuit breaker + per-agent rate limit is a follow-up; this
// is the minimal protection against latency amplification.
const scriptSessionJudgeCallTimeout = 8 * time.Second

// ScriptSessionEvaluator passes through tool_uses that are already
// shaped for the proxy's resolver mount via a script-session caller
// token. These calls carry a cv-script-* token in X-Clawvisor-Caller
// and a URL targeting the resolver — running the inspector chain on
// them would try to "rewrite" an already-rewritten curl and fail.
//
// Runs after ControlToolUseEvaluator and before InspectorChain. The
// gate requires BOTH the script-session header AND a URL pointing at
// the resolver host; mismatched off-proxy curls fall through to the
// inspector chain — unless the deterministic recognizer reports
// URLUnrecognized (clear script-session intent but the URL the
// literal-prefix recognizer can see doesn't target the resolver, e.g.
// the agent variable-ized the URL/header), in which case the
// evaluator consults the LLM judge for re-classification.
type ScriptSessionEvaluator struct {
	resolver ScriptSessionResolver
}

// ScriptSessionInputs is the per-call bundle supplied by the host.
// ResolverBaseURL is the proxy's /api/proxy mount; an empty value
// disables the policy (the inspector chain handles all tool_uses).
// Judge, when non-nil, re-classifies URLUnrecognized tool_uses with
// an LLM — see scriptjudge.Judge.
type ScriptSessionInputs struct {
	ResolverBaseURL string
	Judge           scriptjudge.Judge
}

// ScriptSessionResolver returns per-call inputs. Returning nil makes
// the evaluator Skip.
type ScriptSessionResolver func(ctx context.Context, tu conversation.ToolUse) *ScriptSessionInputs

// NewScriptSessionEvaluator constructs the evaluator. A nil resolver
// makes it always Skip.
func NewScriptSessionEvaluator(resolver ScriptSessionResolver) *ScriptSessionEvaluator {
	return &ScriptSessionEvaluator{resolver: resolver}
}

// Name returns the audit-friendly identifier.
func (ScriptSessionEvaluator) Name() string { return "script_session" }

// Evaluate returns OutcomeAllow when the tool_use is a recognized
// script-session call; OutcomeDeny when the judge classifies a
// URL-unrecognized attempt as a real block with actionable guidance;
// otherwise Skip.
func (e *ScriptSessionEvaluator) Evaluate(ctx context.Context, _ pipeline.ReadOnlyResponse, tu conversation.ToolUse, _ pipeline.ToolUseMutator) (pipeline.ToolUseVerdict, error) {
	if e.resolver == nil {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	in := e.resolver(ctx, tu)
	if in == nil || in.ResolverBaseURL == "" {
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	switch scriptrecognition.Recognize(tu.Input, in.ResolverBaseURL) {
	case scriptrecognition.Passthrough:
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeAllow,
			Reason:  "tool_use carries a script-session caller token; resolver enforces scope",
			Facts:   []pipeline.EvaluationFact{pipeline.ScriptSessionFact{Outcome: "script_session_passthrough"}},
		}, nil
	case scriptrecognition.URLUnrecognized:
		// The agent emitted clear script-session signals but the
		// literal-prefix recognizer can't see the URL. Ask the LLM
		// judge to re-classify; on transport/parse error, fall
		// through to the inspector chain (Skip) so the agent gets
		// the inspector's generic refusal rather than a spurious
		// allow.
		if in.Judge == nil {
			return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
		}
		token := scriptjudge.ExtractToken(string(tu.Input))
		// Bound each judge call with a deadline shorter than the
		// LLM client's own timeout so a wedged upstream can't pin
		// this hot-path stage.
		judgeCtx, cancel := context.WithTimeout(ctx, scriptSessionJudgeCallTimeout)
		verdict, err := in.Judge.Judge(judgeCtx, scriptjudge.Input{
			ToolName:        tu.Name,
			ToolInput:       tu.Input,
			ResolverBaseURL: in.ResolverBaseURL,
			CVScriptToken:   token,
		})
		cancel()
		if err != nil {
			// ErrNotConfigured means "no judge wired" (Noop, or
			// LLMJudge with verification disabled). Falling through
			// without an audit fact matches the "no judge configured"
			// outcome a caller using `nil` would see — emitting a
			// judge_error fact here would mislead operators into
			// chasing a phantom transport failure.
			if errors.Is(err, scriptjudge.ErrNotConfigured) {
				return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
			}

			if ctx.Err() != nil {
				return pipeline.ToolUseVerdict{}, ctx.Err()
			}

			// Bounded latency / timeout defense: if the LLM judge sub-context timed out
			// (deadline exceeded while parent context is still active), deny the tool call
			// with a clear retry message rather than falling back to a manual
			// approval prompt (Skip) that would stall a headless session.
			if errors.Is(err, context.DeadlineExceeded) || judgeCtx.Err() == context.DeadlineExceeded {
				return pipeline.ToolUseVerdict{
					Outcome: pipeline.OutcomeDeny,
					Reason:  "Clawvisor: script-session call refused — LLM intent judge timed out (" + scriptSessionJudgeCallTimeout.String() + " limit reached). Please retry.",
					Facts: []pipeline.EvaluationFact{pipeline.ScriptSessionFact{
						Outcome:           "script_session_judge_timeout",
						JudgePromptSHA:    verdict.PromptSHA,
						JudgeLatencyMS:    verdict.LatencyMS,
						JudgeInputTokens:  verdict.InputTokens,
						JudgeOutputTokens: verdict.OutputTokens,
						JudgeError:        "timeout",
					}},
				}, nil
			}

			// Real error: fall through to inspector chain, but
			// emit an audit-only fact so the judge attempt is
			// forensically visible — operators can see latency +
			// error rates without inferring from logs alone.
			//
			// Redact sensitive substrings before crossing into the
			// audit row. Some LLM clients fold upstream response
			// bodies into error strings; if that body echoed the
			// user message (which carried tool_use input) the
			// audit DB would otherwise grow a credential vector.
			// The wrapped error itself keeps the original chain
			// intact for errors.Is callers higher up.
			return pipeline.ToolUseVerdict{
				Outcome: pipeline.OutcomeSkip,
				Facts: []pipeline.EvaluationFact{pipeline.ScriptSessionFact{
					Outcome:           "script_session_judge_error",
					JudgePromptSHA:    verdict.PromptSHA,
					JudgeLatencyMS:    verdict.LatencyMS,
					JudgeInputTokens:  verdict.InputTokens,
					JudgeOutputTokens: verdict.OutputTokens,
					JudgeError:        scriptjudge.RedactJudgeError(err.Error()),
				}},
			}, nil
		}
		if verdict.Allow {
			return pipeline.ToolUseVerdict{
				Outcome: pipeline.OutcomeAllow,
				Reason:  verdict.Reason,
				Facts: []pipeline.EvaluationFact{pipeline.ScriptSessionFact{
					Outcome:           "script_session_judge_allow",
					JudgePromptSHA:    verdict.PromptSHA,
					JudgeLatencyMS:    verdict.LatencyMS,
					JudgeInputTokens:  verdict.InputTokens,
					JudgeOutputTokens: verdict.OutputTokens,
				}},
			}, nil
		}
		guidance := strings.TrimSpace(verdict.AgentGuidance)
		if guidance == "" {
			guidance = "the call doesn't appear to target the resolver at " + in.ResolverBaseURL
		}
		return pipeline.ToolUseVerdict{
			Outcome: pipeline.OutcomeDeny,
			Reason:  "Clawvisor: script-session call refused — " + guidance,
			Facts: []pipeline.EvaluationFact{pipeline.ScriptSessionFact{
				Outcome:           "script_session_judge_block",
				JudgePromptSHA:    verdict.PromptSHA,
				JudgeLatencyMS:    verdict.LatencyMS,
				JudgeInputTokens:  verdict.InputTokens,
				JudgeOutputTokens: verdict.OutputTokens,
			}},
		}, nil
	case scriptrecognition.NoMatch:
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	default:
		// Unrecognized recognition state — Skip and let the chain
		// continue. A new state added to the recognizer should be
		// handled explicitly above; this default is the conservative
		// fallback.
		return pipeline.ToolUseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
}

var _ pipeline.ToolUseEvaluator = (*ScriptSessionEvaluator)(nil)
