package pipeline

import (
	"context"
	"fmt"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/observability"
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// PreResult is what Pipeline.RunPre returns after running every
// RequestPolicy in declared order.
type PreResult struct {
	// FinalBody is the request body after all queued mutations have
	// been applied. The handler forwards this to the upstream.
	FinalBody []byte

	// BodyReplaced is true when at least one policy explicitly replaced
	// the body through the RequestMutator, even if the replacement bytes
	// are equal to the original bytes.
	BodyReplaced bool

	// AuditParams is the aggregated map of audit-row fields each policy
	// emitted. Conflicts go to the last writer (orderly via declared
	// chain order). The handler merges this with its existing auditParams.
	AuditParams map[string]any

	// Verdicts is the per-policy verdict trail, in execution order.
	// Useful for tests asserting which policies fired. The handler can
	// ignore this; it's also where deny / short-circuit signals surface.
	Verdicts []PolicyVerdict

	// ShortCircuit, if non-nil, is the synthetic response a policy
	// emitted instead of forwarding upstream. The handler returns it
	// to the client without calling Forwarder.Forward.
	ShortCircuit *SyntheticResponse

	// DenyReason is non-empty if a policy returned OutcomeDeny. The
	// handler should respond 400/403 with this reason. Policy chain
	// halts on first Deny.
	DenyReason string

	// DeniedBy names which policy triggered the Deny (audit forensics).
	DeniedBy string

	// Observed lists every enforcing verdict that the Observe posture
	// downgraded to an observation on this chain (spec 02 §3). Empty
	// unless the request context carried WithObserveMode. When non-empty
	// the enforcement was NOT applied: DenyReason / ShortCircuit stay
	// clear and any non-mechanical body rewrite was rolled back.
	Observed []ObservedVerdict
}

// PolicyVerdict pairs a policy name with the verdict it returned.
type PolicyVerdict struct {
	Name    string
	Verdict RequestVerdict
}

// RunPre executes the policy chain in declared order. Mutations apply
// eagerly via the eagerRequestMutator: each policy sees the body left
// by its predecessors.
//
// Stops early on:
//   - First OutcomeDeny: DenyReason populated, remaining policies skipped.
//   - First OutcomeShortCircuit: ShortCircuit populated, remaining
//     policies AND the upstream forward both skipped.
//
// OutcomeSkip and OutcomeAllow continue the chain. Audit fields from
// every executed policy (including Skip and Deny) merge into the result.
//
// req's RawBody() supplies the initial body. The returned PreResult
// carries the final mutated body, which the handler forwards instead of
// the original.
func RunPre(ctx context.Context, req ReadOnlyRequest, policies []RequestPolicy) (*PreResult, error) {
	if req == nil {
		return nil, fmt.Errorf("pipeline.RunPre: nil request")
	}

	mut := newEagerRequestMutator(req.RawBody(), req.ValidateReplacementBody)
	result := &PreResult{
		AuditParams: make(map[string]any),
		Verdicts:    make([]PolicyVerdict, 0, len(policies)),
	}

	// Wrap req so each policy sees the *current* body (post any
	// preceding ReplaceBody). The orchestrator updates the wrapper as
	// mutations land.
	wrapper := &mutatingRequestWrapper{base: req, body: mut.Body()}

	// Observe posture (spec 02 §3): downgrade enforcing verdicts from
	// non-mechanical policies to recorded observations. observeBody holds
	// the body as it stood before each policy ran, so a downgraded policy
	// body rewrite can be rolled back (byte-fidelity).
	observeMode := ObserveModeFromContext(ctx)

	for _, policy := range policies {
		bodyBefore := mut.Body()
		verdict, err := policy.Preprocess(ctx, wrapper, mut)
		if err != nil {
			result.FinalBody = mut.Body()
			return result, fmt.Errorf("policy %q: %w", policy.Name(), err)
		}
		result.Verdicts = append(result.Verdicts, PolicyVerdict{Name: policy.Name(), Verdict: verdict})

		switch verdict.Outcome {
		case OutcomeDeny, OutcomeAllow, OutcomeSkip:
			if verdict.ShortCircuit != nil {
				return nil, fmt.Errorf("policy %q returned %s outcome with ShortCircuit payload", policy.Name(), verdict.Outcome)
			}
		case OutcomeShortCircuit:
			if verdict.ShortCircuit == nil {
				return nil, fmt.Errorf("policy %q returned ShortCircuit outcome with nil ShortCircuit payload", policy.Name())
			}
		default:
			return nil, fmt.Errorf("policy %q returned unsupported outcome %q for RunPre", policy.Name(), verdict.Outcome)
		}

		// Decide whether Observe mode neutralizes this verdict: only
		// enforcing verdicts (deny / short-circuit / a body rewrite) from
		// NON-mechanical policies are downgraded. Mechanical policies keep
		// acting so byte-fidelity and prompt-cache warmth are preserved.
		downgrade := observeMode && !observeExempt(policy.Name())
		enforcingRewrite := downgrade && mut.BodyReplaced() && !bytesEqual(mut.Body(), bodyBefore)
		enforcingVerdict := downgrade && (verdict.Outcome == OutcomeDeny || verdict.Outcome == OutcomeShortCircuit)

		// Emit the pipeline.verdicts metric + a policy.verdict span event.
		// outcome is mapped through DecisionFromOutcome so all six Outcome
		// values collapse to the coarse audit decision (allow/block/rewrite).
		// A downgraded verdict is tagged observed="true".
		observability.RecordPolicyVerdictObserved(ctx, policy.Name(),
			string(DecisionFromOutcome(verdict.Outcome)), "pre", verdict.Reason,
			enforcingVerdict || enforcingRewrite)

		// Merge audit fields regardless of outcome.
		for k, v := range verdict.AuditParams {
			result.AuditParams[k] = v
		}

		if enforcingRewrite {
			// Roll the body back to before this policy's rewrite so the
			// intended redaction is recorded but NOT applied.
			_ = mut.ReplaceBody(bodyBefore)
			result.Observed = append(result.Observed, ObservedVerdict{
				Policy: policy.Name(), Outcome: string(DecisionFromOutcome(verdict.Outcome)), Reason: verdict.Reason,
			})
			result.AuditParams["observed"] = true
		}

		// Refresh the wrapper's body view in case the policy queued a
		// ReplaceBody (eager apply means it's already on mut.body).
		result.BodyReplaced = mut.BodyReplaced()
		wrapper.body = mut.Body()

		switch verdict.Outcome {
		case OutcomeDeny:
			if enforcingVerdict {
				// Record the would-be deny and continue the chain as allow.
				result.Observed = append(result.Observed, ObservedVerdict{
					Policy: policy.Name(), Outcome: string(DecisionFromOutcome(verdict.Outcome)), Reason: verdict.Reason,
				})
				result.AuditParams["observed"] = true
				continue
			}
			result.DenyReason = verdict.Reason
			result.DeniedBy = policy.Name()
			result.FinalBody = mut.Body()
			return result, nil
		case OutcomeShortCircuit:
			if enforcingVerdict {
				// Record the would-be hold/short-circuit; no held state is
				// created and no synthetic response is returned.
				result.Observed = append(result.Observed, ObservedVerdict{
					Policy: policy.Name(), Outcome: string(DecisionFromOutcome(verdict.Outcome)), Reason: verdict.Reason,
				})
				continue
			}
			result.ShortCircuit = verdict.ShortCircuit
			result.FinalBody = mut.Body()
			return result, nil
		case OutcomeAllow, OutcomeSkip:
			// Continue chain.
		}
	}

	result.FinalBody = mut.Body()
	return result, nil
}

// bytesEqual reports byte equality without importing bytes for one call.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// mutatingRequestWrapper presents a ReadOnlyRequest whose RawBody()
// reflects the most-recent mutation. Other accessors delegate to the
// underlying base; only the body view shifts as the chain runs.
type mutatingRequestWrapper struct {
	base ReadOnlyRequest
	body []byte
}

func (w *mutatingRequestWrapper) Provider() conversation.Provider {
	return w.base.Provider()
}
func (w *mutatingRequestWrapper) StreamShape() conversation.StreamShape {
	return w.base.StreamShape()
}
func (w *mutatingRequestWrapper) Turns() []conversation.Turn { return w.base.Turns() }
func (w *mutatingRequestWrapper) HTTPRequest() *http.Request { return w.base.HTTPRequest() }
func (w *mutatingRequestWrapper) RawBody() []byte            { return append([]byte(nil), w.body...) }
func (w *mutatingRequestWrapper) IsFirstTurn() bool          { return w.base.IsFirstTurn() }
func (w *mutatingRequestWrapper) ConversationID() string     { return w.base.ConversationID() }
func (w *mutatingRequestWrapper) UserID() string             { return w.base.UserID() }
func (w *mutatingRequestWrapper) AgentID() string            { return w.base.AgentID() }
func (w *mutatingRequestWrapper) ValidateReplacementBody(body []byte) error {
	return w.base.ValidateReplacementBody(body)
}

var _ ReadOnlyRequest = (*mutatingRequestWrapper)(nil)
