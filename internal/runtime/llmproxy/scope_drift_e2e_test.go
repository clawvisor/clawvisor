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

// ── Menu self-claim regression ───────────────────────────────────────────────────

// TestScopeDriftE2E_MenuIsNotSelfClaiming guards a sharp-edge bug: the
// rendered menu includes an example <clawvisor:decision> block as
// documentation. The rewriter splices the menu into the response body
// in place of the blocked tool_use, and applyScopeDriftDecisions then
// scans the same body for markup. Without a code-fence guard, the
// parser sees the example markup, claims option=one_off against the
// real drift_id rendered into the example, opens a hold, and splices
// in a user-facing one-off approval prompt — all before the agent has
// chosen anything. The one-shot cap is consumed and the agent's actual
// choice is locked out.
//
// This test renders the menu, feeds it through ApplyScopeDriftDecisions
// (mirroring the postproc pipeline), and asserts:
//
//   1. The body is unchanged — the parser refused to match the example.
//   2. The drift's ChosenOption is still empty — no claim happened.
//   3. No pending-approval hold was opened.
func TestScopeDriftE2E_MenuIsNotSelfClaiming(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0)
	cache := NewMemoryPendingApprovalCache(time.Minute)

	drift, _ := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope)
	menuText, _, err := BuildScopeDriftContinuation(ctx, reg, ScopeDrift{
		// Use a fresh template so BuildScopeDriftContinuation gets a
		// distinct drift to render. The fixture drift above is the one
		// the agent would have at block time; we just need any menu
		// text for the regression check.
		UserID:         driftTestUserID,
		AgentID:        driftTestAgentID,
		ConversationID: driftTestConvID,
		ToolUse:        drift.ToolUse,
		Service:        drift.Service,
		Action:         drift.Action,
		Host:           drift.Host,
		Method:         drift.Method,
		Path:           drift.Path,
		Source:         ScopeDriftSourceTaskScope,
		ReasonText:     drift.ReasonText,
	}, "https://clawvisor.local")
	if err != nil {
		t.Fatalf("BuildScopeDriftContinuation: %v", err)
	}
	if !strings.Contains(menuText, "<clawvisor:decision") {
		t.Fatalf("menu must include an example <clawvisor:decision> block; got: %s", menuText)
	}

	// Wrap the menu in a JSON-shaped body the way the rewriter would
	// have spliced it into an assistant text block. This is the shape
	// applyScopeDriftDecisions actually sees in production.
	body := []byte(`{"content":[{"type":"text","text":` + string(mustJSON(menuText)) + `}]}`)
	rc := ScopeDriftResolveContext{
		AgentContext:     AgentContext{AgentID: driftTestAgentID, AgentUserID: driftTestUserID},
		ConversationID:   driftTestConvID,
		Registry:         reg,
		PendingApprovals: cache,
	}
	out, changed := ApplyScopeDriftDecisions(ctx, rc, conversation.ProviderAnthropic, body)
	if changed {
		t.Fatalf("menu's example markup was matched and substituted; body diff:\nbefore: %s\nafter:  %s", body, out)
	}

	// All drifts must remain unclaimed — the menu rendered TWO drifts
	// in this test (the fixture + the BuildScopeDriftContinuation), and
	// neither should have a chosen option.
	got, _ := reg.Get(ctx, drift.ID)
	if got.ChosenOption != "" {
		t.Fatalf("fixture drift was self-claimed; ChosenOption=%q", got.ChosenOption)
	}
	if holds := peekAllHolds(ctx, cache); len(holds) != 0 {
		t.Fatalf("menu self-claim opened a hold; want 0, got %d", len(holds))
	}
}

// ── (c) One-off: approve path ───────────────────────────────────────────────────

