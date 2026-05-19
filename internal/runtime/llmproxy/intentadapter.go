package llmproxy

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/intent"
)

// intentVerifierAdapter wraps an intent.Verifier (the gateway's existing
// LLM-backed verifier) with the postprocess-local IntentVerifier interface.
// This avoids forcing the inspector path to import the LLM stack directly,
// while still reusing the gateway's prompts, caching, and provider config.
type intentVerifierAdapter struct {
	v intent.Verifier
}

// NewIntentVerifierAdapter bridges intent.Verifier into the lite-proxy's
// narrower IntentVerifier shape. Pass intent.NoopVerifier{} (or nil) when
// verification is disabled.
func NewIntentVerifierAdapter(v intent.Verifier) IntentVerifier {
	if v == nil {
		return nil
	}
	return &intentVerifierAdapter{v: v}
}

// Verify forwards to the underlying intent.Verifier and projects the
// returned VerificationVerdict onto the lite-proxy's narrower IntentVerdict.
func (a *intentVerifierAdapter) Verify(ctx context.Context, req IntentVerifyRequest) (*IntentVerdict, error) {
	verdict, err := a.v.Verify(ctx, intent.VerifyRequest{
		TaskPurpose: req.TaskPurpose,
		ExpectedUse: req.ExpectedUse,
		Service:     req.Service,
		Action:      req.Action,
		Params:      req.Params,
		Reason:      req.Reason,
		TaskID:      req.TaskID,
		Lenient:     req.Lenient,
		ProxyLite:   true,
	})
	if err != nil {
		return nil, err
	}
	if verdict == nil {
		return nil, nil
	}
	return &IntentVerdict{
		Allow:       verdict.Allow,
		Explanation: verdict.Explanation,
	}, nil
}
