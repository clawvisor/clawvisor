package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// End-to-end tests covering each agent decision path the scope-drift
// menu exposes:
//
//   - (a) Expand the active task   → POST .../expand?surface=inline
//   - (b) Create a new task        → POST .../tasks?surface=inline
//   - (c) One-off                  → <clawvisor:decision option="one-off">
//   - (implicit) Do something else → no markup, drift TTL-expires
//
// Plus the registry's guards: one-shot ClaimOption cap, cross-
// conversation refusal, and pre-clear single-consumption semantics.
//
// Each test walks an end-to-end agent flow:
//   1. mint a drift (via the registry, mirroring what the credentialed
//      resolver does at block time)
//   2. simulate the agent's decision (the intercept call OR the markup
//      in the assistant body)
//   3. assert the hold landed at the correct stage with the drift link
//   4. simulate the user's yes/no on the resulting approval prompt
//   5. assert the registry's terminal state AND the pre-clear's
//      availability/non-availability

const (
	driftTestAgentID  = "agent-drift-1"
	driftTestUserID   = "user-drift-1"
	driftTestConvID   = "conv-drift-1"
	driftTestService  = "github"
	driftTestAction   = "post_issue"
	driftTestHost     = "api.github.com"
	driftTestMethod   = "POST"
	driftTestPath     = "/repos/o/r/issues"
	driftTestToolName = "Bash"
)

// mintDriftFixture seeds a ScopeDrift in the registry that mirrors the
// state the credentialed resolver writes at block time. Returns the
// stored record + the original (blocked) tool_use so callers can
// reconstruct the agent's retry.
func mintDriftFixture(t *testing.T, reg ScopeDriftRegistry, source ScopeDriftSource) (ScopeDrift, conversation.ToolUse) {
	t.Helper()
	tu := conversation.ToolUse{
		ID:    "tu-blocked",
		Name:  driftTestToolName,
		Input: json.RawMessage(`{"command":"curl -X POST https://api.github.com/repos/o/r/issues -d '{\"title\":\"hi\"}'"}`),
	}
	stored, err := reg.Register(context.Background(), ScopeDrift{
		UserID:         driftTestUserID,
		AgentID:        driftTestAgentID,
		ConversationID: driftTestConvID,
		Provider:       conversation.ProviderAnthropic,
		ToolUse:        tu,
		Service:        driftTestService,
		Action:         driftTestAction,
		Host:           driftTestHost,
		Method:         driftTestMethod,
		Path:           driftTestPath,
		Source:         source,
		ReasonText:     "no active task scope covers github.post_issue",
	})
	if err != nil {
		t.Fatalf("seed drift: %v", err)
	}
	return stored, tu
}

