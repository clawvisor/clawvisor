package policies

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// ApprovalRelease handles the "release a pending approval" preprocess
// step. When a user reply (yes/no/approve/deny) matches a pending
// hold AND the underlying tool_use can be released, this policy
// short-circuits the request — the synthesized response (carrying the
// tool_use result or refusal) goes back to the harness directly,
// without forwarding to the upstream model.
//
// The release call has many dependencies (Catalog, Inspector, Store,
// IntentVerifier, AuditEmitter, CallerNonces, candidate tasks + tool
// rules + egress rules) — the policy takes a single closure that the
// handler constructs per-request with all of them baked in. This
// keeps the policy decoupled from the Store and inspector internals
// while still using the existing TryReleasePendingApproval helper.
//
// Outcomes:
//   - resolver returns Handled=false → Allow with no mutation (this
//     reply isn't a release; downstream policies handle it).
//   - resolver returns Handled=true → ShortCircuit with the
//     synthesized response body; preprocess halts, forwarder skipped.
type ApprovalRelease struct {
	resolver ApprovalReleaseResolver
}

// ApprovalReleaseResolver is the handler-supplied closure that runs
// the release attempt. Returns Handled=false when the reply isn't a
// release attempt; Handled=true with the synthesized body + content
// type when a release fires.
type ApprovalReleaseResolver func(ctx context.Context) llmproxy.ReleaseResult

// NewApprovalRelease constructs the policy. nil resolver → Skip.
func NewApprovalRelease(resolver ApprovalReleaseResolver) *ApprovalRelease {
	return &ApprovalRelease{resolver: resolver}
}

// Name returns the audit-friendly identifier.
func (ApprovalRelease) Name() string { return "approval_release" }

// Preprocess invokes the release resolver. On Handled=true the policy
// emits ShortCircuit with the synthesized response — the orchestrator
// skips the rest of the preprocess chain AND the forwarder.
func (p *ApprovalRelease) Preprocess(ctx context.Context, _ pipeline.ReadOnlyRequest, _ pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	if p.resolver == nil {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeSkip}, nil
	}
	result := p.resolver(ctx)
	if !result.Handled {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow}, nil
	}

	contentType := result.ContentType
	if contentType == "" {
		contentType = "application/json"
	}
	status := result.HTTPStatus
	if status == 0 {
		status = 200
	}

	fields := map[string]any{
		"approval_release_handled": true,
	}
	if result.Decision != "" {
		fields["approval_release_decision"] = result.Decision
	}
	if result.Outcome != "" {
		fields["approval_release_outcome"] = result.Outcome
	}
	if result.Reason != "" {
		fields["approval_release_reason"] = result.Reason
	}

	return pipeline.RequestVerdict{
		Outcome:     pipeline.OutcomeShortCircuit,
		AuditParams: fields,
		ShortCircuit: &pipeline.SyntheticResponse{
			Body:       result.Body,
			StatusCode: status,
			Headers:    map[string]string{"Content-Type": contentType},
		},
	}, nil
}

var _ pipeline.RequestPolicy = (*ApprovalRelease)(nil)
