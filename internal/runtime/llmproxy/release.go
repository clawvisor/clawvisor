package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// InlineTaskCreator is the lite-proxy's contract for creating an
// inline-approved task. The handlers package implements this via the
// canonical TasksHandler.Create flow (sharing all validation), but
// runs in a single function call so the release path doesn't have to
// import handlers (which would cycle).
//
// On success the returned task is already active with an
// approval_records row marking surface="inline_chat" + resolution
// derived from lifetime ("standing"→allow_always, otherwise
// allow_session). On validation failure the error message is shown
// back to the user in the synthetic deny response so they can fix the
// request (or ask the agent to).
type InlineTaskCreator interface {
	CreateInlineApprovedTask(ctx context.Context, agent *store.Agent, req *runtimetasks.TaskCreateRequest, originalToolUseID string) (*InlineApprovedTask, error)
}

// InlineApprovedTask is the slice of the created task surfaced back
// through the synthetic release response. The fields here are what the
// model needs to see — it isn't a full store.Task because the LLM
// doesn't care about every column.
type InlineApprovedTask struct {
	ID                string `json:"task_id"`
	Status            string `json:"status"`
	Purpose           string `json:"purpose,omitempty"`
	Lifetime          string `json:"lifetime,omitempty"`
	ApprovalSource    string `json:"approval_source,omitempty"`
	ApprovalRecordID  string `json:"approval_record_id,omitempty"`
	ExpiresAtRFC3339  string `json:"expires_at,omitempty"`
}

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
	// CallerNonces mints the per-release nonce that replaces the agent's
	// bearer token in the released tool_use's X-Clawvisor-Caller header.
	// Same semantics as the inline path: bound to (agent, host, method,
	// path), one-shot, consumed by the resolver. Required when releasing
	// a credentialed tool_use; release fails closed if nil.
	CallerNonces CallerNonceCache

	// InlineTaskCreator is invoked when releasing an
	// awaiting_task_approval hold (the user typed "approve" on an inline
	// task definition prompt). Optional — when nil and a task hold is
	// resolved, release fails closed with a 503.
	InlineTaskCreator InlineTaskCreator
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
		return ReleaseResult{}
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
	// Inline task approval: resolved hold is the inner
	// StageAwaitingTaskApproval entry. Cascade to the original tool hold
	// so the user's single "approve" gesture both creates the task AND
	// re-emits the original tool_use for the harness to run.
	if pending.Stage == StageAwaitingTaskApproval {
		return releaseInlineTaskApproval(ctx, req, pending, verb)
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

// releaseInlineTaskApproval runs the cascade-release for the inner
// StageAwaitingTaskApproval hold. The user's "approve" creates the task
// pre-approved, drops the original tool hold from the cache (the model
// will re-emit the tool naturally and pass the new task-scope check on
// the next turn), and synthesizes a confirmation response. "deny" drops
// both holds and synthesizes a denial. Either way the response goes back
// to the harness as a normal turn — no multi-block bending of the
// existing single-block synthesized release path.
func releaseInlineTaskApproval(ctx context.Context, req ReleaseRequest, inner *PendingLiteApproval, verb string) ReleaseResult {
	// Drop the original tool hold from the cache regardless of outcome.
	// On approve, the model will re-emit the original tool on its next
	// turn and the new task-scope check will allow it (and run normal
	// credential rewrites for credentialed tools). On deny, the original
	// is moot — the model should give up on the tool entirely.
	if inner.AwaitingTaskFor != "" {
		_ = req.PendingApproval.Drop(ctx, ResolveRequest{
			UserID:     req.Agent.UserID,
			AgentID:    req.Agent.ID,
			Provider:   req.Provider,
			ApprovalID: inner.AwaitingTaskFor,
		})
	}

	if verb == "deny" {
		req.logRelease(ctx, inner, "deny", "denied", "inline task creation denied by user")
		return syntheticReleaseResult(req, inner, false, nil, "deny", "inline_task_denied", "user denied inline task")
	}

	// Approve path: create the task pre-approved.
	if req.InlineTaskCreator == nil {
		req.logRelease(ctx, inner, "deny", "blocked", "no inline task creator configured")
		return syntheticReleaseResult(req, inner, false, nil, "deny", "inline_task_creator_missing", "Clawvisor: inline task creation not available")
	}
	if inner.TaskDefinition == nil {
		req.logRelease(ctx, inner, "deny", "blocked", "no task definition on inner hold")
		return syntheticReleaseResult(req, inner, false, nil, "deny", "inline_task_definition_missing", "Clawvisor: missing task definition on approval")
	}
	originalToolUseID := ""
	if inner.AwaitingTaskFor != "" {
		originalToolUseID = inner.AwaitingTaskFor
	}
	created, err := req.InlineTaskCreator.CreateInlineApprovedTask(ctx, req.Agent, inner.TaskDefinition, originalToolUseID)
	if err != nil {
		req.logRelease(ctx, inner, "deny", "blocked", "inline task creation failed: "+err.Error())
		return syntheticReleaseResult(req, inner, false, nil, "deny", "inline_task_create_failed", "Clawvisor: inline task creation failed — "+err.Error())
	}

	// Synth an assistant tool_use_result-shaped response that surfaces
	// the created task. The model sees the same shape it would have
	// gotten from the real /control/tasks handler had it been called
	// directly, plus the task is already active so its next tool_use
	// passes the scope check.
	taskInput := inlineTaskResultPayload(created)
	syntheticToolName := inner.ToolUse.Name
	if syntheticToolName == "" {
		syntheticToolName = "Bash"
	}
	synth, ok := conversation.SyntheticApprovalToolUseResponse(req.HTTPRequest, req.Provider, req.Body, true, inner.ToolUse.ID, syntheticToolName, taskInput)
	if !ok {
		req.logRelease(ctx, inner, "allow", "released", "task created but response synthesis unsupported")
		return ReleaseResult{Handled: true, HTTPStatus: http.StatusBadRequest, Decision: "deny", Outcome: "inline_task_synthesis_unsupported", Reason: "unsupported approval release provider"}
	}
	req.logRelease(ctx, inner, "allow", "released", "inline task created and approved")
	if req.Audit != nil {
		req.Audit.LogInlineTaskApproved(ctx, req.Agent, req.RequestID, inner, created)
	}
	return ReleaseResult{
		Handled:     true,
		HTTPStatus:  http.StatusOK,
		Decision:    "allow",
		Outcome:     "inline_task_approved",
		Reason:      "",
		ContentType: synth.ContentType,
		Body:        synth.Body,
	}
}

// inlineTaskResultPayload builds the synthesized tool_use input the
// model sees in place of the never-executed POST /control/tasks. It
// mirrors the shape of the real handler's response so the model can
// proceed without provider-specific branching.
func inlineTaskResultPayload(t *InlineApprovedTask) map[string]any {
	if t == nil {
		return map[string]any{"status": "active"}
	}
	out := map[string]any{
		"task_id":         t.ID,
		"status":          t.Status,
		"approval_source": t.ApprovalSource,
	}
	if t.Purpose != "" {
		out["purpose"] = t.Purpose
	}
	if t.Lifetime != "" {
		out["lifetime"] = t.Lifetime
	}
	if t.ApprovalRecordID != "" {
		out["approval_record_id"] = t.ApprovalRecordID
	}
	if t.ExpiresAtRFC3339 != "" {
		out["expires_at"] = t.ExpiresAtRFC3339
	}
	out["message"] = "Task approved inline by user. Proceed with the originally requested tool call."
	return out
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
			IntentVerifier:    decisionIntentVerifier{inner: req.IntentVerifier},
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
		IntentVerifier: decisionIntentVerifier{inner: req.IntentVerifier},
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
	// Mint a fresh nonce at release time — the original hold predates
	// this release by minutes-to-hours, and any nonce that was minted at
	// hold time has long since expired. Bound to (agent, host, method,
	// path) so it only authorizes the specific call we're about to emit.
	if req.CallerNonces == nil {
		return nil, errors.New("caller nonce cache not configured; refusing to release with raw agent token")
	}
	nonce, mintErr := req.CallerNonces.Mint(ctx, req.Agent.ID, NonceTarget{
		Host:   verdict.Host,
		Method: verdict.Method,
		Path:   verdict.Path,
	})
	if mintErr != nil {
		return nil, errors.New("caller nonce mint failed: " + mintErr.Error())
	}
	opts := req.RewriteOpts
	opts.CallerToken = nonce
	raw, err := inspector.Rewrite(inspector.ToolUse{ID: pending.ToolUse.ID, Name: pending.ToolUse.Name, Input: pending.ToolUse.Input}, verdict, opts)
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