// anthropicReplyBody returns an Anthropic /v1/messages request body
// whose latest user message is the verb (yes/no) plus the approval ID.
// Used to drive the user-approval rewriters.
func anthropicReplyBody(verb, approvalID string) []byte {
	return []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"` + verb + ` ` + approvalID + `"}]}]}`)
}

// invokeOneOffIntercept calls MaybeInterceptScopeDriftOneOff with a
// synthetic tool_use that POSTs to the new one-off endpoint. Returns
// (verdict, claimed) like the real intercept. driftID is rendered into
// the path; rationale into the body.
func invokeOneOffIntercept(t *testing.T, reg ScopeDriftRegistry, cache PendingApprovalCache, agentID, convID, driftID, rationale string) (conversation.ToolUseVerdict, bool) {
	t.Helper()
	cfg := PostprocessConfig{
		AgentContext: AgentContext{AgentID: agentID, AgentUserID: driftTestUserID},
		AuditContext: AuditContext{ConversationID: convID},
		AuthorizationContext: AuthorizationContext{
			ScopeDrifts: reg,
		},
		ApprovalContext: ApprovalContext{PendingApprovals: cache},
	}
	httpReq := httptest.NewRequest("POST", "http://daemon/api/control/scope-drifts/"+driftID+"/one-off?surface=inline", nil)
	call := ControlCall{Method: "POST", URL: httpReq.URL}
	body := map[string]any{"rationale": rationale}
	bodyJSON, _ := json.Marshal(body)
	tu := conversation.ToolUse{
		ID:    "tu-one-off",
		Name:  "Bash",
		Input: json.RawMessage(`{"body":` + string(mustJSON(string(bodyJSON))) + `}`),
	}
	return MaybeInterceptScopeDriftOneOff(httpReq, cfg, func(string, string, string) {}, func(string, ...any) {}, conversation.ProviderAnthropic, tu, call)
}

// ── (c) One-off: approve path ───────────────────────────────────────────────────

// TestScopeDriftE2E_OneOffApprove walks the POST → user-approve →
// pre-clear path:
//
//  1. agent POSTs /api/control/scope-drifts/<id>/one-off?surface=inline
//     with a one-line rationale
//  2. MaybeInterceptScopeDriftOneOff claims the option + opens a
//     StageAwaitingScopeDriftOneOff hold + substitutes the tool_result
//     with the user-facing approval prompt
//  3. user replies "yes <approval_id>"
//  4. RewriteScopeDriftOneOffApprovalReply resolves the hold + sets
//     the drift outcome to Succeeded + mints the pre-clear
//  5. agent's retry of the original tool_use consumes the pre-clear once
func TestScopeDriftE2E_OneOffApprove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0)
	cache := NewMemoryPendingApprovalCache(time.Minute)

	drift, blockedTU := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope)

	// Step 1 + 2: agent POSTs the one-off; intercept claims and opens hold.
	verdict, claimed := invokeOneOffIntercept(t, reg, cache, driftTestAgentID, driftTestConvID, drift.ID, "need this single call to file the issue.")
	if !claimed {
		t.Fatal("expected MaybeInterceptScopeDriftOneOff to claim the POST")
	}
	if verdict.Allowed {
		t.Fatalf("verdict should be a held block (Allowed=false); got %+v", verdict)
	}
	if !strings.Contains(verdict.SubstituteWith, "Reply `yes` or `y`") {
		t.Fatalf("expected user-facing approval prompt in SubstituteWith: %s", verdict.SubstituteWith)
	}

	// Step 3: the intercept should have opened exactly one hold at the
	// scope-drift one-off stage.
	holds := peekAllHolds(ctx, cache)
	if len(holds) != 1 {
		t.Fatalf("want 1 hold after intercept, got %d", len(holds))
	}
	if holds[0].Stage != StageAwaitingScopeDriftOneOff {
		t.Fatalf("hold stage = %q, want %q", holds[0].Stage, StageAwaitingScopeDriftOneOff)
	}
	if holds[0].ScopeDriftID != drift.ID {
		t.Fatalf("hold ScopeDriftID = %q, want %q", holds[0].ScopeDriftID, drift.ID)
	}
	approvalID := holds[0].ID

	// Confirm the registry recorded the claim.
	got, _ := reg.Get(ctx, drift.ID)
	if got.ChosenOption != ScopeDriftOptionOneOff || got.Outcome != ScopeDriftOutcomePending {
		t.Fatalf("registry state after claim: %+v", got)
	}
	_ = claimed

	// Step 4: user types "yes <approval-id>". The reply rewriter
	// resolves the hold and flips the drift to Succeeded.
	replyBody := anthropicReplyBody("yes", approvalID)
	result, err := RewriteScopeDriftOneOffApprovalReply(ctx, ScopeDriftReplyRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            replyBody,
		Agent:           &store.Agent{ID: driftTestAgentID, UserID: driftTestUserID},
		ConversationID:  driftTestConvID,
		PendingApproval: cache,
		ScopeDrifts:     reg,
		Logger:          slog.Default(),
	})
	if err != nil {
		t.Fatalf("RewriteScopeDriftOneOffApprovalReply: %v", err)
	}
	if !result.Rewritten || result.Decision != "allow" || result.DriftID != drift.ID {
		t.Fatalf("approve result: %+v", result)
	}

	final, _ := reg.Get(ctx, drift.ID)
	if final.Outcome != ScopeDriftOutcomeSucceeded {
		t.Fatalf("registry outcome = %q, want %q", final.Outcome, ScopeDriftOutcomeSucceeded)
	}

	// Step 5: the agent retries the original tool_use. Build the
	// fingerprint from the SAME (agent, conv, service, action, host,
	// method, path, input) tuple the credentialed resolver uses.
	fp := ScopeDrift{
		AgentID:        driftTestAgentID,
		ConversationID: driftTestConvID,
		ToolUse:        blockedTU,
		Service:        driftTestService,
		Action:         driftTestAction,
		Host:           driftTestHost,
		Method:         driftTestMethod,
		Path:           driftTestPath,
	}.Fingerprint()
	gotDrift, hit := reg.LookupPreClear(ctx, driftTestAgentID, fp)
	if !hit || gotDrift != drift.ID {
		t.Fatalf("LookupPreClear first call: hit=%v id=%q, want hit=true id=%q", hit, gotDrift, drift.ID)
	}
	// One-shot: second consume MUST miss.
	if _, hit := reg.LookupPreClear(ctx, driftTestAgentID, fp); hit {
		t.Fatal("LookupPreClear second call: want miss (consumed), got hit")
	}
}

// ── Trigger-miss pre-clear consumption (cubic violation #2 regression) ─────────

// TestScopeDriftE2E_TriggerMissPreClearConsumed pins the lifecycle for
// non-credentialed (Bash/Edit/Read) drifts: an approved one-off mints
// a pre-clear under the trigger-miss fingerprint (no service/action),
// and the agent's retry must consume it via
// ConsumePreClearForTriggerMiss. Without this hook the retry would
// hit AuthorizationPolicy → VerdictNeedsApproval → mint another drift
// → infinite menu loop.
func TestScopeDriftE2E_TriggerMissPreClearConsumed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0)

	// Mint a trigger-miss-shaped drift directly: no Service / Action,
	// just host/method/path/input. Mirrors what
	// scopeDriftCoordinator.MintForTriggerMiss writes at block time.
	tu := conversation.ToolUse{
		ID:    "tu-bash",
		Name:  "Bash",
		Input: json.RawMessage(`{"command":"rm -rf /tmp/scratch"}`),
	}
	stored, err := reg.Register(ctx, ScopeDrift{
		UserID:         driftTestUserID,
		AgentID:        driftTestAgentID,
		ConversationID: driftTestConvID,
		Provider:       conversation.ProviderAnthropic,
		ToolUse:        tu,
		Host:           "",
		Method:         "",
		Path:           "",
		Source:         ScopeDriftSourceTaskScope,
		ReasonText:     "no covering task",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// User approves the one-off → SetOutcome(Succeeded) mints the
	// pre-clear keyed by the SAME (no-service-no-action) fingerprint
	// that ConsumePreClearForTriggerMiss will reconstruct.
	if err := reg.SetOutcome(ctx, stored.ID, ScopeDriftOutcomeSucceeded); err != nil {
		t.Fatalf("SetOutcome: %v", err)
	}

	// The trigger-miss fingerprint must match what
	// ConsumePreClearForTriggerMiss reconstructs from the same tu +
	// inspector verdict shape (no service, no action — the inspector
	// classifies Bash as trigger_miss).
	fp := ScopeDrift{
		AgentID:        driftTestAgentID,
		ConversationID: driftTestConvID,
		ToolUse:        tu,
	}.Fingerprint()
	driftID, hit := reg.LookupPreClear(ctx, driftTestAgentID, fp)
	if !hit {
		t.Fatal("trigger-miss pre-clear lookup must hit after Succeeded outcome")
	}
	if driftID != stored.ID {
		t.Fatalf("pre-clear driftID = %q, want %q", driftID, stored.ID)
	}
	// One-shot — second consume must miss.
	if _, hit := reg.LookupPreClear(ctx, driftTestAgentID, fp); hit {
		t.Fatal("trigger-miss pre-clear must be one-shot")
	}

	// Critical: the CREDENTIALED fingerprint MUST NOT collide with the
	// trigger-miss one. A retry that the inspector reclassifies as
	// credentialed shouldn't accidentally consume a trigger-miss
	// pre-clear and vice versa.
	credFp := ScopeDrift{
		AgentID:        driftTestAgentID,
		ConversationID: driftTestConvID,
		ToolUse:        tu,
		Service:        "shell",
		Action:         "run_command",
	}.Fingerprint()
	if fp == credFp {
		t.Fatal("trigger-miss and credentialed fingerprints must differ")
	}
}

// ── (c) One-off: deny path ──────────────────────────────────────────────────────

// TestScopeDriftE2E_OneOffDeny walks the same flow as Approve but with
// the user declining. SetOutcome lands on Denied and no pre-clear is
// minted; the drift is closed.
func TestScopeDriftE2E_OneOffDeny(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0)
	cache := NewMemoryPendingApprovalCache(time.Minute)

	drift, blockedTU := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope)
	_, claimed := invokeOneOffIntercept(t, reg, cache, driftTestAgentID, driftTestConvID, drift.ID, "need it once")
	if !claimed {
		t.Fatal("expected intercept to claim")
	}
	holds := peekAllHolds(ctx, cache)
	if len(holds) != 1 {
		t.Fatalf("want 1 hold, got %d", len(holds))
	}
	approvalID := holds[0].ID

	result, err := RewriteScopeDriftOneOffApprovalReply(ctx, ScopeDriftReplyRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            anthropicReplyBody("no", approvalID),
		Agent:           &store.Agent{ID: driftTestAgentID, UserID: driftTestUserID},
		ConversationID:  driftTestConvID,
		PendingApproval: cache,
		ScopeDrifts:     reg,
		Logger:          slog.Default(),
	})
	if err != nil {
		t.Fatalf("deny rewrite: %v", err)
	}
	if !result.Rewritten || result.Decision != "deny" {
		t.Fatalf("deny result: %+v", result)
	}
	final, _ := reg.Get(ctx, drift.ID)
	if final.Outcome != ScopeDriftOutcomeDenied {
		t.Fatalf("registry outcome = %q, want %q", final.Outcome, ScopeDriftOutcomeDenied)
	}
	// No pre-clear should have been minted.
	fp := ScopeDrift{
		AgentID:        driftTestAgentID,
		ConversationID: driftTestConvID,
		ToolUse:        blockedTU,
		Service:        driftTestService,
		Action:         driftTestAction,
		Host:           driftTestHost,
		Method:         driftTestMethod,
		Path:           driftTestPath,
	}.Fingerprint()
	if _, hit := reg.LookupPreClear(ctx, driftTestAgentID, fp); hit {
		t.Fatal("LookupPreClear after deny: want miss, got hit")
	}
}

// ── One-shot cap ────────────────────────────────────────────────────────────────

// TestScopeDriftE2E_OneShotCap confirms a second POST against the same
// drift_id is refused: the registry's ClaimOption returns
// ErrDriftAlreadyResolved, the intercept falls through (no hold opens),
// and the original claim's hold is unaffected.
func TestScopeDriftE2E_OneShotCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0)
	cache := NewMemoryPendingApprovalCache(time.Minute)
	drift, _ := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope)

	if _, claimed := invokeOneOffIntercept(t, reg, cache, driftTestAgentID, driftTestConvID, drift.ID, "first try"); !claimed {
		t.Fatal("first claim: expected intercept to claim")
	}
	// Second POST against the same drift_id. The intercept must fall
	// through (return claimed=false) because ClaimOption now rejects.
	if _, claimed := invokeOneOffIntercept(t, reg, cache, driftTestAgentID, driftTestConvID, drift.ID, "second try"); claimed {
		t.Fatal("second claim: expected intercept to fall through (one-shot cap)")
	}
	// The first claim's hold is the only one in the cache.
	holds := peekAllHolds(ctx, cache)
	if len(holds) != 1 {
		t.Fatalf("want exactly 1 hold (first claim's), got %d", len(holds))
	}
}

// ── Cross-conversation guard ────────────────────────────────────────────────────

// TestScopeDriftE2E_CrossConversationGuard confirms a POST carrying a
// drift_id minted in a different conversation is refused at peek time:
// the intercept never claims, the drift stays pending for the rightful
// conversation to resolve, and no hold opens. Rejecting BEFORE claim
// (rather than claim-then-rollback) closes the denial-of-service path
// where a leaked drift_id could be used to permanently terminate
// someone else's pending one-off.
func TestScopeDriftE2E_CrossConversationGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0)
	cache := NewMemoryPendingApprovalCache(time.Minute)
	drift, _ := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope) // minted in conv-drift-1

	if _, claimed := invokeOneOffIntercept(t, reg, cache, driftTestAgentID, "conv-other", drift.ID, "x"); claimed {
		t.Fatal("expected intercept to fall through (wrong conversation)")
	}
	got, _ := reg.Get(ctx, drift.ID)
	if got.ChosenOption != "" {
		t.Fatalf("wrong-conversation POST must not claim; got ChosenOption=%q", got.ChosenOption)
	}
	if got.Outcome != "" {
		t.Fatalf("wrong-conversation POST must leave drift unresolved; got %+v", got)
	}
	if len(peekAllHolds(ctx, cache)) != 0 {
		t.Fatal("cross-conversation refusal must not open a hold")
	}
	// The legitimate session can still claim afterwards.
	if _, claimed := invokeOneOffIntercept(t, reg, cache, driftTestAgentID, driftTestConvID, drift.ID, "legit"); !claimed {
		t.Fatal("legitimate session must still be able to claim the drift")
	}
}

// ── Cross-agent guard ───────────────────────────────────────────────────────────

// TestScopeDriftE2E_CrossAgentGuard mirrors the cross-conversation
// test for the agent_id mismatch case: refuse at peek time so the
// legitimate agent can still claim.
func TestScopeDriftE2E_CrossAgentGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0)
	cache := NewMemoryPendingApprovalCache(time.Minute)
	drift, _ := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope)

	if _, claimed := invokeOneOffIntercept(t, reg, cache, "agent-other", driftTestConvID, drift.ID, "x"); claimed {
		t.Fatal("expected intercept to fall through (wrong agent)")
	}
	got, _ := reg.Get(ctx, drift.ID)
	if got.ChosenOption != "" {
		t.Fatalf("wrong-agent POST must not claim; got ChosenOption=%q", got.ChosenOption)
	}
	if got.Outcome != "" {
		t.Fatalf("wrong-agent POST must leave drift unresolved; got %+v", got)
	}
	if len(peekAllHolds(ctx, cache)) != 0 {
		t.Fatal("cross-agent refusal must not open a hold")
	}
	if _, claimed := invokeOneOffIntercept(t, reg, cache, driftTestAgentID, driftTestConvID, drift.ID, "legit"); !claimed {
		t.Fatal("legitimate agent must still be able to claim the drift")
	}
}

// ── Implicit fall-through (TTL expiry) ──────────────────────────────────────────

// TestScopeDriftE2E_ImplicitFallThroughTTLExpires confirms the
// implicit decision path: the agent picks none of (a)/(b)/(c) and just
// emits its next turn. The drift sits unclaimed until TTL and is
// pruned; no pre-clear is ever minted.
func TestScopeDriftE2E_ImplicitFallThroughTTLExpires(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := &memoryScopeDriftRegistry{
		ttl:     50 * time.Millisecond,
		now:     time.Now,
		drifts:  map[string]*ScopeDrift{},
		cleared: map[string]string{},
	}
	stored, _ := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope)
	// No claim, no markup, no POST — agent just emitted a different
	// next turn. Wait past TTL.
	time.Sleep(120 * time.Millisecond)

	if _, err := reg.Get(ctx, stored.ID); !errors.Is(err, ErrDriftNotFound) {
		t.Fatalf("after TTL: want ErrDriftNotFound, got %v", err)
	}
	// No pre-clear was ever minted (no Succeeded transition).
	fp := stored.Fingerprint()
	if _, hit := reg.LookupPreClear(ctx, driftTestAgentID, fp); hit {
		t.Fatal("implicit fall-through must not mint a pre-clear")
	}
}

// ── (a) Expand: full state machine via the inline intercept ─────────────────────

// TestScopeDriftE2E_ExpandFullStateMachine drives the (a) Expand
// path:
//
//  1. agent reads the menu, POSTs to .../tasks/<id>/expand?surface=inline
//     with a `drift_id` field in the body
//  2. MaybeInterceptInlineExpansion opens a hold at
//     StageAwaitingExpansionApproval AND records the drift link
//  3. user replies "yes" → RewriteInlineTaskApprovalReply resolves,
//     the expansion creator is approved, AND ScopeDrifts.SetOutcome
//     fires with Succeeded → pre-clear minted
//  4. agent's retry of the original blocked tool_use consumes the
//     pre-clear once
func TestScopeDriftE2E_ExpandFullStateMachine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0)
	cache := NewMemoryPendingApprovalCache(time.Minute)
	drift, blockedTU := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope)

	// Step 1: the agent emits a curl POST that mirrors the expand
	// envelope the menu instructs it to send.
	expandBody := map[string]any{
		"expected_tools": []map[string]any{
			{"tool_name": "Bash", "why": "file the issue via curl"},
		},
		"reason":   "the active task should cover github.post_issue",
		"drift_id": drift.ID,
	}
	expandBodyJSON, _ := json.Marshal(expandBody)
	tu := conversation.ToolUse{
		ID:    "tu-expand",
		Name:  "Bash",
		Input: json.RawMessage(`{"body":` + string(mustJSON(string(expandBodyJSON))) + `}`),
	}

	fc := &fakeExpansionCreator{
		ApproveResult: &InlineApprovedExpansion{TaskID: "task-A", Status: "active", Purpose: "manage repo"},
	}
	cfg := PostprocessConfig{
		AgentContext: AgentContext{
			AgentID:     driftTestAgentID,
			AgentUserID: driftTestUserID,
		},
		AuditContext: AuditContext{
			ConversationID: driftTestConvID,
		},
		AuthorizationContext: AuthorizationContext{
			ScopeDrifts: reg,
		},
		ApprovalContext: ApprovalContext{
			PendingApprovals:  cache,
			InlineTaskCreator: fc,
		},
	}
	httpReq := httptest.NewRequest("POST", "http://daemon/api/control/tasks/task-A/expand?surface=inline", nil)
	call := ControlCall{Method: "POST", URL: httpReq.URL}

	// Step 2: intercept fires + opens the hold.
	_, claimed := MaybeInterceptInlineExpansion(httpReq, cfg, func(string, string, string) {}, func(string, ...any) {}, conversation.ProviderAnthropic, tu, call)
	if !claimed {
		t.Fatal("intercept did not claim the expand POST")
	}
	holds := peekAllHolds(ctx, cache)
	if len(holds) != 1 {
		t.Fatalf("want 1 hold after intercept, got %d", len(holds))
	}
	hold := holds[0]
	if hold.Stage != StageAwaitingExpansionApproval {
		t.Fatalf("hold stage = %q, want %q", hold.Stage, StageAwaitingExpansionApproval)
	}
	if hold.ScopeDriftID != drift.ID {
		t.Fatalf("hold ScopeDriftID = %q, want %q (drift_id did not flow through expand body)", hold.ScopeDriftID, drift.ID)
	}

	// Step 3: user types "yes". The reply rewriter resolves the
	// hold, calls ApproveInlineExpansion, AND fires
	// ScopeDrifts.SetOutcome(Succeeded) for the linked drift.
	replyBody := anthropicReplyBody("yes", hold.ID)
	result, err := RewriteInlineTaskApprovalReply(ctx, InlineApprovalRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            replyBody,
		Agent:           &store.Agent{ID: driftTestAgentID, UserID: driftTestUserID},
		ConversationID:  driftTestConvID,
		PendingApproval: cache,
		Creator:         fc,
		ScopeDrifts:     reg,
	})
	if err != nil {
		t.Fatalf("approve rewrite: %v", err)
	}
	if !result.Rewritten || result.Decision != "allow" {
		t.Fatalf("approve result: %+v", result)
	}
	if fc.ApproveCalls != 1 {
		t.Fatalf("expansion creator approve calls = %d, want 1", fc.ApproveCalls)
	}
	final, _ := reg.Get(ctx, drift.ID)
	if final.Outcome != ScopeDriftOutcomeSucceeded {
		t.Fatalf("registry outcome = %q, want %q (SetOutcome should fire on approved expand carrying a drift link)", final.Outcome, ScopeDriftOutcomeSucceeded)
	}

	// Step 4: pre-clear consumed once on the agent's retry.
	fp := ScopeDrift{
		AgentID:        driftTestAgentID,
		ConversationID: driftTestConvID,
		ToolUse:        blockedTU,
		Service:        driftTestService,
		Action:         driftTestAction,
		Host:           driftTestHost,
		Method:         driftTestMethod,
		Path:           driftTestPath,
	}.Fingerprint()
	if id, hit := reg.LookupPreClear(ctx, driftTestAgentID, fp); !hit || id != drift.ID {
		t.Fatalf("pre-clear lookup: hit=%v id=%q, want hit=true id=%q", hit, id, drift.ID)
	}
	if _, hit := reg.LookupPreClear(ctx, driftTestAgentID, fp); hit {
		t.Fatal("pre-clear must be one-shot")
	}
}

// TestScopeDriftE2E_ExpandDenyClosesDriftWithoutPreClear is the mirror
// of ExpandFullStateMachine for the deny side: user types "no", the
// drift transitions to Denied, no pre-clear is minted.
func TestScopeDriftE2E_ExpandDenyClosesDriftWithoutPreClear(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0)
	cache := NewMemoryPendingApprovalCache(time.Minute)
	drift, _ := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope)

	expandBody := map[string]any{
		"expected_tools": []map[string]any{{"tool_name": "Bash", "why": "x"}},
		"reason":         "x",
		"drift_id":       drift.ID,
	}
	expandBodyJSON, _ := json.Marshal(expandBody)
	tu := conversation.ToolUse{
		ID:    "tu-expand",
		Name:  "Bash",
		Input: json.RawMessage(`{"body":` + string(mustJSON(string(expandBodyJSON))) + `}`),
	}
	fc := &fakeExpansionCreator{}
	cfg := PostprocessConfig{
		AgentContext:         AgentContext{AgentID: driftTestAgentID, AgentUserID: driftTestUserID},
		AuditContext:         AuditContext{ConversationID: driftTestConvID},
		AuthorizationContext: AuthorizationContext{ScopeDrifts: reg},
		ApprovalContext:      ApprovalContext{PendingApprovals: cache, InlineTaskCreator: fc},
	}
	httpReq := httptest.NewRequest("POST", "http://daemon/api/control/tasks/task-A/expand?surface=inline", nil)
	if _, ok := MaybeInterceptInlineExpansion(httpReq, cfg, func(string, string, string) {}, func(string, ...any) {}, conversation.ProviderAnthropic, tu, ControlCall{Method: "POST", URL: httpReq.URL}); !ok {
		t.Fatal("intercept did not claim")
	}
	holds := peekAllHolds(ctx, cache)
	if len(holds) != 1 {
		t.Fatalf("want 1 hold, got %d", len(holds))
	}
	result, err := RewriteInlineTaskApprovalReply(ctx, InlineApprovalRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            anthropicReplyBody("no", holds[0].ID),
		Agent:           &store.Agent{ID: driftTestAgentID, UserID: driftTestUserID},
		ConversationID:  driftTestConvID,
		PendingApproval: cache,
		Creator:         fc,
		ScopeDrifts:     reg,
	})
	if err != nil {
		t.Fatalf("deny rewrite: %v", err)
	}
	if result.Decision != "deny" {
		t.Fatalf("deny result: %+v", result)
	}
	final, _ := reg.Get(ctx, drift.ID)
	if final.Outcome != ScopeDriftOutcomeDenied {
		t.Fatalf("registry outcome = %q, want %q", final.Outcome, ScopeDriftOutcomeDenied)
	}
	// No pre-clear.
	fp := ScopeDrift{
		AgentID: driftTestAgentID, ConversationID: driftTestConvID, ToolUse: tu,
		Service: driftTestService, Action: driftTestAction, Host: driftTestHost, Method: driftTestMethod, Path: driftTestPath,
	}.Fingerprint()
	if _, hit := reg.LookupPreClear(ctx, driftTestAgentID, fp); hit {
		t.Fatal("denied expand must not mint a pre-clear")
	}
}

// ── (b) New task: full state machine via the inline intercept ───────────────────

// TestScopeDriftE2E_NewTaskFullStateMachine drives the (b) Create-a-new-
// task path with a drift_id linked in the body. Mirrors the expand
// state machine for the task-creation intercept.
func TestScopeDriftE2E_NewTaskFullStateMachine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0)
	cache := NewMemoryPendingApprovalCache(time.Minute)
	drift, blockedTU := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope)

	taskBody := &runtimetasks.TaskCreateRequest{
		Purpose:                "File the issue",
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
		ExpectedTools: []runtimetasks.ExpectedTool{
			{ToolName: "Bash", Why: "curl to github"},
		},
		DriftID: drift.ID,
	}
	taskBodyJSON, _ := json.Marshal(taskBody)
	tu := conversation.ToolUse{
		ID:    "tu-create",
		Name:  "Bash",
		Input: json.RawMessage(`{"body":` + string(mustJSON(string(taskBodyJSON))) + `}`),
	}

	fc := &fakeInlineTaskCreator{
		resp: &InlineApprovedTask{ID: "task-created", Status: "active", Purpose: "File the issue"},
	}
	cfg := PostprocessConfig{
		AgentContext:         AgentContext{AgentID: driftTestAgentID, AgentUserID: driftTestUserID, AgentName: "agent-drift"},
		AuditContext:         AuditContext{ConversationID: driftTestConvID},
		AuthorizationContext: AuthorizationContext{ScopeDrifts: reg},
		ApprovalContext:      ApprovalContext{PendingApprovals: cache, InlineTaskCreator: fc},
	}
	httpReq := httptest.NewRequest("POST", "http://daemon/api/control/tasks?surface=inline", nil)
	call := ControlCall{Method: "POST", URL: httpReq.URL}

	if _, ok := MaybeInterceptInlineTaskDefinition(httpReq, cfg, func(string, string, string) {}, func(string, ...any) {}, conversation.ProviderAnthropic, tu, call); !ok {
		t.Fatal("intercept did not claim the create POST")
	}
	holds := peekAllHolds(ctx, cache)
	if len(holds) != 1 {
		t.Fatalf("want 1 hold, got %d", len(holds))
	}
	hold := holds[0]
	if hold.Stage != StageAwaitingTaskApproval {
		t.Fatalf("hold stage = %q, want %q", hold.Stage, StageAwaitingTaskApproval)
	}
	if hold.ScopeDriftID != drift.ID {
		t.Fatalf("hold ScopeDriftID = %q, want %q (drift_id did not flow through TaskCreateRequest)", hold.ScopeDriftID, drift.ID)
	}

	result, err := RewriteInlineTaskApprovalReply(ctx, InlineApprovalRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            anthropicReplyBody("yes", hold.ID),
		Agent:           &store.Agent{ID: driftTestAgentID, UserID: driftTestUserID},
		ConversationID:  driftTestConvID,
		PendingApproval: cache,
		Creator:         fc,
		ScopeDrifts:     reg,
	})
	if err != nil {
		t.Fatalf("approve rewrite: %v", err)
	}
	if !result.Rewritten || result.Decision != "allow" {
		t.Fatalf("approve result: %+v", result)
	}
	final, _ := reg.Get(ctx, drift.ID)
	if final.Outcome != ScopeDriftOutcomeSucceeded {
		t.Fatalf("registry outcome = %q, want %q", final.Outcome, ScopeDriftOutcomeSucceeded)
	}
	fp := ScopeDrift{
		AgentID: driftTestAgentID, ConversationID: driftTestConvID, ToolUse: blockedTU,
		Service: driftTestService, Action: driftTestAction, Host: driftTestHost, Method: driftTestMethod, Path: driftTestPath,
	}.Fingerprint()
	if id, hit := reg.LookupPreClear(ctx, driftTestAgentID, fp); !hit || id != drift.ID {
		t.Fatalf("pre-clear after approved new_task: hit=%v id=%q, want hit=true id=%q", hit, id, drift.ID)
	}
}

// fakeInlineTaskCreator and mustJSON are defined in sibling test files
// (inline_task_release_test.go and secret_detection_test.go).

// ── Inbound rewrite: placeholder → original tool_use + menu tool_result ─────

// TestScopeDriftE2E_InboundRewritesPlaceholder drives the full
// mutate→inbound-restore round-trip on a representative Anthropic
// /v1/messages body:
//
//  1. Seed the registry with a pending substitution (mirroring what
//     the mint path writes alongside the drift).
//  2. Build an inbound /v1/messages body whose history reflects what
//     the harness sends after running the placeholder: an assistant
//     turn carrying the Bash placeholder tool_use with the original
//     tool_use_id, paired with a user turn whose tool_result names
//     the same tool_use_id.
//  3. Run RewriteScopeDriftPlaceholders.
//  4. Assert the assistant turn was restored byte-for-byte (the
//     model's original Bash command + input survives) AND the
//     user-turn tool_result content is the menu text.
//
// Also asserts the substitution is consumed one-shot — a second
// RewriteScopeDriftPlaceholders is a no-op.
func TestScopeDriftE2E_InboundRewritesPlaceholder(t *testing.T) {
	t.Parallel()
	reg := NewMemoryScopeDriftRegistry(time.Minute)
	const (
		toolUseID        = "tu-blocked-inbound"
		originalToolName = "Bash"
		menuText         = "Clawvisor: github.post_issue is outside your current task scope.\n  Drift ID: drift-fake-1\n(menu body…)"
	)
	originalInput := json.RawMessage(`{"command":"curl -X POST https://api.github.com/repos/o/r/issues -d '{\"title\":\"hi\"}'"}`)
	if err := reg.RegisterPendingSubstitution(context.Background(),
		PendingSubstitutionKey{
			AgentID:        driftTestAgentID,
			ConversationID: driftTestConvID,
			ToolUseID:      toolUseID,
		},
		PendingSubstitution{
			DriftID:           "drift-fake-1",
			MenuText:          menuText,
			OriginalToolName:  originalToolName,
			OriginalToolInput: append([]byte(nil), originalInput...),
		},
	); err != nil {
		t.Fatalf("RegisterPendingSubstitution: %v", err)
	}

	placeholderCmd := BuildScopeDriftPlaceholderCommand(originalToolName, "drift-fake-1")
	body := []byte(`{"messages":[` +
		`{"role":"user","content":"open an issue"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"` + toolUseID + `","name":"Bash","input":{"command":` + string(mustJSON(placeholderCmd)) + `}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + toolUseID + `","content":"command exited 0"}]}` +
		`]}`)

	agent := &store.Agent{ID: driftTestAgentID, UserID: driftTestUserID}
	result, err := RewriteScopeDriftPlaceholders(context.Background(), ScopeDriftInboundRewriteRequest{
		HTTPRequest:    httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:       conversation.ProviderAnthropic,
		Body:           body,
		Agent:          agent,
		ConversationID: driftTestConvID,
		ScopeDrifts:    reg,
		Logger:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("RewriteScopeDriftPlaceholders: %v", err)
	}
	if !result.Rewritten {
		t.Fatal("expected Rewritten=true")
	}
	if want := []string{"drift-fake-1"}; len(result.AppliedDriftIDs) != 1 || result.AppliedDriftIDs[0] != want[0] {
		t.Fatalf("AppliedDriftIDs = %v, want %v", result.AppliedDriftIDs, want)
	}

	var rewritten struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(result.Body, &rewritten); err != nil {
		t.Fatalf("rewritten body unmarshal: %v", err)
	}
	if len(rewritten.Messages) != 3 {
		t.Fatalf("expected 3 messages after rewrite, got %d", len(rewritten.Messages))
	}

	// Assistant turn: tool_use restored byte-for-byte.
	var assistantMsg struct {
		Role    string `json:"role"`
		Content []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rewritten.Messages[1], &assistantMsg); err != nil {
		t.Fatalf("assistant message unmarshal: %v", err)
	}
	if assistantMsg.Role != "assistant" {
		t.Fatalf("assistant message role = %q, want assistant", assistantMsg.Role)
	}
	if len(assistantMsg.Content) != 1 || assistantMsg.Content[0].Type != "tool_use" {
		t.Fatalf("assistant content shape unexpected: %+v", assistantMsg.Content)
	}
	if assistantMsg.Content[0].Name != originalToolName {
		t.Fatalf("restored tool name = %q, want %q (Bash placeholder should have been replaced)", assistantMsg.Content[0].Name, originalToolName)
	}
	if assistantMsg.Content[0].ID != toolUseID {
		t.Fatalf("restored tool_use_id = %q, want %q (id must round-trip)", assistantMsg.Content[0].ID, toolUseID)
	}
	if string(assistantMsg.Content[0].Input) != string(originalInput) {
		t.Fatalf("restored tool input mismatch:\n  got:  %s\n  want: %s", string(assistantMsg.Content[0].Input), string(originalInput))
	}

	// User turn: tool_result content is the menu text.
	var userMsg struct {
		Role    string `json:"role"`
		Content []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rewritten.Messages[2], &userMsg); err != nil {
		t.Fatalf("user message unmarshal: %v", err)
	}
	if userMsg.Role != "user" {
		t.Fatalf("user message role = %q, want user", userMsg.Role)
	}
	if len(userMsg.Content) != 1 || userMsg.Content[0].Type != "tool_result" {
		t.Fatalf("user content shape unexpected: %+v", userMsg.Content)
	}
	var resultBlocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(userMsg.Content[0].Content, &resultBlocks); err != nil {
		t.Fatalf("tool_result content unmarshal: %v", err)
	}
	if len(resultBlocks) != 1 || resultBlocks[0].Type != "text" {
		t.Fatalf("expected single text block in tool_result, got %+v", resultBlocks)
	}
	if !strings.Contains(resultBlocks[0].Text, "outside your current task scope") {
		t.Fatalf("tool_result content does not carry menu text:\n%s", resultBlocks[0].Text)
	}
	if !strings.Contains(resultBlocks[0].Text, "Drift ID: drift-fake-1") {
		t.Fatalf("tool_result content missing Drift ID:\n%s", resultBlocks[0].Text)
	}

	// Persistence: a second rewrite (next conversation turn) still
	// finds the substitution and restores the placeholder. The harness
	// keeps the Bash placeholder in its stored history for the rest of
	// the conversation; without persistent restoration the model would
	// see its own past as the placeholder from turn 3 onward.
	second, err := RewriteScopeDriftPlaceholders(context.Background(), ScopeDriftInboundRewriteRequest{
		HTTPRequest:    httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:       conversation.ProviderAnthropic,
		Body:           body,
		Agent:          agent,
		ConversationID: driftTestConvID,
		ScopeDrifts:    reg,
		Logger:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("second RewriteScopeDriftPlaceholders: %v", err)
	}
	if !second.Rewritten {
		t.Fatal("expected second rewrite to also restore the placeholder (substitutions persist across turns)")
	}
}

// TestScopeDriftE2E_InboundRewritesOpenAIResponsesPlaceholder mirrors
// TestScopeDriftE2E_InboundRewritesPlaceholder but on the OpenAI
// Responses wire shape: the previous turn's function_call gets
// restored byte-for-byte and the matching function_call_output's
// content is the menu text.
func TestScopeDriftE2E_InboundRewritesOpenAIResponsesPlaceholder(t *testing.T) {
	t.Parallel()
	reg := NewMemoryScopeDriftRegistry(time.Minute)
	const (
		callID           = "call_1"
		originalToolName = "exec_command"
		menuText         = "Clawvisor: github.post_issue is outside your current task scope.\n  Drift ID: drift-fake-openai-1\n(menu body…)"
	)
	originalInput := json.RawMessage(`{"cmd":"mkdir /tmp/needs-task"}`)
	if err := reg.RegisterPendingSubstitution(context.Background(),
		PendingSubstitutionKey{
			AgentID:        driftTestAgentID,
			ConversationID: driftTestConvID,
			ToolUseID:      callID,
		},
		PendingSubstitution{
			DriftID:           "drift-fake-openai-1",
			MenuText:          menuText,
			OriginalToolName:  originalToolName,
			OriginalToolInput: append([]byte(nil), originalInput...),
		},
	); err != nil {
		t.Fatalf("RegisterPendingSubstitution: %v", err)
	}

	placeholderCmd := BuildScopeDriftPlaceholderCommand(originalToolName, "drift-fake-openai-1")
	placeholderArgs, _ := json.Marshal(map[string]string{"command": placeholderCmd})

	body := []byte(`{"input":[` +
		`{"type":"message","role":"user","content":"open an issue"},` +
		`{"type":"function_call","call_id":"` + callID + `","name":"Bash","arguments":` + string(mustJSON(string(placeholderArgs))) + `},` +
		`{"type":"function_call_output","call_id":"` + callID + `","output":"command exited 0"}` +
		`]}`)

	agent := &store.Agent{ID: driftTestAgentID, UserID: driftTestUserID}
	httpReq := httptest.NewRequest("POST", "/v1/responses", nil)
	result, err := RewriteScopeDriftPlaceholders(context.Background(), ScopeDriftInboundRewriteRequest{
		HTTPRequest:    httpReq,
		Provider:       conversation.ProviderOpenAI,
		Body:           body,
		Agent:          agent,
		ConversationID: driftTestConvID,
		ScopeDrifts:    reg,
		Logger:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("RewriteScopeDriftPlaceholders: %v", err)
	}
	if !result.Rewritten {
		t.Fatalf("expected Rewritten=true; AppliedDriftIDs=%v", result.AppliedDriftIDs)
	}

	var rewritten struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(result.Body, &rewritten); err != nil {
		t.Fatalf("rewritten body unmarshal: %v", err)
	}
	if len(rewritten.Input) != 3 {
		t.Fatalf("expected 3 input items after rewrite, got %d", len(rewritten.Input))
	}

	var fc struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	if err := json.Unmarshal(rewritten.Input[1], &fc); err != nil {
		t.Fatalf("function_call unmarshal: %v", err)
	}
	if fc.Type != "function_call" || fc.CallID != callID {
		t.Fatalf("function_call shape mismatch: %+v", fc)
	}
	if fc.Name != originalToolName {
		t.Fatalf("restored function_call name = %q, want %q", fc.Name, originalToolName)
	}
	if fc.Arguments != string(originalInput) {
		t.Fatalf("restored function_call arguments = %q, want %q", fc.Arguments, string(originalInput))
	}

	var fco struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(rewritten.Input[2], &fco); err != nil {
		t.Fatalf("function_call_output unmarshal: %v", err)
	}
	if fco.Type != "function_call_output" || fco.CallID != callID {
		t.Fatalf("function_call_output shape mismatch: %+v", fco)
	}
	if !strings.Contains(fco.Output, "outside your current task scope") {
		t.Fatalf("function_call_output does not carry menu text: %s", fco.Output)
	}
}

// TestScopeDriftE2E_InboundRewritesOpenAIResponsesPlaceholderChained
// exercises the OpenAI Responses chained-mode path (codex's pattern):
// the harness sends `previous_response_id` referencing the prior turn,
// and `input[]` carries ONLY the function_call_output (no function_call
// to restore — OpenAI's server-stored history holds the model's real
// call). The rewriter must substitute the function_call_output content
// without requiring a restoration target.
//
// This regression locks the codex fix: pre-fix the atomicity gate
// skipped the substitution because no function_call was in input[] to
// restore, so codex saw an empty Bash stdout and fabricated results.
func TestScopeDriftE2E_InboundRewritesOpenAIResponsesPlaceholderChained(t *testing.T) {
	t.Parallel()
	reg := NewMemoryScopeDriftRegistry(time.Minute)
	const (
		callID           = "call_chained_1"
		originalToolName = "exec_command"
		menuText         = "Clawvisor: github.post_issue is outside your current task scope.\n  Drift ID: drift-chained-1\n(menu body…)"
	)
	originalInput := json.RawMessage(`{"cmd":"mkdir /tmp/needs-task"}`)
	if err := reg.RegisterPendingSubstitution(context.Background(),
		PendingSubstitutionKey{
			AgentID:        driftTestAgentID,
			ConversationID: driftTestConvID,
			ToolUseID:      callID,
		},
		PendingSubstitution{
			DriftID:           "drift-chained-1",
			MenuText:          menuText,
			OriginalToolName:  originalToolName,
			OriginalToolInput: append([]byte(nil), originalInput...),
		},
	); err != nil {
		t.Fatalf("RegisterPendingSubstitution: %v", err)
	}

	// Chained body: previous_response_id references a prior turn (OpenAI
	// retrieves the real function_call from its stored history). input[]
	// carries only the function_call_output the harness produced after
	// running the placeholder.
	body := []byte(`{` +
		`"previous_response_id":"resp_prev_abc123",` +
		`"input":[` +
		`{"type":"function_call_output","call_id":"` + callID + `","output":""}` +
		`]}`)

	agent := &store.Agent{ID: driftTestAgentID, UserID: driftTestUserID}
	httpReq := httptest.NewRequest("POST", "/v1/responses", nil)
	result, err := RewriteScopeDriftPlaceholders(context.Background(), ScopeDriftInboundRewriteRequest{
		HTTPRequest:    httpReq,
		Provider:       conversation.ProviderOpenAI,
		Body:           body,
		Agent:          agent,
		ConversationID: driftTestConvID,
		ScopeDrifts:    reg,
		Logger:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("RewriteScopeDriftPlaceholders: %v", err)
	}
	if !result.Rewritten {
		t.Fatalf("expected Rewritten=true in chained mode; AppliedDriftIDs=%v", result.AppliedDriftIDs)
	}

	var rewritten struct {
		PreviousResponseID string            `json:"previous_response_id"`
		Input              []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(result.Body, &rewritten); err != nil {
		t.Fatalf("rewritten body unmarshal: %v", err)
	}
	if rewritten.PreviousResponseID != "resp_prev_abc123" {
		t.Fatalf("previous_response_id should round-trip unchanged; got %q", rewritten.PreviousResponseID)
	}
	if len(rewritten.Input) != 1 {
		t.Fatalf("expected 1 input item in chained mode (no function_call to restore), got %d", len(rewritten.Input))
	}

	var fco struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(rewritten.Input[0], &fco); err != nil {
		t.Fatalf("function_call_output unmarshal: %v", err)
	}
	if fco.Type != "function_call_output" || fco.CallID != callID {
		t.Fatalf("function_call_output shape mismatch: %+v", fco)
	}
	if !strings.Contains(fco.Output, "outside your current task scope") {
		t.Fatalf("function_call_output does not carry menu text: %s", fco.Output)
	}
}

// peekAllHolds returns every hold for the test fixture's (user, agent,
// conv) bucket. SnapshotHoldsForTest keys by a zero-conversation
// bucket and would miss conversation-scoped holds; this helper reaches
// into the memory cache directly so the e2e flow can assert the hold
// shape the resolver actually wrote.
func peekAllHolds(ctx context.Context, cache *MemoryPendingApprovalCache) []PendingLiteApproval {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	key := pendingApprovalKey{
		userID:         driftTestUserID,
		agentID:        driftTestAgentID,
		provider:       conversation.ProviderAnthropic,
		conversationID: driftTestConvID,
	}
	items := cache.pending[key]
	out := make([]PendingLiteApproval, len(items))
	copy(out, items)
	return out
}

// TestScopeDriftE2E_ExpandCrossAgentGuard verifies that an expand request from a different
// agent or conversation using a victim's drift_id is rejected.
func TestScopeDriftE2E_ExpandCrossAgentGuard(t *testing.T) {
	t.Parallel()
	reg := NewMemoryScopeDriftRegistry(0)
	cache := NewMemoryPendingApprovalCache(time.Minute)
	drift, _ := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope)

	expandBody := map[string]any{
		"expected_tools": []map[string]any{{"tool_name": "Bash", "why": "x"}},
		"reason":         "x",
		"drift_id":       drift.ID,
	}
	expandBodyJSON, _ := json.Marshal(expandBody)
	tu := conversation.ToolUse{
		ID:    "tu-expand",
		Name:  "Bash",
		Input: json.RawMessage(`{"body":` + string(mustJSON(string(expandBodyJSON))) + `}`),
	}

	fc := &fakeExpansionCreator{}
	cfg := PostprocessConfig{
		AgentContext:         AgentContext{AgentID: "agent-attacker", AgentUserID: driftTestUserID},
		AuditContext:         AuditContext{ConversationID: driftTestConvID},
		AuthorizationContext: AuthorizationContext{ScopeDrifts: reg},
		ApprovalContext:      ApprovalContext{PendingApprovals: cache, InlineTaskCreator: fc},
	}
	httpReq := httptest.NewRequest("POST", "http://daemon/api/control/tasks/task-A/expand?surface=inline", nil)
	call := ControlCall{Method: "POST", URL: httpReq.URL}

	if _, claimed := MaybeInterceptInlineExpansion(httpReq, cfg, func(string, string, string) {}, func(string, ...any) {}, conversation.ProviderAnthropic, tu, call); claimed {
		t.Fatal("expected expand from different agent to be rejected")
	}

	// Also check different conversation!
	cfg2 := PostprocessConfig{
		AgentContext:         AgentContext{AgentID: driftTestAgentID, AgentUserID: driftTestUserID},
		AuditContext:         AuditContext{ConversationID: "conv-attacker"}, // Different conversation!
		AuthorizationContext: AuthorizationContext{ScopeDrifts: reg},
		ApprovalContext:      ApprovalContext{PendingApprovals: cache, InlineTaskCreator: fc},
	}
	if _, claimed := MaybeInterceptInlineExpansion(httpReq, cfg2, func(string, string, string) {}, func(string, ...any) {}, conversation.ProviderAnthropic, tu, call); claimed {
		t.Fatal("expected expand from different conversation to be rejected")
	}
}

// TestScopeDriftE2E_OneShotCapExpandAndOneOff verifies that submitting an expand request
// successfully claims the drift, and a subsequent one-off request against the same drift_id
// fails with ErrDriftAlreadyResolved.
func TestScopeDriftE2E_OneShotCapExpandAndOneOff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0)
	cache := NewMemoryPendingApprovalCache(time.Minute)
	drift, _ := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope)

	// Step 1: Submit an expand request. It should successfully claim the drift.
	expandBody := map[string]any{
		"expected_tools": []map[string]any{{"tool_name": "Bash", "why": "x"}},
		"reason":         "x",
		"drift_id":       drift.ID,
	}
	expandBodyJSON, _ := json.Marshal(expandBody)
	tu := conversation.ToolUse{
		ID:    "tu-expand",
		Name:  "Bash",
		Input: json.RawMessage(`{"body":` + string(mustJSON(string(expandBodyJSON))) + `}`),
	}

	fc := &fakeExpansionCreator{}
	cfg := PostprocessConfig{
		AgentContext:         AgentContext{AgentID: driftTestAgentID, AgentUserID: driftTestUserID},
		AuditContext:         AuditContext{ConversationID: driftTestConvID},
		AuthorizationContext: AuthorizationContext{ScopeDrifts: reg},
		ApprovalContext:      ApprovalContext{PendingApprovals: cache, InlineTaskCreator: fc},
	}
	httpReq := httptest.NewRequest("POST", "http://daemon/api/control/tasks/task-A/expand?surface=inline", nil)
	call := ControlCall{Method: "POST", URL: httpReq.URL}

	if _, claimed := MaybeInterceptInlineExpansion(httpReq, cfg, func(string, string, string) {}, func(string, ...any) {}, conversation.ProviderAnthropic, tu, call); !claimed {
		t.Fatal("expected expand intercept to claim the drift")
	}

	// Verify drift is now claimed with "expand".
	stored, err := reg.Get(ctx, drift.ID)
	if err != nil {
		t.Fatalf("get drift: %v", err)
	}
	if stored.ChosenOption != ScopeDriftOptionExpand {
		t.Fatalf("drift ChosenOption = %q, want %q", stored.ChosenOption, ScopeDriftOptionExpand)
	}

	// Step 2: Try a subsequent one-off request against the same drift_id. It should fail.
	_, claimed := invokeOneOffIntercept(t, reg, cache, driftTestAgentID, driftTestConvID, drift.ID, "need a one-off too")
	if claimed {
		t.Fatal("expected one-off intercept to reject already claimed drift")
	}

	// Verify that the drift ChosenOption remains "expand" (was not wiped by the one-off's deferred rollback).
	stored, err = reg.Get(ctx, drift.ID)
	if err != nil {
		t.Fatalf("get drift: %v", err)
	}
	if stored.ChosenOption != ScopeDriftOptionExpand {
		t.Fatalf("drift ChosenOption was wiped! got %q, want %q", stored.ChosenOption, ScopeDriftOptionExpand)
	}
}

// TestScopeDriftE2E_ExpandCreatorFailureClosesDrift verifies that if CreatePendingInlineExpansion
// fails, the drift outcome is transitioned to Denied.
func TestScopeDriftE2E_ExpandCreatorFailureClosesDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0)
	cache := NewMemoryPendingApprovalCache(time.Minute)
	drift, _ := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope)

	expandBody := map[string]any{
		"expected_tools": []map[string]any{{"tool_name": "Bash", "why": "x"}},
		"reason":         "x",
		"drift_id":       drift.ID,
	}
	expandBodyJSON, _ := json.Marshal(expandBody)
	tu := conversation.ToolUse{
		ID:    "tu-expand",
		Name:  "Bash",
		Input: json.RawMessage(`{"body":` + string(mustJSON(string(expandBodyJSON))) + `}`),
	}

	// This expansion creator returns an error to simulate DB/creator failure.
	fc := &fakeExpansionCreator{
		CreatePendingErr: errors.New("database outage"),
	}
	cfg := PostprocessConfig{
		AgentContext:         AgentContext{AgentID: driftTestAgentID, AgentUserID: driftTestUserID},
		AuditContext:         AuditContext{ConversationID: driftTestConvID},
		AuthorizationContext: AuthorizationContext{ScopeDrifts: reg},
		ApprovalContext:      ApprovalContext{PendingApprovals: cache, InlineTaskCreator: fc},
	}
	httpReq := httptest.NewRequest("POST", "http://daemon/api/control/tasks/task-A/expand?surface=inline", nil)
	call := ControlCall{Method: "POST", URL: httpReq.URL}

	if _, claimed := MaybeInterceptInlineExpansion(httpReq, cfg, func(string, string, string) {}, func(string, ...any) {}, conversation.ProviderAnthropic, tu, call); claimed {
		t.Fatal("expected expansion to fall through due to creator failure")
	}

	// Verify that the drift outcome and chosen option were rolled back (cleared).
	stored, err := reg.Get(ctx, drift.ID)
	if err != nil {
		t.Fatalf("get drift: %v", err)
	}
	if stored.Outcome != "" {
		t.Fatalf("drift Outcome = %q, want empty", stored.Outcome)
	}
	if stored.ChosenOption != "" {
		t.Fatalf("drift ChosenOption = %q, want empty", stored.ChosenOption)
	}

	// Verify we can claim the drift again (it is not stranded or permanently resolved).
	if _, err := reg.ClaimOption(ctx, drift.ID, ScopeDriftOptionExpand, "retry"); err != nil {
		t.Fatalf("expected drift to be claimable again, but got err: %v", err)
	}
}

