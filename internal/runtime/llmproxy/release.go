package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type ReleaseRequest struct {
	HTTPRequest *http.Request
	RequestID   string
	Provider    conversation.Provider
	Body        []byte
	Agent       *store.Agent

	Inspector   *inspector.Inspector
	RewriteOpts inspector.RewriteOpts
	Store       store.Store
	Catalog     interface {
		Resolve(host, method, path string) (ResolvedAction, bool)
	}
	CandidateTasks  []*store.Task
	ToolRules       []*store.RuntimePolicyRule
	EgressRules     []*store.RuntimePolicyRule
	Posture         runtimedecision.EvaluationPosture
	IntentVerifier  IntentVerifier
	PendingApproval PendingApprovalCache
	Audit           *AuditEmitter
}

type ReleaseResult struct {
	Handled     bool
	HTTPStatus  int
	Decision    string
	Outcome     string
	Reason      string
	ContentType string
	Body        []byte
}

func TryReleasePendingApproval(ctx context.Context, req ReleaseRequest) ReleaseResult {
	verb, approvalID := conversation.ApprovalReplyForProvider(req.Provider, req.Body)
	if verb == "" || req.PendingApproval == nil || req.Agent == nil {
		return ReleaseResult{}
	}
	if verb == "task" {
		return handleTaskGuidanceReply(ctx, req, approvalID)
	}
	pending, err := req.PendingApproval.Resolve(ctx, ResolveRequest{
		UserID:     req.Agent.UserID,
		AgentID:    req.Agent.ID,
		Provider:   req.Provider,
		ApprovalID: approvalID,
	})
	if err != nil {
		return ReleaseResult{Handled: true, HTTPStatus: http.StatusServiceUnavailable, Decision: "deny", Outcome: "approval_release_error", Reason: err.Error()}
	}
	if pending == nil {
		if approvalID != "" {
			return ReleaseResult{Handled: true, HTTPStatus: http.StatusNotFound, Decision: "deny", Outcome: "approval_not_found", Reason: "no matching pending approval"}
		}
		return ReleaseResult{}
	}
	if verb == "deny" {
		req.logRelease(ctx, pending, "deny", "denied", "denied inline by user")
		return syntheticReleaseResult(req, pending, false, nil, "deny", "approval_denied", "")
	}

	rewrittenInput, releaseErr := rewriteApprovedToolUse(ctx, req, pending)
	if releaseErr != nil {
		req.logRelease(ctx, pending, "deny", "blocked", releaseErr.Error())
		return syntheticReleaseResult(req, pending, false, nil, "deny", "approval_release_blocked", releaseErr.Error())
	}
	req.logRelease(ctx, pending, "allow", "released", "approved inline by user")
	return syntheticReleaseResult(req, pending, true, rewrittenInput, "allow", "approval_released", "")
}

func handleTaskGuidanceReply(ctx context.Context, req ReleaseRequest, approvalID string) ReleaseResult {
	pending, err := req.PendingApproval.Peek(ctx, ResolveRequest{
		UserID:     req.Agent.UserID,
		AgentID:    req.Agent.ID,
		Provider:   req.Provider,
		ApprovalID: approvalID,
	})
	if err != nil {
		return ReleaseResult{Handled: true, HTTPStatus: http.StatusServiceUnavailable, Decision: "deny", Outcome: "approval_task_error", Reason: err.Error()}
	}
	if pending == nil {
		if approvalID != "" {
			return ReleaseResult{Handled: true, HTTPStatus: http.StatusNotFound, Decision: "deny", Outcome: "approval_not_found", Reason: "no matching pending approval"}
		}
		return ReleaseResult{}
	}
	synth, ok := conversation.SyntheticApprovalTextResponse(req.HTTPRequest, req.Provider, req.Body, taskCreationPrompt(pending.ToolUse))
	if !ok {
		return ReleaseResult{Handled: true, HTTPStatus: http.StatusBadRequest, Decision: "deny", Outcome: "approval_task_unsupported", Reason: "unsupported task guidance provider"}
	}
	return ReleaseResult{
		Handled:     true,
		HTTPStatus:  http.StatusOK,
		Decision:    "allow",
		Outcome:     "approval_task_guidance",
		ContentType: synth.ContentType,
		Body:        synth.Body,
	}
}

