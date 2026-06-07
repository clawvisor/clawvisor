package handlers

// llm_endpoint_pipeline.go houses the bridge between the LLMEndpointHandler
// and internal/runtime/llmproxy/pipeline. Preprocess policies still run
// at their handler-owned call sites so the handler can preserve the
// existing deny / short-circuit semantics around each step.

import (
	"context"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// pipelineReadOnlyRequest is the handler-side concrete ReadOnlyRequest
// implementation. The handler constructs one per request, populating
// the fields each migrated policy needs.
type pipelineReadOnlyRequest struct {
	provider       conversation.Provider
	streamShape    conversation.StreamShape
	httpReq        *http.Request
	body           []byte
	firstTurn      bool
	conversationID string
	userID         string
	agentID        string
}

func (r *pipelineReadOnlyRequest) Provider() conversation.Provider       { return r.provider }
func (r *pipelineReadOnlyRequest) StreamShape() conversation.StreamShape { return r.streamShape }
func (r *pipelineReadOnlyRequest) Turns() []conversation.Turn            { return nil }
func (r *pipelineReadOnlyRequest) HTTPRequest() *http.Request            { return r.httpReq }
func (r *pipelineReadOnlyRequest) RawBody() []byte                       { return append([]byte(nil), r.body...) }
func (r *pipelineReadOnlyRequest) IsFirstTurn() bool                     { return r.firstTurn }
func (r *pipelineReadOnlyRequest) ConversationID() string                { return r.conversationID }
func (r *pipelineReadOnlyRequest) UserID() string                        { return r.userID }
func (r *pipelineReadOnlyRequest) AgentID() string                       { return r.agentID }

var _ pipeline.ReadOnlyRequest = (*pipelineReadOnlyRequest)(nil)

// runSinglePolicy invokes Pipeline.RunPre with a single-policy chain.
// Used at each call site that's been migrated to the policy abstraction
// before the full chain consolidates.
//
// Returns the result. The caller threads result.FinalBody back into the
// handler's working body, merges result.AuditParams into auditParams,
// and handles result.DenyReason / result.ShortCircuit per its existing
// error semantics.
func runSinglePolicy(
	ctx context.Context,
	req *pipelineReadOnlyRequest,
	policy pipeline.RequestPolicy,
) (*pipeline.PreResult, error) {
	return pipeline.RunPre(ctx, req, []pipeline.RequestPolicy{policy})
}
