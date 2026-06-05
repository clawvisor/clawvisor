package policies_test

import (
	"context"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/policies"
)

// BenchmarkPipelineOverhead_AnthropicSanitize measures the per-call
// cost of running anthropic_sanitize through Pipeline.RunPre vs.
// calling llmproxy.SanitizeAnthropicRequest directly.
//
// The plan budget is <10% overhead. If this benchmark shows a higher
// ratio, the pipeline allocation path needs optimization before more
// policies migrate.
//
// Body is a representative Anthropic request body — small enough that
// the policy dispatch overhead dominates over the actual sanitization
// work, which is what we want to measure.
func BenchmarkPipelineOverhead_AnthropicSanitize(b *testing.B) {
	body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	b.Run("legacy_inline_call", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _, _ = llmproxy.SanitizeAnthropicRequest(body)
		}
	})

	b.Run("pipeline_runpre", func(b *testing.B) {
		policy := policies.NewAnthropicSanitize()
		chain := []pipeline.RequestPolicy{policy}
		ctx := context.Background()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			req := &stubReadOnlyRequest{
				provider: conversation.ProviderAnthropic,
				rawBody:  body,
			}
			_, _ = pipeline.RunPre(ctx, req, chain)
		}
	})
}

// BenchmarkPipelineOverhead_NoMutationPath measures the path where
// no policy mutates the body — a benign clean request. This is the
// common case for production traffic; the pipeline overhead here
// must be negligible.
func BenchmarkPipelineOverhead_NoMutationPath(b *testing.B) {
	body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	policy := policies.NewAnthropicSanitize()
	chain := []pipeline.RequestPolicy{policy}
	ctx := context.Background()

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req := &stubReadOnlyRequest{
			provider: conversation.ProviderAnthropic,
			rawBody:  body,
		}
		_, _ = pipeline.RunPre(ctx, req, chain)
	}
}