func rewriteApprovedToolUse(ctx context.Context, req ReleaseRequest, pending *PendingLiteApproval) (map[string]any, error) {
	if req.Inspector == nil || pending == nil {
		return nil, errors.New("no pending approval")
	}
	verdict := req.Inspector.Inspect(ctx, inspector.ToolUse{
		ID:    pending.ToolUse.ID,
		Name:  pending.ToolUse.Name,
		Input: pending.ToolUse.Input,
	})
	if verdict.Source == inspector.SourceTriggerMiss {
		decisionInput := runtimedecision.AuthorizationInput{
			ToolUse:           pending.ToolUse,
			UserID:            req.Agent.UserID,
			AgentID:           req.Agent.ID,
			Posture:           req.Posture,
			CandidateTasks:    req.CandidateTasks,
			ToolRules:         req.ToolRules,
			EgressRules:       req.EgressRules,
			IntentVerifier:    releaseDecisionIntentVerifier{inner: req.IntentVerifier},
			AllowMissingScope: true,
		}
		dec, err := runtimedecision.EvaluateAuthorization(ctx, decisionInput)
		if err != nil {
			return nil, err
		}
		switch dec.Kind {
		case runtimedecision.VerdictDeny:
			return nil, errors.New(dec.Reason)
		case runtimedecision.VerdictNeedsApproval:
			if !runtimedecision.EquivalentFingerprint(pending.Fingerprint, runtimedecision.Fingerprint(dec, decisionInput)) {
				return nil, errors.New("held approval no longer matches current authorization decision")
			}
		}
		return decodeToolUseInput(pending.ToolUse.Input), nil
	}
	if verdict.Ambiguous || !verdict.IsAPICall {
		return nil, errors.New("held tool use no longer resolves to a credentialed API call")
	}
	if reason, ok := boundaryCheckReleaseVerdict(ctx, req, verdict); !ok {
		return nil, errors.New(reason)
	}
	resolved := ResolvedAction{}
	if req.Catalog != nil {
		resolved, _ = req.Catalog.Resolve(verdict.Host, verdict.Method, verdict.Path)
	}
	decisionInput := runtimedecision.AuthorizationInput{
		ToolUse:        pending.ToolUse,
		UserID:         req.Agent.UserID,
		AgentID:        req.Agent.ID,
		Posture:        req.Posture,
		Target:         runtimedecision.TargetRequest{Host: verdict.Host, Method: verdict.Method, Path: verdict.Path},
		Service:        resolved.ServiceID,
		Action:         resolved.ActionID,
		CandidateTasks: req.CandidateTasks,
		ToolRules:      req.ToolRules,
		EgressRules:    req.EgressRules,
		IntentVerifier: releaseDecisionIntentVerifier{inner: req.IntentVerifier},
	}
	dec, err := runtimedecision.EvaluateAuthorization(ctx, decisionInput)
	if err != nil {
		return nil, err
	}
	switch dec.Kind {
	case runtimedecision.VerdictDeny:
		return nil, errors.New(dec.Reason)
	case runtimedecision.VerdictNeedsApproval:
		if !runtimedecision.EquivalentFingerprint(pending.Fingerprint, runtimedecision.Fingerprint(dec, decisionInput)) {
			return nil, errors.New("held approval no longer matches current authorization decision")
		}
	}
	raw, err := inspector.Rewrite(inspector.ToolUse{ID: pending.ToolUse.ID, Name: pending.ToolUse.Name, Input: pending.ToolUse.Input}, verdict, req.RewriteOpts)
	if err != nil {
		return nil, err
	}
	var input map[string]any
	_ = json.Unmarshal(raw, &input)
	return input, nil
}

func decodeToolUseInput(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil
	}
	return input
}

func boundaryCheckReleaseVerdict(ctx context.Context, req ReleaseRequest, v inspector.Verdict) (string, bool) {
	if req.Store == nil {
		return "no store configured for boundary check", false
	}
	if req.Agent == nil {
		return "no agent context for boundary check", false
	}
	if len(v.Placeholders) == 0 {
		return "verdict missing placeholder for boundary lookup", false
	}
	for _, ph := range v.Placeholders {
		rec, err := req.Store.GetRuntimePlaceholder(ctx, ph)
		if err != nil {
			return "placeholder lookup failed", false
		}
		if rec.UserID != req.Agent.UserID || rec.AgentID != req.Agent.ID {
			return "placeholder owned by another agent", false
		}
		if ok, reason := inspector.BoundaryCheck(v, inspector.BoundServiceHosts(rec.ServiceID)); !ok {
			return reason, false
		}
	}
	return "", true
}

type releaseDecisionIntentVerifier struct {
	inner IntentVerifier
}

func (v releaseDecisionIntentVerifier) Verify(ctx context.Context, req runtimedecision.IntentVerifyRequest) (*runtimedecision.IntentVerdict, error) {
	if v.inner == nil {
		return nil, nil
	}
	verdict, err := v.inner.Verify(ctx, IntentVerifyRequest{
		TaskPurpose: req.TaskPurpose,
		ExpectedUse: req.ExpectedUse,
		Service:     req.Service,
		Action:      req.Action,
		Params:      req.Params,
		Reason:      req.Reason,
		TaskID:      req.TaskID,
		Lenient:     req.Lenient,
	})
	if err != nil || verdict == nil {
		return nil, err
	}
	return &runtimedecision.IntentVerdict{Allow: verdict.Allow, Explanation: verdict.Explanation}, nil
}

func syntheticReleaseResult(req ReleaseRequest, pending *PendingLiteApproval, allow bool, toolInput map[string]any, decision, outcome, reason string) ReleaseResult {
	synth, ok := conversation.SyntheticApprovalToolUseResponse(req.HTTPRequest, req.Provider, req.Body, allow, pending.ToolUse.ID, pending.ToolUse.Name, toolInput)
	if !ok {
		return ReleaseResult{Handled: true, HTTPStatus: http.StatusBadRequest, Decision: "deny", Outcome: "approval_release_unsupported", Reason: "unsupported approval release provider"}
	}
	return ReleaseResult{
		Handled:     true,
		HTTPStatus:  http.StatusOK,
		Decision:    decision,
		Outcome:     outcome,
		Reason:      reason,
		ContentType: synth.ContentType,
		Body:        synth.Body,
	}
}

func (r ReleaseRequest) logRelease(ctx context.Context, pending *PendingLiteApproval, decision, outcome, reason string) {
	if r.Audit != nil {
		r.Audit.LogApprovalRelease(ctx, r.Agent, r.RequestID, pending, decision, outcome, reason)
	}
}
