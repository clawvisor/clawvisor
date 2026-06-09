package decision

import (
	"context"
	"sync"
)

// AuthorizationOutcome pairs an EvaluateAuthorization result with its
// error. EvaluateAuthorizationBatch returns one outcome per input, in
// the same order.
type AuthorizationOutcome struct {
	Decision AuthorizationDecision
	Err      error
}

// EvaluateAuthorizationBatch runs EvaluateAuthorization for each input
// concurrently. Each input's IntentVerifier round-trip — the expensive
// LLM call — overlaps with its siblings'; the rest of the evaluation
// (rule eval, task classification) is pure-function and safe to run
// concurrently on independent inputs.
//
// Errors are per-input — a failure on one input does not affect any
// other. The returned slice is index-aligned with inputs. Callers that
// need to react to errors should inspect each outcome.Err.
//
// Singleton batches degenerate to a direct EvaluateAuthorization call
// (no goroutine cost). An empty input slice returns an empty slice.
//
// The batch does not impose a concurrency cap. Verifier implementations
// that need rate limiting should enforce it themselves; that constraint
// is the verifier's, not the decision engine's.
func EvaluateAuthorizationBatch(ctx context.Context, inputs []AuthorizationInput) []AuthorizationOutcome {
	outcomes := make([]AuthorizationOutcome, len(inputs))
	if len(inputs) == 0 {
		return outcomes
	}
	if len(inputs) == 1 {
		outcomes[0].Decision, outcomes[0].Err = EvaluateAuthorization(ctx, inputs[0])
		return outcomes
	}
	var wg sync.WaitGroup
	wg.Add(len(inputs))
	for i := range inputs {
		i := i
		go func() {
			defer wg.Done()
			outcomes[i].Decision, outcomes[i].Err = EvaluateAuthorization(ctx, inputs[i])
		}()
	}
	wg.Wait()
	return outcomes
}