// TestScopeDriftE2E_OneOffApprove walks the markup → user-approve →
// pre-clear path:
//
//	1. agent emits <clawvisor:decision option="one-off"> in assistant text
//	2. ApplyScopeDriftDecisions claims the option + opens a
//	   StageAwaitingScopeDriftOneOff hold + substitutes the markup with
//	   the user-facing approval prompt
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

	// Step 1 + 2: the agent emits markup; the postproc resolver claims
	// and substitutes. Wrap the markup in a JSON string field to match
	// how an Anthropic assistant turn would carry it on the wire (the
	// resolver's substitution targets JSON-escaped content).
	body := []byte(`{"content":[{"type":"text","text":"I'll request a one-off. <clawvisor:decision drift=\"` + drift.ID + `\" option=\"one-off\">need this single call to file the issue.</clawvisor:decision>"}]}`)
	rc := ScopeDriftResolveContext{
		AgentContext:     AgentContext{AgentID: driftTestAgentID, AgentUserID: driftTestUserID},
		ConversationID:   driftTestConvID,
		Registry:         reg,
		PendingApprovals: cache,
	}
	out, changed := ApplyScopeDriftDecisions(ctx, rc, conversation.ProviderAnthropic, body)
	if !changed {
		t.Fatal("ApplyScopeDriftDecisions: expected the markup to be substituted")
	}
	if strings.Contains(string(out), "<clawvisor:decision") {
		t.Fatalf("markup was not stripped from body: %s", out)
	}
	if !strings.Contains(string(out), "Reply `yes` or `y`") {
		t.Fatalf("expected user-facing approval prompt in body: %s", out)
	}

	// Step 3: the resolver should have opened exactly one hold at the
	// scope-drift one-off stage.
	holds := peekAllHolds(ctx, cache)
	if len(holds) != 1 {
		t.Fatalf("want 1 hold after resolve, got %d", len(holds))
	}
	if holds[0].Stage != StageAwaitingScopeDriftOneOff {
		t.Fatalf("hold stage = %q, want %q", holds[0].Stage, StageAwaitingScopeDriftOneOff)
	}
	if holds[0].ScopeDriftID != drift.ID {
		t.Fatalf("hold ScopeDriftID = %q, want %q", holds[0].ScopeDriftID, drift.ID)
	}
	approvalID := holds[0].ID

	// Confirm the registry recorded the claim.
	claimed, _ := reg.Get(ctx, drift.ID)
	if claimed.ChosenOption != ScopeDriftOptionOneOff || claimed.Outcome != ScopeDriftOutcomePending {
		t.Fatalf("registry state after claim: %+v", claimed)
	}

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
	body := []byte(`{"content":[{"type":"text","text":"<clawvisor:decision drift=\"` + drift.ID + `\" option=\"one-off\">need it once</clawvisor:decision>"}]}`)
	rc := ScopeDriftResolveContext{
		AgentContext:     AgentContext{AgentID: driftTestAgentID, AgentUserID: driftTestUserID},
		ConversationID:   driftTestConvID,
		Registry:         reg,
		PendingApprovals: cache,
	}
	if _, changed := ApplyScopeDriftDecisions(ctx, rc, conversation.ProviderAnthropic, body); !changed {
		t.Fatal("expected markup to be substituted")
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

// TestScopeDriftE2E_OneShotCap confirms a second <clawvisor:decision>
// markup against the same drift_id is rejected with an "already
// resolved" status, and the original claim's hold is unaffected.
func TestScopeDriftE2E_OneShotCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0)
	cache := NewMemoryPendingApprovalCache(time.Minute)
	drift, _ := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope)

	rc := ScopeDriftResolveContext{
		AgentContext:     AgentContext{AgentID: driftTestAgentID, AgentUserID: driftTestUserID},
		ConversationID:   driftTestConvID,
		Registry:         reg,
		PendingApprovals: cache,
	}
	first := []byte(`{"content":[{"type":"text","text":"<clawvisor:decision drift=\"` + drift.ID + `\" option=\"one-off\">first try</clawvisor:decision>"}]}`)
	if _, changed := ApplyScopeDriftDecisions(ctx, rc, conversation.ProviderAnthropic, first); !changed {
		t.Fatal("first claim: expected substitution")
	}
	// Second markup against the same drift_id. Use a fresh body so the
	// offsets are independent.
	second := []byte(`{"content":[{"type":"text","text":"<clawvisor:decision drift=\"` + drift.ID + `\" option=\"one-off\">second try</clawvisor:decision>"}]}`)
	out, changed := ApplyScopeDriftDecisions(ctx, rc, conversation.ProviderAnthropic, second)
	if !changed {
		t.Fatal("second claim: expected substitution (with rejection status)")
	}
	if !strings.Contains(string(out), "already resolved") {
		t.Fatalf("second claim should report 'already resolved'; body: %s", out)
	}
	// The first claim's hold is the only one in the cache; the second
	// claim must not have opened a new hold.
	holds := peekAllHolds(ctx, cache)
	if len(holds) != 1 {
		t.Fatalf("want exactly 1 hold (first claim's), got %d", len(holds))
	}
}