// TestScopeDriftE2E_NewTaskPendingCreatorFailureClosesDrift verifies that if CreatePendingInlineTask
// fails, the drift outcome is transitioned to Denied.
func TestScopeDriftE2E_NewTaskPendingCreatorFailureClosesDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0)
	cache := NewMemoryPendingApprovalCache(time.Minute)
	drift, _ := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope)

	taskBody := &runtimetasks.TaskCreateRequest{
		Purpose:                "File the issue",
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
		ExpectedTools: []runtimetasks.ExpectedTool{
			{ToolName: "Bash", Why: "curl to github"},
		},
		DriftID: drift.ID,
	}
	taskBodyJSON, _ := json.Marshal(taskBody)
	tu := conversation.ToolUse{
		ID:    "tu-create",
		Name:  "Bash",
		Input: json.RawMessage(`{"body":` + string(mustJSON(string(taskBodyJSON))) + `}`),
	}

	// This creator returns an error on pending task creation.
	fc := &fakeInlineTaskCreator{
		pendingErr: errors.New("database outage"),
	}
	cfg := PostprocessConfig{
		AgentContext:         AgentContext{AgentID: driftTestAgentID, AgentUserID: driftTestUserID, AgentName: "agent-drift"},
		AuditContext:         AuditContext{ConversationID: driftTestConvID},
		AuthorizationContext: AuthorizationContext{ScopeDrifts: reg},
		ApprovalContext:      ApprovalContext{PendingApprovals: cache, InlineTaskCreator: fc},
	}
	httpReq := httptest.NewRequest("POST", "http://daemon/api/control/tasks?surface=inline", nil)
	call := ControlCall{Method: "POST", URL: httpReq.URL}

	if _, ok := MaybeInterceptInlineTaskDefinition(httpReq, cfg, func(string, string, string) {}, func(string, ...any) {}, conversation.ProviderAnthropic, tu, call); ok {
		t.Fatal("expected task definition intercept to fall through due to creator failure")
	}

	// Verify that the drift outcome and chosen option were rolled back (cleared).
	stored, err := reg.Get(ctx, drift.ID)
	if err != nil {
		t.Fatalf("get drift: %v", err)
	}
	if stored.Outcome != "" {
		t.Fatalf("drift Outcome = %q, want empty", stored.Outcome)
	}
	if stored.ChosenOption != "" {
		t.Fatalf("drift ChosenOption = %q, want empty", stored.ChosenOption)
	}

	// Verify we can claim the drift again (it is not stranded or permanently resolved).
	if _, err := reg.ClaimOption(ctx, drift.ID, ScopeDriftOptionNewTask, "retry"); err != nil {
		t.Fatalf("expected drift to be claimable again, but got err: %v", err)
	}
}

