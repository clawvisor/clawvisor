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
		AgentContext:    AgentContext{AgentID: agentID, AgentUserID: driftTestUserID},
		AuditContext:    AuditContext{ConversationID: convID},
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
//	1. agent POSTs /api/control/scope-drifts/<id>/one-off?surface=inline
//	   with a one-line rationale
//	2. MaybeInterceptScopeDriftOneOff claims the option + opens a
//	   StageAwaitingScopeDriftOneOff hold + substitutes the tool_result
//	   with the user-facing approval prompt
//	3. user replies "yes <approval_id>"
//	4. RewriteScopeDriftOneOffApprovalReply resolves the hold + sets
//	   the drift outcome to Succeeded + mints the pre-clear
//	5. agent's retry of the original tool_use consumes the pre-clear once
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
//	1. agent reads the menu, POSTs to .../tasks/<id>/expand?surface=inline
//	   with a `drift_id` field in the body
//	2. MaybeInterceptInlineExpansion opens a hold at
//	   StageAwaitingExpansionApproval AND records the drift link
//	3. user replies "yes" → RewriteInlineTaskApprovalReply resolves,
//	   the expansion creator is approved, AND ScopeDrifts.SetOutcome
//	   fires with Succeeded → pre-clear minted
//	4. agent's retry of the original blocked tool_use consumes the
//	   pre-clear once
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
		AgentContext:    AgentContext{AgentID: driftTestAgentID, AgentUserID: driftTestUserID},
		AuditContext:    AuditContext{ConversationID: driftTestConvID},
		ApprovalContext: ApprovalContext{PendingApprovals: cache, InlineTaskCreator: fc},
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
		AgentContext:    AgentContext{AgentID: driftTestAgentID, AgentUserID: driftTestUserID, AgentName: "agent-drift"},
		AuditContext:    AuditContext{ConversationID: driftTestConvID},
		ApprovalContext: ApprovalContext{PendingApprovals: cache, InlineTaskCreator: fc},
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
