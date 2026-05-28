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
	"github.com/clawvisor/clawvisor/internal/taskrisk"
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

// InlineTaskCreatorWithAssessment is an optional extension of
// InlineTaskCreator. When the auto-approve gate has already run the
// LLM risk assessor (and so already has a RiskAssessment in hand),
// callers can type-assert to this interface and pass the precomputed
// verdict, avoiding a second round-trip and the verdict drift it can
// cause. Implementations that don't (or can't) honor the precomputed
// value may simply call through to CreateInlineApprovedTask and
// compute a fresh assessment — the precomputed value is a hint, not
// a contract.
type InlineTaskCreatorWithAssessment interface {
	InlineTaskCreator
	CreateInlineApprovedTaskWithAssessment(
		ctx context.Context,
		agent *store.Agent,
		req *runtimetasks.TaskCreateRequest,
		originalToolUseID string,
		precomputed *taskrisk.RiskAssessment,
	) (*InlineApprovedTask, error)
}

// InlineTaskPendingCreator is the contract the inline-task intercept
// uses to land a pending Task row in the dashboard's Tasks surface BEFORE
// the user has replied in chat. The auto-approve gate continues to use
// CreateInlineApprovedTask (which produces an already-active task in
// one step); this interface is only for the human-prompt branch.
//
// Why split from InlineTaskCreator: the pending flow lives on a
// separate timeline from the active-creation flow. The intercept calls
// CreatePendingInlineTask once (returns the task id which lands in the
// cache hold); later, when the user replies, the chat-resolve path
// calls ApproveInlineTask or DenyInlineTask against that same task id.
// All three are typically implemented by the same handler, but a
// stripped-down test stub can implement just one. Returning the task
// ID (not the full Task) keeps the llmproxy package independent of
// store.Task's schema.
type InlineTaskPendingCreator interface {
	CreatePendingInlineTask(
		ctx context.Context,
		agent *store.Agent,
		req *runtimetasks.TaskCreateRequest,
		originalToolUseID string,
		precomputed *taskrisk.RiskAssessment,
	) (taskID string, err error)
	// ApproveInlineTask transitions a pending inline-chat task to
	// active and returns the InlineApprovedTask shape the chat path
	// renders to the LLM. Bypasses the dashboard CHAT_APPROVAL_REQUIRED
	// guard since the chat surface itself is doing the approval.
	ApproveInlineTask(ctx context.Context, taskID, userID string) (*InlineApprovedTask, error)
	// DenyInlineTask transitions a pending inline-chat task to
	// denied. Symmetric to ApproveInlineTask.
	DenyInlineTask(ctx context.Context, taskID, userID string) error
	// ExpireInlineTask transitions a pending inline-chat task to
	// expired. Distinct from Deny because the cause is operational
	// (the LRU cache evicted the hold so the chat anchor is gone),
	// not a user dismissal. Without this the dashboard would keep
	// showing the row as pending_approval with "reply in chat to
	// approve" guidance even though no chat reply can ever resolve
	// it. Idempotent on already-terminal rows.
	ExpireInlineTask(ctx context.Context, taskID, userID string) error
}

// ErrInlineTaskAlreadyTerminal is the typed error
// ApproveInlineTask returns when the chat-side "approve" reply
// arrives after the task has already been terminated through a
// non-chat surface (dashboard or notifier Deny, the 24h expiry
// sweep, manual revocation). It is intentionally a distinct shape
// from the generic "task is not pending" string so
// resolveInlineTaskApproval can detect this race specifically and
// render an explanatory reply directing the model to ask the user
// to create a fresh task, rather than surfacing a generic
// "approve failed" creator error to the LLM.
type ErrInlineTaskAlreadyTerminal struct {
	// Status is the actual terminal status the row landed at
	// ("denied" / "expired" / "revoked") so the rendered reply
	// can match the user's mental model of what happened.
	Status string
}

func (e *ErrInlineTaskAlreadyTerminal) Error() string {
	if e == nil {
		return "inline-chat task already terminal"
	}
	return "inline-chat task already terminal: " + e.Status
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
	HTTPRequest    *http.Request
	RequestID      string
	Provider       conversation.Provider
	Body           []byte
	Agent          *store.Agent
	ConversationID string

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
		ConversationID:  req.ConversationID,
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
			return ReleaseResult{Handled: true, HTTPStatus: http.StatusNotFound, Decision: "deny", Outcome: "approval_not_found", Reason: "this approval is no longer valid — it may have expired, already been used, or been issued to another agent. Please retry your original request to get a fresh approval prompt."}
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
			Reason:     "couldn't process this approval (internal: inline-task preprocess missing). Please retry your original request.",
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
		UserID:         req.Agent.UserID,
		AgentID:        req.Agent.ID,
		Provider:       req.Provider,
		ConversationID: req.ConversationID,
		ApprovalID:     peeked.ID,
	})
	if err != nil {
		return ReleaseResult{Handled: true, HTTPStatus: http.StatusServiceUnavailable, Decision: "deny", Outcome: "approval_release_error", Reason: err.Error()}
	}
	if pending == nil {
		// Peeked one moment ago but it's gone now — a concurrent
		// release/rewrite pass consumed it. Treat as not-found.
		if approvalID != "" {
			return ReleaseResult{Handled: true, HTTPStatus: http.StatusNotFound, Decision: "deny", Outcome: "approval_not_found", Reason: "this approval is no longer valid — it may have expired, already been used, or been issued to another agent. Please retry your original request to get a fresh approval prompt."}
		}
		return ReleaseResult{}
	}
	if verb == "deny" {
		req.logRelease(ctx, pending, "deny", "denied", "denied inline by user")
		return syntheticReleaseResultMulti(req, pending, false, nil, "deny", "approval_denied", "")
	}

	rewrittenCalls, releaseErr := rewriteApprovedToolUses(ctx, req, pending)
	if releaseErr != nil {
		req.logRelease(ctx, pending, "deny", "blocked", releaseErr.Error())
		return syntheticReleaseResultMulti(req, pending, false, nil, "deny", "approval_release_blocked", releaseErr.Error())
	}
	req.logRelease(ctx, pending, "allow", "released", "approved inline by user")
	return syntheticReleaseResultMulti(req, pending, true, rewrittenCalls, "allow", "approval_released", "")
}