// TestScopeDriftE2E_NewTaskAutoApproveResolvesDriftSucceeded verifies that if a task is
// auto-approved, the drift is successfully resolved immediately without human prompting.
func TestScopeDriftE2E_NewTaskAutoApproveResolvesDriftSucceeded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0)
	cache := NewMemoryPendingApprovalCache(time.Minute)
	drift, _ := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope)

	taskBody := &runtimetasks.TaskCreateRequest{
		Purpose:                "File the issue",
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
		ExpectedTools: []runtimetasks.ExpectedTool{
			{ToolName: "Bash", Why: "curl to github"},
		},
		DriftID: drift.ID,
	}
	taskBodyJSON, _ := json.Marshal(taskBody)
	tu := conversation.ToolUse{
		ID:    "tu-create",
		Name:  "Bash",
		Input: json.RawMessage(`{"body":` + string(mustJSON(string(taskBodyJSON))) + `}`),
	}

	fc := &fakeInlineTaskCreator{
		resp: &InlineApprovedTask{ID: "task-created", Status: "active", Purpose: "File the issue"},
	}
	assessor := &mockTaskRiskAssessor{
		verdict: &TaskRiskAssessment{
			RiskLevel:   "low",
			IntentMatch: "yes",
		},
	}
	cfg := PostprocessConfig{
		AgentContext:         AgentContext{AgentID: driftTestAgentID, AgentUserID: driftTestUserID, AgentName: "agent-drift"},
		AuditContext:         AuditContext{ConversationID: driftTestConvID},
		AuthorizationContext: AuthorizationContext{ScopeDrifts: reg},
		ApprovalContext: ApprovalContext{
			PendingApprovals:                 cache,
			InlineTaskCreator:                fc,
			TaskRiskAssessor:                 assessor,
			ConversationAutoApproveThreshold: "low",
			RecentUserTurns:                  []string{"please file the issue immediately"},
		},
	}
	httpReq := httptest.NewRequest("POST", "http://daemon/api/control/tasks?surface=inline", nil)
	call := ControlCall{Method: "POST", URL: httpReq.URL}

	verdict, ok := MaybeInterceptInlineTaskDefinition(httpReq, cfg, func(string, string, string) {}, func(string, ...any) {}, conversation.ProviderAnthropic, tu, call)
	if !ok {
		t.Fatal("expected task definition intercept to claim the request")
	}
	// Auto-approve verdict shape after continuation removal: Outcome=Deny
	// (block the original control_tool POST from reaching the harness)
	// + SubstituteWithToolCall set to the Bash placeholder + SubstituteWith
	// carrying the [Clawvisor] user-facing notice. The inbound rewriter
	// restores the original call and substitutes the augmentation on the
	// next /v1/messages.
	if verdict.SubstituteWithToolCall == nil {
		t.Fatal("expected auto-approve verdict to set SubstituteWithToolCall (Bash placeholder)")
	}
	if verdict.SubstituteWith == "" {
		t.Fatal("expected auto-approve verdict to set SubstituteWith with the [Clawvisor] notice")
	}

	// Verify that the drift outcome was marked as Succeeded.
	stored, err := reg.Get(ctx, drift.ID)
	if err != nil {
		t.Fatalf("get drift: %v", err)
	}
	if stored.Outcome != ScopeDriftOutcomeSucceeded {
		t.Fatalf("drift Outcome = %q, want %q", stored.Outcome, ScopeDriftOutcomeSucceeded)
	}
}

