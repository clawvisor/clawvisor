package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// AgentNoticeResponse prepends a human-visible routing notice
// ("[Clawvisor] Routing to <agent>...") to the assistant turn for
// first-turn requests. The notice text is computed during the
// request leg (today by the handler calling
// llmproxy.RenderAgentRoutingNotice) and threaded into this policy
// via its constructor — so the response-side policy stays purely
// about the prepend operation.
//
// Empty notice text → OutcomeSkip (no mutation). That's how the
// non-first-turn case is expressed; the handler simply constructs
// the policy with "" when it doesn't want a notice.
//
// This is the first migrated ResponsePolicy. Its proof point: the
// existing streamingResponseMutator (Phase 2) already does the
// per-shape prepend; this policy just commands it.
type AgentNoticeResponse struct {
	noticeText string
}

// NewAgentNoticeResponse constructs the policy with the rendered
// notice. Pass "" for no-op (non-first-turn).
func NewAgentNoticeResponse(noticeText string) *AgentNoticeResponse {
	return &AgentNoticeResponse{noticeText: noticeText}
}

// Name returns the audit-friendly policy identifier.
func (AgentNoticeResponse) Name() string { return "agent_notice_response" }

// Postprocess queues PrependAssistantText with the rendered notice,
// or Skips when the notice is empty.
func (p *AgentNoticeResponse) Postprocess(ctx context.Context, res pipeline.ReadOnlyResponse, mut pipeline.ResponseMutator) (pipeline.ResponseVerdict, error) {
	if p.noticeText == "" {
		return pipeline.ResponseVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	if err := mut.PrependAssistantText(p.noticeText); err != nil {
		return pipeline.ResponseVerdict{}, err
	}
	return pipeline.ResponseVerdict{
		Outcome: pipeline.OutcomeAllow,
		AuditFields: map[string]any{
			"agent_notice_prepended": true,
		},
	}, nil
}

var _ pipeline.ResponsePolicy = (*AgentNoticeResponse)(nil)