// ── Cross-conversation guard ────────────────────────────────────────────────────

// TestScopeDriftE2E_CrossConversationGuard confirms a markup carrying a
// drift_id minted in a different conversation is rejected without
// claiming the drift. This is the guard that stops a stale assistant
// transcript copied across sessions from consuming approvals in the
// wrong session.
func TestScopeDriftE2E_CrossConversationGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0)
	cache := NewMemoryPendingApprovalCache(time.Minute)
	drift, _ := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope) // minted in conv-drift-1

	// Resolver invoked from a DIFFERENT conversation.
	rc := ScopeDriftResolveContext{
		AgentContext:     AgentContext{AgentID: driftTestAgentID, AgentUserID: driftTestUserID},
		ConversationID:   "conv-other",
		Registry:         reg,
		PendingApprovals: cache,
	}
	body := []byte(`{"content":[{"type":"text","text":"<clawvisor:decision drift=\"` + drift.ID + `\" option=\"one-off\">x</clawvisor:decision>"}]}`)
	out, changed := ApplyScopeDriftDecisions(ctx, rc, conversation.ProviderAnthropic, body)
	if !changed {
		t.Fatal("expected substitution (with rejection status)")
	}
	if !strings.Contains(string(out), "belongs to a different conversation") {
		t.Fatalf("expected cross-conversation rejection; body: %s", out)
	}
	// The drift must still be UNCLAIMED so the rightful conversation
	// can resolve it.
	got, _ := reg.Get(ctx, drift.ID)
	if got.ChosenOption != "" {
		t.Fatalf("cross-conversation refusal must not claim; got ChosenOption=%q", got.ChosenOption)
	}
	if len(peekAllHolds(ctx, cache)) != 0 {
		t.Fatal("cross-conversation refusal must not open a hold")
	}
}

// ── Cross-agent guard ───────────────────────────────────────────────────────────

// TestScopeDriftE2E_CrossAgentGuard mirrors the cross-conversation
// test for the agent_id mismatch case.
func TestScopeDriftE2E_CrossAgentGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := NewMemoryScopeDriftRegistry(0)
	cache := NewMemoryPendingApprovalCache(time.Minute)
	drift, _ := mintDriftFixture(t, reg, ScopeDriftSourceTaskScope)

	rc := ScopeDriftResolveContext{
		AgentContext:     AgentContext{AgentID: "agent-other", AgentUserID: driftTestUserID},
		ConversationID:   driftTestConvID,
		Registry:         reg,
		PendingApprovals: cache,
	}
	body := []byte(`{"content":[{"type":"text","text":"<clawvisor:decision drift=\"` + drift.ID + `\" option=\"one-off\">x</clawvisor:decision>"}]}`)
	out, changed := ApplyScopeDriftDecisions(ctx, rc, conversation.ProviderAnthropic, body)
	if !changed {
		t.Fatal("expected substitution (with rejection status)")
	}
	if !strings.Contains(string(out), "minted for a different agent") {
		t.Fatalf("expected cross-agent rejection; body: %s", out)
	}
	got, _ := reg.Get(ctx, drift.ID)
	if got.ChosenOption != "" {
		t.Fatalf("cross-agent refusal must not claim; got %q", got.ChosenOption)
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