// TestScopeDriftE2E_NewTaskAutoApproveCreatorFailureRollsBackDrift verifies that if task auto-approval
// triggers but the creator fails to build the task, the drift claim is rolled back and the request falls through.
func TestScopeDriftE2E_NewTaskAutoApproveCreatorFailureRollsBackDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0)
	cache := NewMemoryPendingApprovalCache(time.Minute)
	drift, _ := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope)

	taskBody := &runtimetasks.TaskCreateRequest{
		Purpose:                "File the issue",
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
		ExpectedTools: []runtimetasks.ExpectedTool{
			{ToolName: "Bash", Why: "curl to github"},
		},
		DriftID: drift.ID,
	}
	taskBodyJSON, _ := json.Marshal(taskBody)
	tu := conversation.ToolUse{
		ID:    "tu-create",
		Name:  "Bash",
		Input: json.RawMessage(`{"body":` + string(mustJSON(string(taskBodyJSON))) + `}`),
	}

	// Mock a creator error for both auto-approval task creation and pending creation.
	fc := &fakeInlineTaskCreator{
		err:        errors.New("auto-approve task creation failed"),
		pendingErr: errors.New("pending task creation failed"),
	}
	assessor := &mockTaskRiskAssessor{
		verdict: &TaskRiskAssessment{
			RiskLevel:   "low",
			IntentMatch: "yes",
		},
	}
	cfg := PostprocessConfig{
		AgentContext:         AgentContext{AgentID: driftTestAgentID, AgentUserID: driftTestUserID, AgentName: "agent-drift"},
		AuditContext:         AuditContext{ConversationID: driftTestConvID},
		AuthorizationContext: AuthorizationContext{ScopeDrifts: reg},
		ApprovalContext: ApprovalContext{
			PendingApprovals:                 cache,
			InlineTaskCreator:                fc,
			TaskRiskAssessor:                 assessor,
			ConversationAutoApproveThreshold: "low",
			RecentUserTurns:                  []string{"please file the issue immediately"},
		},
	}
	httpReq := httptest.NewRequest("POST", "http://daemon/api/control/tasks?surface=inline", nil)
	call := ControlCall{Method: "POST", URL: httpReq.URL}

	_, ok := MaybeInterceptInlineTaskDefinition(httpReq, cfg, func(string, string, string) {}, func(string, ...any) {}, conversation.ProviderAnthropic, tu, call)
	if ok {
		t.Fatal("expected task definition intercept to return false (fallthrough) due to creation failure")
	}

	// Verify that the drift outcome and chosen option were rolled back (cleared).
	stored, err := reg.Get(ctx, drift.ID)
	if err != nil {
		t.Fatalf("get drift: %v", err)
	}
	if stored.Outcome != "" {
		t.Fatalf("drift Outcome = %q, want empty", stored.Outcome)
	}
	if stored.ChosenOption != "" {
		t.Fatalf("drift ChosenOption = %q, want empty", stored.ChosenOption)
	}

	// Verify we can claim the drift again (it was successfully rolled back).
	if _, err := reg.ClaimOption(ctx, drift.ID, ScopeDriftOptionNewTask, "retry"); err != nil {
		t.Fatalf("expected drift to be claimable again, but got err: %v", err)
	}
}