// rewriteApprovedToolUses re-evaluates every held tool_use in the
// pending approval against current state and returns the synthesized
// call list in turn order. Fail-closed for the whole batch: if any
// single use refuses re-eval (decision changed, fingerprint diverged,
// boundary failed, nonce mint failed), the entire release is denied so
// we never half-execute a coalesced turn.
//
// For single-tool holds (no Additional entries) this collapses to the
// pre-coalescence behavior — one input map for one tool_use — and the
// returned slice has length 1.
func rewriteApprovedToolUses(ctx context.Context, req ReleaseRequest, pending *PendingLiteApproval) ([]conversation.SyntheticToolCall, error) {
	if pending == nil {
		return nil, errors.New("no pending approval")
	}
	holds := pending.AllHolds()
	out := make([]conversation.SyntheticToolCall, 0, len(holds))
	for _, held := range holds {
		input, err := rewriteApprovedHeldToolUse(ctx, req, held)
		if err != nil {
			return nil, err
		}
		out = append(out, conversation.SyntheticToolCall{
			ID:    held.ToolUse.ID,
			Name:  held.ToolUse.Name,
			Input: input,
		})
	}
	return out, nil
}

// rewriteApprovedHeldToolUse is the per-held-use re-evaluation that
// rewriteApprovedToolUses calls in a loop. Replays the inspector,
// decision, boundary, and (for credentialed paths) caller-nonce mint
// against the current agent / catalog / posture state. Any divergence
// from the originally captured fingerprint denies the release.
func rewriteApprovedHeldToolUse(ctx context.Context, req ReleaseRequest, held HeldToolUse) (map[string]any, error) {
	if req.Inspector == nil {
		return nil, errors.New("no inspector configured for release")
	}
	verdict := req.Inspector.Inspect(ctx, inspector.ToolUse{
		ID:    held.ToolUse.ID,
		Name:  held.ToolUse.Name,
		Input: held.ToolUse.Input,
	})
	if verdict.Source == inspector.SourceTriggerMiss {
		decisionInput := runtimedecision.AuthorizationInput{
			ToolUse:           held.ToolUse,
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
			if held.Kind != HeldKindApproval {
				// A coalesced sibling that was previously auto-allow now
				// requires approval. Fail-closed so the user isn't
				// silently bypassing a new policy decision via an
				// earlier yes that didn't promise this.
				return nil, errors.New("coalesced sibling now requires approval; re-prompt needed")
			}
			if !runtimedecision.EquivalentFingerprint(held.Fingerprint, runtimedecision.Fingerprint(dec, decisionInput)) {
				return nil, errors.New("held approval no longer matches current authorization decision")
			}
		}
		return decodeToolUseInput(held.ToolUse.Input), nil
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
		ToolUse:        held.ToolUse,
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
		if held.Kind != HeldKindApproval {
			return nil, errors.New("coalesced sibling now requires approval; re-prompt needed")
		}
		if !runtimedecision.EquivalentFingerprint(held.Fingerprint, runtimedecision.Fingerprint(dec, decisionInput)) {
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
	raw, err := inspector.Rewrite(inspector.ToolUse{ID: held.ToolUse.ID, Name: held.ToolUse.Name, Input: held.ToolUse.Input}, verdict, opts)
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

// syntheticReleaseResultMulti synthesizes the release response for a
// hold that may carry multiple tool_uses (the coalesced path). On
// allow, the response carries every approved tool_use in turn order so
// the harness executes them all from one user yes. On deny, the
// response is a single text block — calls is ignored. A single-tool
// hold (no Additional entries) collapses to a one-element synthesis
// that is byte-identical to the pre-coalescence shape.
func syntheticReleaseResultMulti(req ReleaseRequest, pending *PendingLiteApproval, allow bool, calls []conversation.SyntheticToolCall, decision, outcome, reason string) ReleaseResult {
	denyMessage := conversation.ApprovalDeniedMessage
	if !allow && outcome == "approval_release_blocked" && strings.TrimSpace(reason) != "" {
		// Branded so the user can tell the deny reason came from
		// Clawvisor (not the model or upstream provider). Mirrors the
		// "Clawvisor: ..." prefix added by writeLiteProxyError for
		// every other synthesized error.
		denyMessage = "Clawvisor: couldn't release this approval. " + strings.TrimSpace(reason)
	}
	synth, ok := conversation.SyntheticApprovalToolUsesResponseWithDenyMessage(req.HTTPRequest, req.Provider, req.Body, allow, calls, denyMessage)
	if !ok {
		return ReleaseResult{Handled: true, HTTPStatus: http.StatusBadRequest, Decision: "deny", Outcome: "approval_release_unsupported", Reason: "can't process this approval for this provider. Please retry your original request."}
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
