package pipeline

import (
	"context"
	"fmt"
	"io"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// PostResult is what Pipeline.RunPost returns after running every
// ResponsePolicy in declared order and committing queued mutations
// to the destination writer.
type PostResult struct {
	// AuditParams is the aggregated map of audit-row fields each policy
	// emitted. Last-writer-wins on key collision.
	AuditParams map[string]any
	// Verdicts is the per-policy verdict trail, in execution order.
	Verdicts []ResponsePolicyVerdict
}

// ResponsePolicyVerdict pairs a policy name with the ResponseVerdict
// it returned.
type ResponsePolicyVerdict struct {
	Name    string
	Verdict ResponseVerdict
}

// RunPost executes the ResponsePolicy chain in declared order against
// a streaming response. Mutations queue (via PrependAssistantText /
// SubstituteEntireResponse on the supplied mutator); the orchestrator
// calls Commit at the end to stream the transformed bytes to dst.
//
// For Phase 4 the post-phase doesn't yet support tool-use evaluators
// (Phase 5 lifts those in). One ResponseMutator is shared across all
// policies in the chain; if multiple policies queue mutations they
// MAY conflict at commit time (e.g., a second PrependAssistantText
// errors today since multiple calls aren't yet supported). Production
// chains will encode their order to avoid the conflict.
//
// dst is the client connection writer. src is the upstream response
// body reader. shape selects the per-shape codec.
func RunPost(
	ctx context.Context,
	res ReadOnlyResponse,
	dst io.Writer,
	src io.Reader,
	shape conversation.StreamShape,
	policies []ResponsePolicy,
) (*PostResult, error) {
	if res == nil {
		return nil, fmt.Errorf("pipeline.RunPost: nil response")
	}
	if dst == nil || src == nil {
		return nil, fmt.Errorf("pipeline.RunPost: dst and src required")
	}

	mut, err := NewStreamingResponseMutator(dst, src, shape)
	if err != nil {
		return nil, fmt.Errorf("pipeline.RunPost: build mutator: %w", err)
	}

	result := &PostResult{
		AuditParams: make(map[string]any),
		Verdicts:    make([]ResponsePolicyVerdict, 0, len(policies)),
	}

	for _, policy := range policies {
		verdict, err := policy.Postprocess(ctx, res, mut)
		if err != nil {
			return nil, fmt.Errorf("policy %q: %w", policy.Name(), err)
		}
		result.Verdicts = append(result.Verdicts, ResponsePolicyVerdict{Name: policy.Name(), Verdict: verdict})
		for k, v := range verdict.AuditParams {
			result.AuditParams[k] = v
		}

		// ResponsePolicies don't halt the chain via Deny / ShortCircuit
		// in the same way as RequestPolicies. Allow and Skip both
		// continue. Deny errors (postprocess Deny doesn't have a clear
		// semantic — the upstream already responded). The interface
		// allows Deny but the orchestrator refuses it.
		switch verdict.Outcome {
		case OutcomeAllow, OutcomeSkip:
			// continue
		default:
			return nil, fmt.Errorf("policy %q returned unsupported outcome %q for RunPost", policy.Name(), verdict.Outcome)
		}
	}

	// Commit: stream the transformed response to dst.
	committer, ok := mut.(interface{ Commit() error })
	if !ok {
		return nil, fmt.Errorf("pipeline.RunPost: mutator doesn't expose Commit")
	}
	if err := committer.Commit(); err != nil {
		return nil, fmt.Errorf("pipeline.RunPost: commit: %w", err)
	}

	return result, nil
}
