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
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/intentverify"
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

// InlineApprovedTaskExpirer is the rollback contract the auto-approve
// path uses when post-creation steps (pending-substitution registration)
// fail. Without this, a registration failure after
// CreateInlineApprovedTask succeeded would leave an orphan active task
// the user never saw the agent receive — wasted scope, confusing audit
// trail. Implementations transition active → expired so the orphan is
// visibly cleaned up in the dashboard. Idempotent on already-terminal
// rows.
type InlineApprovedTaskExpirer interface {
	ExpireInlineApprovedTask(ctx context.Context, taskID, userID string) error
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

// InlineExpansionCreator is the lite-proxy's contract for landing,
// approving, and denying an inline-chat scope expansion. Mirrors
// InlineTaskPendingCreator's shape (create pending row → wait for the
// chat verb → flip to active or back to active/expired/denied) but
// targets the existing pending-expansion state machine
// (SetTaskPendingExpansion → UpdateTaskEnvelopeFrom on approve,
// ResolveTaskPendingExpansion on deny). TasksHandler implements this;
// the inline-task creator field is reused via type assertion so the
// pipeline factory only has to wire one creator.
type InlineExpansionCreator interface {
	// CreatePendingInlineExpansion runs the same validation +
	// derived-action gates as the public Expand handler, lands the
	// pending state via SetTaskPendingExpansion (CAS on
	// active/expired), and stamps the canonical approval record's
	// Surface as "inline_chat". Returns the parent task id on success
	// so the caller can pin the cache hold to it.
	//
	// precomputedRisk, when non-nil, is the merged LLM+floor
	// assessment the intercept computed over the MERGED envelope. The
	// implementation persists it onto PendingTaskExpansion.RiskAssessment
	// so a dashboard-side approve (race against the chat anchor) also
	// reuses the LLM read instead of re-deriving deterministic-only.
	// nil is allowed; the cached field stays empty.
	CreatePendingInlineExpansion(
		ctx context.Context,
		agent *store.Agent,
		taskID string,
		additions *runtimetasks.Envelope,
		reason string,
		precomputedRisk *taskrisk.RiskAssessment,
	) (string, error)
	// ApproveInlineExpansion flips a pending_scope_expansion task to
	// active via UpdateTaskEnvelopeFrom (atomic merge + risk
	// reassessment + pending-snapshot guard). Bypasses any dashboard
	// chat-surface guard; the chat IS the surface. Returns the same
	// shape ApproveInlineTask does so the rewrite path can share the
	// approved-augmentation renderer.
	//
	// Approve callers that have a precomputed risk assessment from
	// the inline-expansion intercept should prefer
	// ApproveInlineExpansionWithAssessment (optional capability,
	// detected via type assertion) so the LLM read is reused on the
	// user's button click instead of falling back to the cached
	// PendingTaskExpansion value (which may be stale if the inline
	// intercept saw a fresher envelope than the headless Expand did).
	ApproveInlineExpansion(ctx context.Context, taskID, userID string) (*InlineApprovedExpansion, error)
	// DenyInlineExpansion routes a chat-side deny through
	// ResolveTaskPendingExpansion. Mirrors DenyInlineTask: the task
	// returns to active (or expired, mirroring the parent task's
	// deadline) — denying the EXPANSION, not the parent task.
	DenyInlineExpansion(ctx context.Context, taskID, userID string) error
	// ExpireInlineExpansion is called when the LRU cache evicts the
	// hold (the chat anchor is gone) and also on hold-creation /
	// record-write rollbacks from the intercept. Despite the
	// "Expire" name — kept for symmetry with ExpireInlineTask on the
	// task-creation interface — implementations MUST pick Active vs
	// Expired based on the PARENT task's actual deadline, NOT
	// unconditionally land Expired. The expansion attempt is what's
	// dropped; the parent task's prior status (typically active) must
	// be preserved on revert so a transient cache failure can't kill
	// a previously-healthy task. Idempotent on already-resolved rows.
	ExpireInlineExpansion(ctx context.Context, taskID, userID string) error
}

// InlineExpansionApproverWithAssessment is an optional extension of
// InlineExpansionCreator. Mirror of InlineTaskCreatorWithAssessment
// for the expansion flow: callers that have a precomputed merged
// LLM+floor risk assessment (from the inline expansion intercept's
// assessInlineExpansionRisk) can type-assert to this interface and
// pass the verdict through, so the approve path persists the same
// risk level the user saw in the chat prompt without re-running the
// assessor. Implementations that ignore the precomputed value may
// simply call through to ApproveInlineExpansion — the precomputed
// value is a hint, not a contract.
type InlineExpansionApproverWithAssessment interface {
	InlineExpansionCreator
	ApproveInlineExpansionWithAssessment(
		ctx context.Context,
		taskID, userID string,
		precomputed *taskrisk.RiskAssessment,
	) (*InlineApprovedExpansion, error)
}

// InlineApprovedExpansion is the slice of the expanded task surfaced
// back through the synthetic release response. Parallel shape to
// InlineApprovedTask so the renderer can share code. The task already
// exists, so we surface only what the model needs to know: the parent
// task id (so chat context remains anchored) and any credential
// placeholders the expansion minted.
type InlineApprovedExpansion struct {
	TaskID           string                            `json:"task_id"`
	Status           string                            `json:"status"`
	Purpose          string                            `json:"purpose,omitempty"`
	Lifetime         string                            `json:"lifetime,omitempty"`
	ApprovalRecordID string                            `json:"approval_record_id,omitempty"`
	ExpiresAtRFC3339 string                            `json:"expires_at,omitempty"`
	Credentials      []InlineTaskCredentialPlaceholder `json:"credential_placeholders,omitempty"`
	// CredentialMintFailed is set when the expansion's CAS landed
	// (envelope is broadened, status='active') but post-CAS
	// credential placeholder minting failed. The model sees the
	// success notice but ALSO a "credentials missing — ask the user
	// to retry" follow-up so it doesn't blindly proceed assuming
	// vault placeholders exist for the new required_credentials.
	// Distinct from a hard failure: the new tools / egress entries
	// in the envelope ARE usable; only the credential dimension is
	// incomplete.
	CredentialMintFailed bool `json:"credential_mint_failed,omitempty"`
}

// ErrInlineExpansionAlreadyTerminal is the typed error
// ApproveInlineExpansion returns when the chat-side "approve" reply
// arrives after the expansion has already been resolved from a
// non-chat surface (dashboard / notifier Approve or Deny, the
// pending-expansion expiry sweep). Same role as
// ErrInlineTaskAlreadyTerminal for the task-creation flow.
type ErrInlineExpansionAlreadyTerminal struct {
	// Status is the actual landing state ("active", "expired",
	// "denied", "revoked") so the rendered chat reply matches what
	// the user already saw on the other surface.
	Status string
}

func (e *ErrInlineExpansionAlreadyTerminal) Error() string {
	if e == nil {
		return "inline-chat expansion already terminal"
	}
	return "inline-chat expansion already terminal: " + e.Status
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
		return ReleaseResult{Handled: true, HTTPStatus: http.StatusServiceUnavailable, Decision: "deny", Outcome: "approval_release_error", Reason: "approval release unavailable; details are in the Clawvisor audit log."}
	}
	peeked := action.Hold
	if peeked == nil {
		// approvalID may be non-empty here because the bare-reply
		// parser scanned the assistant transcript for the latest
		// [clawvisor:approval=cv-xxx] marker and attached a stale one.
		// We can't distinguish that from a user explicitly typing
		// "yes cv-xxx", and the former is the overwhelmingly common
		// case (users type bare verbs; the marker just rides along).
		// Fall through silently so an ordinary "yes" to an agent's
		// own question isn't hijacked into a 404 once any earlier
		// Clawvisor approval has been resolved in this conversation.
		return ReleaseResult{}
	}
	// Preprocess-missing defense: if the hold the bare reply targets
	// is itself an inline-task hold, the preprocess didn't fire. Fail
	// closed without consuming so a retry once preprocess is restored
	// can drive the inline flow. Same applies to an explicit-ID reply
	// naming an inline-task hold directly. Older inline holds sitting
	// behind a newer non-inline hold are NOT the user's target here
	// and stay in the cache untouched.
	if action.Kind == approvalReplyActionApproveInlineTask || action.Kind == approvalReplyActionDenyInlineTask ||
		action.Kind == approvalReplyActionApproveInlineExpansion || action.Kind == approvalReplyActionDenyInlineExpansion {
		req.logRelease(ctx, peeked, "deny", "blocked", "inline-task/expansion hold reached release path; preprocess not wired")
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
		return ReleaseResult{Handled: true, HTTPStatus: http.StatusServiceUnavailable, Decision: "deny", Outcome: "approval_release_error", Reason: "approval release unavailable; details are in the Clawvisor audit log."}
	}
	if pending == nil {
		// Peeked one moment ago but it's gone now — a concurrent
		// release/rewrite pass consumed it. Fall through silently
		// for the same reason as the peeked==nil branch above: we
		// can't distinguish a user-typed cv-id from a stale marker
		// scraped out of transcript history, and silently dropping
		// is the safe behavior for bare yes/no replies.
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
			IntentVerifier:    intentverify.DecisionVerifierFor(req.IntentVerifier),
			PreferredTaskID:   held.Fingerprint.TaskID,
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
		ToolUse:         held.ToolUse,
		UserID:          req.Agent.UserID,
		AgentID:         req.Agent.ID,
		Posture:         req.Posture,
		Target:          runtimedecision.TargetRequest{Host: verdict.Host, Method: verdict.Method, Path: verdict.Path},
		Service:         resolved.ServiceID,
		Action:          resolved.ActionID,
		CandidateTasks:  req.CandidateTasks,
		ToolRules:       req.ToolRules,
		EgressRules:     req.EgressRules,
		IntentVerifier:  intentverify.DecisionVerifierFor(req.IntentVerifier),
		PreferredTaskID: held.Fingerprint.TaskID,
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
