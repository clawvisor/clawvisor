package pipeline

import (
	"context"
	"fmt"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// PreResult is what Pipeline.RunPre returns after running every
// RequestPolicy in declared order.
type PreResult struct {
	// FinalBody is the request body after all queued mutations have
	// been applied. The handler forwards this to the upstream.
	FinalBody []byte

	// AuditFields is the aggregated map of audit-row fields each policy
	// emitted. Conflicts go to the last writer (orderly via declared
	// chain order). The handler merges this with its existing auditParams.
	AuditFields map[string]any

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

	mut := newEagerRequestMutator(req.RawBody())
	result := &PreResult{
		AuditFields: make(map[string]any),
		Verdicts:    make([]PolicyVerdict, 0, len(policies)),
	}

	// Wrap req so each policy sees the *current* body (post any
	// preceding ReplaceBody). The orchestrator updates the wrapper as
	// mutations land.
	wrapper := &mutatingRequestWrapper{base: req, body: mut.Body()}

	for _, policy := range policies {
		verdict, err := policy.Preprocess(ctx, wrapper, mut)
		if err != nil {
			return nil, fmt.Errorf("policy %q: %w", policy.Name(), err)
		}
		result.Verdicts = append(result.Verdicts, PolicyVerdict{Name: policy.Name(), Verdict: verdict})

		// Merge audit fields regardless of outcome.
		for k, v := range verdict.AuditFields {
			result.AuditFields[k] = v
		}

		// Refresh the wrapper's body view in case the policy queued a
		// ReplaceBody (eager apply means it's already on mut.body).
		wrapper.body = mut.Body()

		switch verdict.Outcome {
		case OutcomeDeny:
			result.DenyReason = verdict.Reason
			result.DeniedBy = policy.Name()
			result.FinalBody = mut.Body()
			return result, nil
		case OutcomeShortCircuit:
			if verdict.ShortCircuit == nil {
				return nil, fmt.Errorf("policy %q returned ShortCircuit outcome with nil ShortCircuit payload", policy.Name())
			}
			if result.ShortCircuit != nil {
				return nil, fmt.Errorf("policy %q is the second to ShortCircuit; only one allowed per request", policy.Name())
			}
			result.ShortCircuit = verdict.ShortCircuit
			result.FinalBody = mut.Body()
			return result, nil
		case OutcomeAllow, OutcomeSkip:
			// Continue chain.
		default:
			return nil, fmt.Errorf("policy %q returned unsupported outcome %q for RunPre", policy.Name(), verdict.Outcome)
		}
	}

	result.FinalBody = mut.Body()
	return result, nil
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
func (w *mutatingRequestWrapper) RawBody() []byte            { return w.body }
func (w *mutatingRequestWrapper) IsFirstTurn() bool          { return w.base.IsFirstTurn() }
func (w *mutatingRequestWrapper) ConversationID() string     { return w.base.ConversationID() }
func (w *mutatingRequestWrapper) UserID() string             { return w.base.UserID() }
func (w *mutatingRequestWrapper) AgentID() string            { return w.base.AgentID() }

var _ ReadOnlyRequest = (*mutatingRequestWrapper)(nil)
