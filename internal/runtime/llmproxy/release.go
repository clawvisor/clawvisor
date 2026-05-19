package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

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
	ID               string                            `json:"task_id"`
	Status           string                            `json:"status"`
	Purpose          string                            `json:"purpose,omitempty"`
	Lifetime         string                            `json:"lifetime,omitempty"`
	ApprovalSource   string                            `json:"approval_source,omitempty"`
	ApprovalRecordID string                            `json:"approval_record_id,omitempty"`
	ExpiresAtRFC3339 string                            `json:"expires_at,omitempty"`
	Credentials      []InlineTaskCredentialPlaceholder `json:"credential_placeholders,omitempty"`
}

type InlineTaskCredentialPlaceholder struct {
	VaultItemID       string `json:"vault_item_id"`
	ServiceID         string `json:"service_id,omitempty"`
	Placeholder       string `json:"placeholder"`
	ExpiresAtRFC3339  string `json:"expires_at,omitempty"`
	CredentialGrantID string `json:"credential_grant_id,omitempty"`
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
	editor, ok := newApprovalBodyEditor(req.HTTPRequest, req.Provider, req.Body)
	if !ok {
		return ReleaseResult{}
	}
	verb, approvalID, ok := editor.LatestApprovalReply()
	if !ok || verb == "" || req.PendingApproval == nil || req.Agent == nil {
		return ReleaseResult{}
	}
	if verb == "task" {
		return ReleaseResult{}
	}
	action, err := resolveApprovalReplyAction(ctx, approvalReplyRoutingRequest{
		UserID:          req.Agent.UserID,
		AgentID:         req.Agent.ID,
		Provider:        req.Provider,
		PendingApproval: req.PendingApproval,
		Verb:            verb,
		ApprovalID:      approvalID,
	})
	if err != nil {
		return ReleaseResult{Handled: true, HTTPStatus: http.StatusServiceUnavailable, Decision: "deny", Outcome: "approval_release_error", Reason: err.Error()}
	}
	peeked := action.Hold
	if peeked == nil {
		if approvalID != "" {
			return ReleaseResult{Handled: true, HTTPStatus: http.StatusNotFound, Decision: "deny", Outcome: "approval_not_found", Reason: "no matching pending approval"}
		}
		return ReleaseResult{}
	}
	// Preprocess-missing defense: if the hold the bare reply targets
	// is itself an inline-task hold, the preprocess didn't fire. Fail
	// closed without consuming so a retry once preprocess is restored
	// can drive the inline flow. Same applies to an explicit-ID reply
	// naming an inline-task hold directly. Older inline holds sitting
	// behind a newer non-inline hold are NOT the user's target here
	// and stay in the cache untouched.
	if action.Kind == approvalReplyActionApproveInlineTask || action.Kind == approvalReplyActionDenyInlineTask {
		req.logRelease(ctx, peeked, "deny", "blocked", "inline-task hold reached release path; preprocess not wired")
		// 503 (Service Unavailable) reads more honestly than 500
		// here: the inline-approval preprocess is missing, the
		// feature isn't currently servable. The hold stays in the
		// cache; once preprocess is restored a retry drives the
		// flow. 500 would imply a runtime crash; this is a wiring
		// gap.
		return ReleaseResult{
			Handled:    true,
			HTTPStatus: http.StatusServiceUnavailable,
			Decision:   "deny",
			Outcome:    "inline_task_preprocess_missing",
			Reason:     "inline task hold reached release without preprocess rewrite",
		}
	}
	// Resolve the SAME hold we inspected, by explicit ID. Calling
	// Resolve with a bare (no-ID) request again would re-run the LIFO
	// selection at a fresh lock acquisition — a concurrent Hold
	// arriving between Peek and Resolve could surface a different
	// newest hold (potentially an inline-task one) and we'd consume
	// it under the tool-release path, destroying the inline flow.
	// Pinning to peeked.ID closes that TOCTOU window: if the peeked
	// hold has since been consumed, Resolve returns nil and we treat
	// as not-found rather than grab whatever else is newest.
	pending, err := req.PendingApproval.Resolve(ctx, ResolveRequest{
		UserID:     req.Agent.UserID,
		AgentID:    req.Agent.ID,
		Provider:   req.Provider,
		ApprovalID: peeked.ID,
	})
	if err != nil {
		return ReleaseResult{Handled: true, HTTPStatus: http.StatusServiceUnavailable, Decision: "deny", Outcome: "approval_release_error", Reason: err.Error()}
	}
	if pending == nil {
		// Peeked one moment ago but it's gone now — a concurrent
		// release/rewrite pass consumed it. Treat as not-found.
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
		if reason, ok := ValidateRuntimePlaceholderAccess(ctx, req.Store, rec, req.Agent.UserID, req.Agent.ID, time.Now().UTC()); !ok {
			return reason, false
		}
		hosts, boundReason := RuntimePlaceholderBoundHosts(ctx, req.Store, rec)
		if len(hosts) == 0 {
			return boundReason, false
		}
		if ok, reason := inspector.BoundaryCheck(v, hosts); !ok {
			return reason, false
		}
	}
	return "", true
}

func syntheticReleaseResult(req ReleaseRequest, pending *PendingLiteApproval, allow bool, toolInput map[string]any, decision, outcome, reason string) ReleaseResult {
	denyMessage := conversation.ApprovalDeniedMessage
	if !allow && outcome == "approval_release_blocked" && strings.TrimSpace(reason) != "" {
		denyMessage = "Approval could not be released. " + strings.TrimSpace(reason)
	}
	synth, ok := conversation.SyntheticApprovalToolUseResponseWithDenyMessage(req.HTTPRequest, req.Provider, req.Body, allow, pending.ToolUse.ID, pending.ToolUse.Name, toolInput, denyMessage)
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