// TestScopeDriftE2E_NewTaskAutoApproveCreatorFailureFallsBackToManual verifies that if task auto-approval
// triggers but task creation fails, the interceptor falls back to manual approval and successfully creates a hold.
func TestScopeDriftE2E_NewTaskAutoApproveCreatorFailureFallsBackToManual(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0)
	cache := NewMemoryPendingApprovalCache(time.Minute)
	drift, _ := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope)

	taskBody := &runtimetasks.TaskCreateRequest{
		Purpose:                "File the issue",
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
		ExpectedTools: []runtimetasks.ExpectedTool{
			{ToolName: "Bash", Why: "curl to github"},
		},
		DriftID: drift.ID,
	}
	taskBodyJSON, _ := json.Marshal(taskBody)
	tu := conversation.ToolUse{
		ID:    "tu-create",
		Name:  "Bash",
		Input: json.RawMessage(`{"body":` + string(mustJSON(string(taskBodyJSON))) + `}`),
	}

	// Creator fails on auto-approve task creation, but succeeds on manual pending task creation.
	fc := &fakeInlineTaskCreator{
		err:        errors.New("auto-approve task creation failed"),
		pendingErr: nil, // succeeds
	}
	assessor := &mockTaskRiskAssessor{
		verdict: &TaskRiskAssessment{
			RiskLevel:   "low",
			IntentMatch: "yes",
		},
	}
	cfg := PostprocessConfig{
		AgentContext:         AgentContext{AgentID: driftTestAgentID, AgentUserID: driftTestUserID, AgentName: "agent-drift"},
		AuditContext:         AuditContext{ConversationID: driftTestConvID},
		AuthorizationContext: AuthorizationContext{ScopeDrifts: reg},
		ApprovalContext: ApprovalContext{
			PendingApprovals:                 cache,
			InlineTaskCreator:                fc,
			TaskRiskAssessor:                 assessor,
			ConversationAutoApproveThreshold: "low",
			RecentUserTurns:                  []string{"please file the issue immediately"},
		},
	}
	httpReq := httptest.NewRequest("POST", "http://daemon/api/control/tasks?surface=inline", nil)
	call := ControlCall{Method: "POST", URL: httpReq.URL}

	_, ok := MaybeInterceptInlineTaskDefinition(httpReq, cfg, func(string, string, string) {}, func(string, ...any) {}, conversation.ProviderAnthropic, tu, call)
	if !ok {
		t.Fatal("expected task definition intercept to return true (intercepted via manual fallback)")
	}

	// Verify that a hold is created.
	holds := peekAllHolds(ctx, cache)
	if len(holds) != 1 {
		t.Fatalf("expected 1 hold to be created, got %d", len(holds))
	}

	// Verify that the drift outcome remains pending and option is new_task (not rolled back).
	stored, err := reg.Get(ctx, drift.ID)
	if err != nil {
		t.Fatalf("get drift: %v", err)
	}
	if stored.Outcome != ScopeDriftOutcomePending {
		t.Fatalf("drift Outcome = %q, want pending", stored.Outcome)
	}
	if stored.ChosenOption != ScopeDriftOptionNewTask {
		t.Fatalf("drift ChosenOption = %q, want new_task", stored.ChosenOption)
	}
}

