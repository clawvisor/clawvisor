package policies

import (
	"bytes"
	"context"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// ExpiredTaskNotice tells the model when its currently-active task has
// lapsed past its ExpiresAt. Without this signal the next tool_use is
// silently scope-denied by the existing task-scope evaluator and the
// model has to infer what happened from the denial reason; the
// preprocess notice gets ahead of that, telling the model directly so
// it can acknowledge the lapse and POST a fresh task (or check out a
// different active one) on the same turn.
//
// The policy is THIN — the lookup of "which task is currently active
// for this agent, and is it past ExpiresAt?" is injected as a loader
// callback (the handler owns the store and the checkout registry).
// Body transformation lives in the parent llmproxy package
// (PrependExpiredTaskNoticeToLastUserMessage) and is reached through
// the loader's Inject callback so this file stays decoupled from the
// llmproxy package.
type ExpiredTaskNotice struct {
	loader ExpiredCheckedOutTaskLoader
	inject ExpiredTaskNoticeInjector
}

// ExpiredCheckedOutTaskLoader resolves whether the conversation's
// active task has expired. Returns expired=true with the task ID +
// purpose only when the agent has a checked-out task whose ExpiresAt
// is non-nil and in the past. Standing tasks (ExpiresAt == nil), no
// checkout, store outages, and tasks whose pointer is stale all
// return expired=false. Errors are swallowed inside the loader — the
// policy stays silent rather than denying on transient store outages.
type ExpiredCheckedOutTaskLoader func(ctx context.Context, userID, agentID, conversationID string) (taskID, purpose string, expired bool)

// ExpiredTaskNoticeInjector performs the body transform when the
// loader reports an expired task. Implementations call
// llmproxy.PrependExpiredTaskNoticeToLastUserMessage internally; the
// indirection keeps the policy file out of the parent llmproxy
// package's import graph (same shape InlineTaskAugment uses).
type ExpiredTaskNoticeInjector func(body []byte, provider conversation.Provider, taskID, purpose string) ([]byte, bool, error)

// NewExpiredTaskNotice constructs the policy. A nil loader or nil
// injector turns the policy into a no-op (skip on every request) so
// the handler can register it unconditionally without first checking
// store wiring.
func NewExpiredTaskNotice(loader ExpiredCheckedOutTaskLoader, inject ExpiredTaskNoticeInjector) *ExpiredTaskNotice {
	return &ExpiredTaskNotice{loader: loader, inject: inject}
}

// Name returns the audit-friendly policy identifier.
func (ExpiredTaskNotice) Name() string { return "expired_task_notice" }

// expiredTaskSentinelPrefix mirrors llmproxy.ExpiredTaskNoticeSentinel.
// We duplicate the constant rather than import the parent package to
// keep this file out of the llmproxy import graph (the same boundary
// every other policy file respects). Drift risk is tiny — the literal
// is a stable wire constant tested on both sides.
const expiredTaskSentinelPrefix = "expired_task_id="

// Preprocess injects the notice when the loader reports an expired
// active task AND the body doesn't already contain the per-taskID
// sentinel.
//
// Gating order is cheapest-first:
//  1. Empty UserID / AgentID → Skip (lookup can't scope).
//  2. count_tokens path → Skip (mirrors ControlNotice; not a real LLM
//     call, no use injecting state).
//  3. Non-Anthropic provider → Skip (v1 Anthropic-only; the body
//     transform doesn't have an OpenAI walker yet).
//  4. Loader returns expired=false → Skip.
//  5. Body already contains the per-taskID sentinel → Skip (already
//     announced in this conversation). Once Anthropic echoes the
//     augmented history on the next turn, this branch keeps us
//     idempotent without per-conversation state.
//  6. Inject. If the injector reports no modification (e.g., the
//     latest user-role messages are all internal verbs / tool_results
//     so nothing genuine to anchor to), Skip — we'll try again on the
//     next turn when a genuine human message arrives.
func (p *ExpiredTaskNotice) Preprocess(ctx context.Context, req pipeline.ReadOnlyRequest, mut pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	if p == nil || p.loader == nil || p.inject == nil {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	userID := req.UserID()
	agentID := req.AgentID()
	if userID == "" || agentID == "" {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	if h := req.HTTPRequest(); h != nil && strings.HasSuffix(h.URL.Path, "/count_tokens") {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	// Provider gating lives inside the injector: the body editor in
	// llmproxy handles only Anthropic-shaped bodies today and reports
	// modified=false for everything else, which falls through to the
	// no-modification Skip branch below.

	taskID, purpose, expired := p.loader(ctx, userID, agentID, req.ConversationID())
	if !expired || strings.TrimSpace(taskID) == "" {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}

	body := req.RawBody()
	if bytes.Contains(body, []byte(expiredTaskSentinelPrefix+taskID)) {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}

	newBody, modified, err := p.inject(body, req.Provider(), taskID, purpose)
	if err != nil {
		// Body malformation is a downstream concern, not a deny signal —
		// the validator on the forward step will surface it. Record the
		// error in audit and proceed without modification.
		return pipeline.RequestVerdict{
			Outcome: pipeline.OutcomeSkip,
			Reason:  err.Error(),
			AuditParams: map[string]any{
				"expired_task_notice_error": err.Error(),
			},
		}, nil
	}
	if !modified {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	if err := mut.ReplaceBody(newBody); err != nil {
		return pipeline.RequestVerdict{}, err
	}
	return pipeline.RequestVerdict{
		Outcome: pipeline.OutcomeAllow,
		AuditParams: map[string]any{
			"expired_task_notice_injected": true,
			"expired_task_id":              taskID,
		},
	}, nil
}

var _ pipeline.RequestPolicy = (*ExpiredTaskNotice)(nil)