type mockTaskRiskAssessor struct {
	verdict *TaskRiskAssessment
}

func (m *mockTaskRiskAssessor) AssessEnvelope(_ context.Context, _ TaskRiskAssessRequest) *TaskRiskAssessment {
	return m.verdict
}

type failingOutcomeRegistry struct {
	ScopeDriftRegistry
	failSetOutcome bool
}

func (r *failingOutcomeRegistry) SetOutcome(ctx context.Context, driftID string, outcome ScopeDriftOutcome) error {
	if r.failSetOutcome {
		return errors.New("database connection lost during outcome write")
	}
	return r.ScopeDriftRegistry.SetOutcome(ctx, driftID, outcome)
}

// TestScopeDriftE2E_NewTaskAutoApproveSetOutcomeFailureRollsBackDrift verifies that if task auto-approval
// triggers and task creation succeeds, but the registry write for SetOutcome(Succeeded) fails, the claim is
// rolled back and the request falls through.
func TestScopeDriftE2E_NewTaskAutoApproveSetOutcomeFailureRollsBackDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	innerReg := NewMemoryScopeDriftRegistry(0)
	reg := &failingOutcomeRegistry{ScopeDriftRegistry: innerReg, failSetOutcome: true}
	cache := NewMemoryPendingApprovalCache(time.Minute)
	drift, _ := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope)

	taskBody := &runtimetasks.TaskCreateRequest{
		Purpose:                "File the issue",
		IntentVerificationMode: "strict",
		ExpiresInSeconds:       600,
		ExpectedTools: []runtimetasks.ExpectedTool{
			{ToolName: "Bash", Why: "curl to github"},
		},
		DriftID: drift.ID,
	}
	taskBodyJSON, _ := json.Marshal(taskBody)
	tu := conversation.ToolUse{
		ID:    "tu-create",
		Name:  "Bash",
		Input: json.RawMessage(`{"body":` + string(mustJSON(string(taskBodyJSON))) + `}`),
	}

	// Creator succeeds on task creation, but SetOutcome will fail.
	fc := &fakeInlineTaskCreator{}
	assessor := &mockTaskRiskAssessor{
		verdict: &TaskRiskAssessment{
			RiskLevel:   "low",
			IntentMatch: "yes",
		},
	}
	cfg := PostprocessConfig{
		AgentContext:         AgentContext{AgentID: driftTestAgentID, AgentUserID: driftTestUserID, AgentName: "agent-drift"},
		AuditContext:         AuditContext{ConversationID: driftTestConvID},
		AuthorizationContext: AuthorizationContext{ScopeDrifts: reg},
		ApprovalContext: ApprovalContext{
			PendingApprovals:                 cache,
			InlineTaskCreator:                fc,
			TaskRiskAssessor:                 assessor,
			ConversationAutoApproveThreshold: "low",
			RecentUserTurns:                  []string{"please file the issue immediately"},
		},
	}
	httpReq := httptest.NewRequest("POST", "http://daemon/api/control/tasks?surface=inline", nil)
	call := ControlCall{Method: "POST", URL: httpReq.URL}

	_, ok := MaybeInterceptInlineTaskDefinition(httpReq, cfg, func(string, string, string) {}, func(string, ...any) {}, conversation.ProviderAnthropic, tu, call)
	if ok {
		t.Fatal("expected task definition intercept to return false (fallthrough) due to SetOutcome failure")
	}

	// Verify that the drift outcome and chosen option were rolled back (cleared).
	stored, err := reg.Get(ctx, drift.ID)
	if err != nil {
		t.Fatalf("get drift: %v", err)
	}
	if stored.Outcome != "" {
		t.Fatalf("drift Outcome = %q, want empty", stored.Outcome)
	}
	if stored.ChosenOption != "" {
		t.Fatalf("drift ChosenOption = %q, want empty", stored.ChosenOption)
	}

	// Verify we can claim the drift again (it was successfully rolled back).
	if _, err := reg.ClaimOption(ctx, drift.ID, ScopeDriftOptionNewTask, "retry"); err != nil {
		t.Fatalf("expected drift to be claimable again, but got err: %v", err)
	}
}
