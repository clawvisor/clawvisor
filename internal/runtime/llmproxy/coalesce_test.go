package llmproxy

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// Mixed-turn coalescence: bash that would auto-allow alongside a
// WebFetch flagged for review. The whole turn is held under one
// coalesced approval; the bash sibling is tagged HeldKindAllow on the
// hold, and the rendered prompt mentions both calls.
func TestPostprocess_CoalescesMixedAllowAndApproval(t *testing.T) {
	body := []byte(`{
		"id":"msg_1",
		"type":"message",
		"role":"assistant",
		"model":"claude-haiku-4-5",
		"content":[
			{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{"command":"ls -la"}},
			{"type":"tool_use","id":"toolu_fetch","name":"WebFetch","input":{"url":"https://example.com/x"}}
		],
		"stop_reason":"tool_use"
	}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	cache := NewMemoryPendingApprovalCache(time.Minute)

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		PendingApprovals: cache,
		ResponseRegistry: conversation.DefaultResponseRegistry(),
		CandidateTasks:   []*store.Task{},
		ToolRules: []*store.RuntimePolicyRule{
			{ID: "review-webfetch", UserID: userID, AgentID: &agentID, Kind: "tool", Action: "review", ToolName: "WebFetch", Reason: "review web fetch", Enabled: true},
		},
		EgressRules: []*store.RuntimePolicyRule{},
	})
	if !got.Rewritten {
		t.Fatalf("expected coalesced approval rewrite, got: %s", got.Body)
	}
	out := string(got.Body)
	if !strings.Contains(out, "Clawvisor paused this turn for approval (2 tool calls).") {
		t.Fatalf("expected coalesced header, got: %s", out)
	}
	if !strings.Contains(out, "`Bash`") || !strings.Contains(out, "`WebFetch`") {
		t.Fatalf("coalesced prompt should mention both tools, got: %s", out)
	}
	if !strings.Contains(out, "held alongside") {
		t.Fatalf("expected the bash sibling to be labeled 'held alongside', got: %s", out)
	}
	if strings.Contains(out, "Tool 'WebFetch' was blocked by Clawvisor policy") {
		t.Fatalf("expected coalesced sibling WebFetch block message to be suppressed, but got it in output: %s", out)
	}

	holds := cache.snapshotHoldsForTest(userID, agentID, conversation.ProviderAnthropic)
	if len(holds) != 1 {
		t.Fatalf("expected exactly one coalesced hold, got %d", len(holds))
	}
	hold := holds[0]
	all := hold.AllHolds()
	if len(all) != 2 {
		t.Fatalf("expected 2 held tool_uses, got %d", len(all))
	}
	// AllHolds() returns held tool_uses in the model's original turn
	// order — Bash first (auto-allow), then WebFetch (approval
	// trigger). The primary slot tracks the approval-needing
	// WebFetch internally (Inspector/Fingerprint/Reason point at it),
	// but the visible order matches the model's emit order so the
	// released call sequence is deterministic for dependent calls.
	if all[0].ToolUse.ID != "toolu_bash" || all[0].Kind != HeldKindAllow {
		t.Fatalf("expected Bash first (turn order) as HeldKindAllow, got %+v", all[0])
	}
	if all[1].ToolUse.ID != "toolu_fetch" || all[1].Kind != HeldKindApproval {
		t.Fatalf("expected WebFetch second (turn order) as HeldKindApproval, got %+v", all[1])
	}
	if hold.ToolUse.ID != "toolu_fetch" {
		t.Fatalf("primary slot should be the approval-needing WebFetch, got %s", hold.ToolUse.ID)
	}
	if hold.PrimaryIndex != 1 {
		t.Fatalf("PrimaryIndex = %d, want 1 (WebFetch was second in the turn)", hold.PrimaryIndex)
	}
}

// A turn with a single tool_use needing approval is NOT coalesced —
// coalescing one element into one element is a no-op, and the legacy
// per-tool prompt (with the "task" verb) stays available.
func TestPostprocess_SingleApprovalKeepsLegacyPromptShape(t *testing.T) {
	body := []byte(`{
		"id":"msg_1",
		"type":"message",
		"role":"assistant",
		"model":"claude-haiku-4-5",
		"content":[
			{"type":"tool_use","id":"toolu_only","name":"WebFetch","input":{"url":"https://example.com/x"}}
		],
		"stop_reason":"tool_use"
	}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	cache := NewMemoryPendingApprovalCache(time.Minute)

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		PendingApprovals: cache,
		ResponseRegistry: conversation.DefaultResponseRegistry(),
		CandidateTasks:   []*store.Task{},
		ToolRules: []*store.RuntimePolicyRule{
			{ID: "review-webfetch", UserID: userID, AgentID: &agentID, Kind: "tool", Action: "review", ToolName: "WebFetch", Reason: "review web fetch", Enabled: true},
		},
		EgressRules: []*store.RuntimePolicyRule{},
	})
	out := string(got.Body)
	if !strings.Contains(out, "Clawvisor paused this tool call for approval.") {
		t.Fatalf("single-tool turn should use legacy prompt, got: %s", out)
	}
	if !strings.Contains(out, "or `task` to instruct") {
		t.Fatalf("single-tool prompt should retain the 'task' verb, got: %s", out)
	}
	holds := cache.snapshotHoldsForTest(userID, agentID, conversation.ProviderAnthropic)
	if len(holds) != 1 || holds[0].IsCoalesced() {
		t.Fatalf("single-tool turn should produce one non-coalesced hold; got %+v", holds)
	}
}

func TestShouldCoalesceTurn(t *testing.T) {
	cases := []struct {
		name string
		in   []evalCapture
		want bool
	}{
		{
			name: "empty",
			in:   nil,
			want: false,
		},
		{
			name: "single approval",
			in:   []evalCapture{{Kind: HeldKindApproval}},
			want: false,
		},
		{
			name: "two approvals",
			in:   []evalCapture{{Kind: HeldKindApproval}, {Kind: HeldKindApproval}},
			want: true,
		},
		{
			name: "approval + auto-allow sibling",
			in:   []evalCapture{{Kind: HeldKindAllow}, {Kind: HeldKindApproval}},
			want: true,
		},
		{
			name: "all auto-allow — nothing to coalesce",
			in:   []evalCapture{{Kind: HeldKindAllow}, {Kind: HeldKindAllow}},
			want: false,
		},
		{
			name: "hard deny in turn — fall back to legacy",
			in:   []evalCapture{{Kind: HeldKindApproval}, {Kind: HeldKindDeny}},
			want: false,
		},
		{
			name: "inline-task stage skips coalesce",
			in:   []evalCapture{{Kind: HeldKindApproval, Stage: StageAwaitingTaskApproval}, {Kind: HeldKindAllow}},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldCoalesceTurn(tc.in); got != tc.want {
				t.Fatalf("shouldCoalesceTurn(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestClassifyVerdict(t *testing.T) {
	cases := []struct {
		name string
		v    conversation.ToolUseVerdict
		want HeldToolUseKind
	}{
		{"allowed pass-through", conversation.ToolUseVerdict{Allowed: true}, HeldKindAllow},
		{"allowed with rewrite", conversation.ToolUseVerdict{Allowed: true, RewriteInput: json.RawMessage(`{"x":1}`)}, HeldKindRewrite},
		{"approval-required", conversation.ToolUseVerdict{Allowed: false, Reason: "Clawvisor: approval required — review"}, HeldKindApproval},
		{"inline-task awaiting", conversation.ToolUseVerdict{Allowed: false, Reason: "Clawvisor: awaiting inline task approval"}, HeldKindApproval},
		{"hard deny", conversation.ToolUseVerdict{Allowed: false, Reason: "Clawvisor: ambiguous credentialed call refused — bad shape"}, HeldKindDeny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyVerdict(tc.v); got != tc.want {
				t.Fatalf("classifyVerdict(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// Coalesced-hold release: a single user "yes" produces a synthetic
// response carrying every approved tool_use. The harness then executes
// them all from one user gesture.
func TestTryReleasePendingApproval_CoalescedAllowEmitsAllCalls(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)

	primary := conversation.ToolUse{ID: "toolu_a", Name: "Bash", Input: json.RawMessage(`{"command":"echo a"}`)}
	sibling := conversation.ToolUse{ID: "toolu_b", Name: "Bash", Input: json.RawMessage(`{"command":"echo b"}`)}
	held, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-coalescereleasexxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageTool,
		ToolUse:  primary,
		Reason:   "test coalesced release",
		Additional: []HeldToolUse{{
			ToolUse: sibling,
			Kind:    HeldKindAllow,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	result := TryReleasePendingApproval(ctx, ReleaseRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            []byte(`{"messages":[{"role":"user","content":"approve ` + held.Pending.ID + `"}]}`),
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
		Inspector:       inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{}),
	})
	if !result.Handled || result.Decision != "allow" || result.Outcome != "approval_released" {
		t.Fatalf("coalesced approve should release, got %+v", result)
	}
	bodyStr := string(result.Body)
	if !strings.Contains(bodyStr, `"id":"toolu_a"`) || !strings.Contains(bodyStr, `"id":"toolu_b"`) {
		t.Fatalf("coalesced release response must carry every approved tool_use; got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, `"name":"Bash"`) {
		t.Fatalf("coalesced release response missing tool name; got: %s", bodyStr)
	}
}

// Coalesced-hold release deny: a single "no" produces a single text
// block; no tool_use blocks at all (no half-execution).
func TestTryReleasePendingApproval_CoalescedDenyProducesTextOnly(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)

	held, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-coalescedenyxxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageTool,
		ToolUse:  conversation.ToolUse{ID: "toolu_a", Name: "Bash", Input: json.RawMessage(`{"command":"echo a"}`)},
		Additional: []HeldToolUse{{
			ToolUse: conversation.ToolUse{ID: "toolu_b", Name: "Bash", Input: json.RawMessage(`{"command":"echo b"}`)},
			Kind:    HeldKindAllow,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	result := TryReleasePendingApproval(ctx, ReleaseRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            []byte(`{"messages":[{"role":"user","content":"deny ` + held.Pending.ID + `"}]}`),
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
		Inspector:       inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{}),
	})
	if !result.Handled || result.Decision != "deny" || result.Outcome != "approval_denied" {
		t.Fatalf("coalesced deny should be handled and denied, got %+v", result)
	}
	bodyStr := string(result.Body)
	if strings.Contains(bodyStr, `"type":"tool_use"`) {
		t.Fatalf("denied coalesced release should carry no tool_use blocks; got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, conversation.ApprovalDeniedMessage) {
		t.Fatalf("denied response should carry the canonical deny message; got: %s", bodyStr)
	}
}

// Regression: when the approval-needing tool_use is NOT the first in
// the turn, coalescence must still surface every held call in the
// original model-emitted order on release. Reordering breaks dependent
// sequences (e.g. a Bash that produces output a following WebFetch
// consumes). Setup: an auto-allow Bash followed by a review-flagged
// WebFetch. The primary slot stores WebFetch (the approval trigger);
// PrimaryIndex records its original position so AllHolds() emits
// [Bash, WebFetch] and the multi-tool release synthesis preserves
// that order.
func TestPostprocess_CoalescedReleasePreservesTurnOrder(t *testing.T) {
	ctx := context.Background()
	body := []byte(`{
		"id":"msg_1",
		"type":"message",
		"role":"assistant",
		"model":"claude-haiku-4-5",
		"content":[
			{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{"command":"ls -la"}},
			{"type":"tool_use","id":"toolu_fetch","name":"WebFetch","input":{"url":"https://example.com/x"}}
		],
		"stop_reason":"tool_use"
	}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	cache := NewMemoryPendingApprovalCache(time.Minute)

	_ = Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		PendingApprovals: cache,
		ResponseRegistry: conversation.DefaultResponseRegistry(),
		CandidateTasks:   []*store.Task{},
		ToolRules: []*store.RuntimePolicyRule{
			{ID: "review-webfetch", UserID: userID, AgentID: &agentID, Kind: "tool", Action: "review", ToolName: "WebFetch", Reason: "review web fetch", Enabled: true},
		},
		EgressRules: []*store.RuntimePolicyRule{},
	})
	holds := cache.snapshotHoldsForTest(userID, agentID, conversation.ProviderAnthropic)
	if len(holds) != 1 {
		t.Fatalf("want one coalesced hold, got %d", len(holds))
	}
	hold := holds[0]
	all := hold.AllHolds()
	if got := []string{all[0].ToolUse.ID, all[1].ToolUse.ID}; got[0] != "toolu_bash" || got[1] != "toolu_fetch" {
		t.Fatalf("AllHolds order = %v, want [toolu_bash toolu_fetch] to match the model's turn order", got)
	}
	// The release synthesis pulls from AllHolds() too; confirm the
	// wire-level call order matches as well.
	result := TryReleasePendingApproval(ctx, ReleaseRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            []byte(`{"messages":[{"role":"user","content":"approve ` + hold.ID + `"}]}`),
		Agent:           &store.Agent{ID: agentID, UserID: userID},
		PendingApproval: cache,
		Inspector:       insp,
		Store:           st,
		RewriteOpts:     inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces:    NewMemoryCallerNonceCache(time.Minute),
	})
	if !result.Handled || result.Decision != "allow" {
		t.Fatalf("coalesced release should allow, got %+v", result)
	}
	bashAt := strings.Index(string(result.Body), `"id":"toolu_bash"`)
	fetchAt := strings.Index(string(result.Body), `"id":"toolu_fetch"`)
	if bashAt < 0 || fetchAt < 0 {
		t.Fatalf("release body missing held tool_use IDs: %s", result.Body)
	}
	if bashAt > fetchAt {
		t.Fatalf("release body reordered tool_uses: bash@%d fetch@%d (want bash first)", bashAt, fetchAt)
	}
}

// Regression: when the coalesced Hold itself fails (Redis down, ID
// gen panic), Postprocess must fall back to writing the per-tool
// holds via legacy replay so the first-pass body's prompts resolve
// to real cache entries. With pass-1 buffering the coalesced Hold is
// the FIRST call against the underlying cache; failure triggers
// legacy replay of the buffered per-tool holds.
func TestPostprocess_CoalescedHoldFailureFallsBackToPerToolHolds(t *testing.T) {
	body := []byte(`{
		"id":"msg_1",
		"type":"message",
		"role":"assistant",
		"model":"claude-haiku-4-5",
		"content":[
			{"type":"tool_use","id":"toolu_1","name":"WebFetch","input":{"url":"https://example.com/one"}},
			{"type":"tool_use","id":"toolu_2","name":"WebFetch","input":{"url":"https://example.com/two"}}
		],
		"stop_reason":"tool_use"
	}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	inner := NewMemoryPendingApprovalCache(time.Minute)
	// failFirst=1 → the coalesced Hold (first call) fails; subsequent
	// replay calls succeed. With pass-1 buffering this is the right
	// shape to exercise the fallback path.
	cache := &flakyHoldCache{inner: inner, failFirst: 1}

	_ = Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		PendingApprovals: cache,
		ResponseRegistry: conversation.DefaultResponseRegistry(),
		CandidateTasks:   []*store.Task{},
		ToolRules: []*store.RuntimePolicyRule{
			{ID: "review-webfetch", UserID: userID, AgentID: &agentID, Kind: "tool", Action: "review", ToolName: "WebFetch", Reason: "review web fetch", Enabled: true},
		},
		EgressRules: []*store.RuntimePolicyRule{},
	})

	// The coalesced Hold failed; legacy replay then wrote both
	// per-tool holds. Final occupancy: 2.
	got := inner.snapshotHoldsForTest(userID, agentID, conversation.ProviderAnthropic)
	if len(got) != 2 {
		t.Fatalf("expected 2 per-tool holds after coalesced fallback; got %d: %+v", len(got), got)
	}
	if got[0].IsCoalesced() || got[1].IsCoalesced() {
		t.Fatalf("fallback holds must be the per-tool singletons, not coalesced: %+v", got)
	}
}

// Regression for #2: coalescing must not evict unrelated older
// approvals when the underlying cache is near capacity. With pass-1
// buffering the per-tool holds are never inserted into the cache, so
// only the coalesced hold competes for an LRU slot — net occupancy
// rises by exactly one regardless of how many tool_uses the turn
// carries.
func TestPostprocess_CoalesceDoesNotEvictUnrelatedApprovals(t *testing.T) {
	ctx := context.Background()
	// Cache max=10. Seed with 9 unrelated approvals so a single new
	// coalesced hold lands at capacity without eviction; the
	// pre-buffering passthrough design would have hit max via
	// per-tool inserts and evicted one of the seeded entries.
	cache := NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	seedIDs := make([]string, 0, 9)
	for i := 0; i < 9; i++ {
		// 26-char suffix to satisfy approvalReplyRE if anyone
		// resolves these by ID later (not used here, but defensive).
		id := "cv-seedapprovalxxxxxxxxxxxx"[:len("cv-")] + padTo26(t, "seed"+stringFromInt(t, i))
		res, err := cache.Hold(ctx, PendingLiteApproval{
			ID:       id,
			UserID:   userID,
			AgentID:  agentID,
			Provider: conversation.ProviderAnthropic,
			Stage:    StageTool,
			ToolUse:  conversation.ToolUse{ID: "seed_" + stringFromInt(t, i), Name: "Bash"},
		})
		if err != nil {
			t.Fatalf("seed hold %d: %v", i, err)
		}
		seedIDs = append(seedIDs, res.Pending.ID)
	}

	body := []byte(`{
		"id":"msg_1",
		"type":"message",
		"role":"assistant",
		"model":"claude-haiku-4-5",
		"content":[
			{"type":"tool_use","id":"toolu_1","name":"WebFetch","input":{"url":"https://example.com/one"}},
			{"type":"tool_use","id":"toolu_2","name":"WebFetch","input":{"url":"https://example.com/two"}}
		],
		"stop_reason":"tool_use"
	}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})

	_ = Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		PendingApprovals: cache,
		ResponseRegistry: conversation.DefaultResponseRegistry(),
		CandidateTasks:   []*store.Task{},
		ToolRules: []*store.RuntimePolicyRule{
			{ID: "review-webfetch", UserID: userID, AgentID: &agentID, Kind: "tool", Action: "review", ToolName: "WebFetch", Reason: "review web fetch", Enabled: true},
		},
		EgressRules: []*store.RuntimePolicyRule{},
	})

	// All 9 seed approvals must still be present; only the one
	// coalesced hold was added, putting total at 10 (== max).
	final := cache.snapshotHoldsForTest(userID, agentID, conversation.ProviderAnthropic)
	if len(final) != 10 {
		t.Fatalf("expected 10 final holds (9 seed + 1 coalesced); got %d", len(final))
	}
	present := map[string]bool{}
	for _, h := range final {
		present[h.ID] = true
	}
	for _, id := range seedIDs {
		if !present[id] {
			t.Fatalf("unrelated seed approval %s was evicted by coalescence", id)
		}
	}
}

// Regression: the coalesced Hold itself can evict an older inline-task
// hold. When that happens, the evicted hold's PendingTaskID must be
// expired so the dashboard stops showing an approval that no longer has
// a cache anchor for chat resolution.
func TestPostprocess_CoalescedHoldEvictionExpiresInlineTask(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)
	cache.max = 1
	st, userID, agentID := seedPostprocessStore(t, "placeholder_github_coalesce")

	if _, err := cache.Hold(ctx, PendingLiteApproval{
		ID:            "cv-inlineevictedxxxxxxxxxx",
		UserID:        userID,
		AgentID:       agentID,
		Provider:      conversation.ProviderAnthropic,
		Stage:         StageAwaitingTaskApproval,
		PendingTaskID: "task-coalesced-evicted",
		ToolUse:       conversation.ToolUse{ID: "toolu_inline", Name: "Bash"},
	}); err != nil {
		t.Fatalf("seed inline hold: %v", err)
	}

	body := []byte(`{
		"id":"msg_1",
		"type":"message",
		"role":"assistant",
		"model":"claude-haiku-4-5",
		"content":[
			{"type":"tool_use","id":"toolu_1","name":"WebFetch","input":{"url":"https://example.com/one"}},
			{"type":"tool_use","id":"toolu_2","name":"WebFetch","input":{"url":"https://example.com/two"}}
		],
		"stop_reason":"tool_use"
	}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	creator := &capturingInlineCreator{}

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		PendingApprovals: cache,
		ResponseRegistry: conversation.DefaultResponseRegistry(),
		CandidateTasks:   []*store.Task{},
		ToolRules: []*store.RuntimePolicyRule{
			{ID: "review-webfetch", UserID: userID, AgentID: &agentID, Kind: "tool", Action: "review", ToolName: "WebFetch", Reason: "review web fetch", Enabled: true},
		},
		EgressRules:       []*store.RuntimePolicyRule{},
		InlineTaskCreator: creator,
	})
	if !got.Rewritten {
		t.Fatalf("expected coalesced approval rewrite")
	}
	if !creator.expireCalled {
		t.Fatalf("ExpireInlineTask was not invoked for coalesced Hold eviction")
	}
	if len(creator.expiredIDs) != 1 || creator.expiredIDs[0] != "task-coalesced-evicted" {
		t.Fatalf("expired ids = %v, want [task-coalesced-evicted]", creator.expiredIDs)
	}
}

// Regression for #1: when coalescence kicks in, the audit trail must
// describe each held tool_use as "coalesced_approval_pending" — not
// as "allow" or "rewrite" that would falsely imply the call ran.
func TestPostprocess_CoalesceAuditDoesNotEmitMisleadingAllow(t *testing.T) {
	body := []byte(`{
		"id":"msg_1",
		"type":"message",
		"role":"assistant",
		"model":"claude-haiku-4-5",
		"content":[
			{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{"command":"ls -la"}},
			{"type":"tool_use","id":"toolu_fetch","name":"WebFetch","input":{"url":"https://example.com/x"}}
		],
		"stop_reason":"tool_use"
	}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	cache := NewMemoryPendingApprovalCache(time.Minute)
	emitter := NewAuditEmitter(st, nil, nil)

	_ = Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		PendingApprovals: cache,
		ResponseRegistry: conversation.DefaultResponseRegistry(),
		CandidateTasks:   []*store.Task{},
		ToolRules: []*store.RuntimePolicyRule{
			{ID: "review-webfetch", UserID: userID, AgentID: &agentID, Kind: "tool", Action: "review", ToolName: "WebFetch", Reason: "review web fetch", Enabled: true},
		},
		EgressRules: []*store.RuntimePolicyRule{},
		Audit:       emitter,
	})

	// Pull every tool_use inspection row back; in coalesce mode the
	// surviving row(s) must be "block / coalesced_approval_pending".
	// The misuse this guards against is a sibling that would have
	// auto-allowed leaving a stale "allow / pass_through" row in the
	// audit trail even though its call never executed. Runtime tool-use
	// rows use child-level dedup so sibling tool_use IDs can persist
	// under the same parent request; what we assert here is that every
	// persisted inspection row describes the coalesced state, never the
	// buffered allow/rewrite that would have been wrong.
	rows, _, err := st.ListAuditEntries(context.Background(), userID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	inspectionRows := 0
	for _, r := range rows {
		if !strings.HasPrefix(r.Action, "lite_proxy.tool_use.") {
			continue
		}
		inspectionRows++
		if r.Action != "lite_proxy.tool_use.block" {
			t.Fatalf("audit action = %q; coalesce mode must report block (not allow/rewrite): %+v", r.Action, r)
		}
		if r.Outcome != "coalesced_approval_pending" {
			t.Fatalf("audit outcome = %q; coalesce mode must report coalesced_approval_pending: %+v", r.Outcome, r)
		}
	}
	if inspectionRows == 0 {
		t.Fatalf("expected at least one tool_use inspection row, got none")
	}
}

// Regression: the persisted coalesced-pending audit detail must not
// lose the tool_use that actually triggered approval. Runtime tool-use rows
// use child-level dedup keys, so held siblings remain queryable under the
// original parent request.
func TestPostprocess_CoalescePendingAuditSurfacesApprovalTrigger(t *testing.T) {
	requestID := "req-coalesced-pending-trigger"
	body := []byte(`{
		"id":"msg_1",
		"type":"message",
		"role":"assistant",
		"model":"claude-haiku-4-5",
		"content":[
			{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{"command":"ls -la"}},
			{"type":"tool_use","id":"toolu_fetch","name":"WebFetch","input":{"url":"https://example.com/x"}}
		],
		"stop_reason":"tool_use"
	}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	cache := NewMemoryPendingApprovalCache(time.Minute)
	emitter := NewAuditEmitter(st, nil, nil)

	_ = Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		RequestID:        requestID,
		PendingApprovals: cache,
		ResponseRegistry: conversation.DefaultResponseRegistry(),
		CandidateTasks:   []*store.Task{},
		ToolRules: []*store.RuntimePolicyRule{
			{ID: "review-webfetch", UserID: userID, AgentID: &agentID, Kind: "tool", Action: "review", ToolName: "WebFetch", Reason: "review web fetch", Enabled: true},
		},
		EgressRules: []*store.RuntimePolicyRule{},
		Audit:       emitter,
	})

	rows, _, err := st.ListAuditEntries(context.Background(), userID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	foundPending := false
	foundApprovalTrigger := false
	var persisted []map[string]any
	for _, r := range rows {
		if r.RequestID != requestID || r.Outcome != "coalesced_approval_pending" {
			continue
		}
		foundPending = true
		var params map[string]any
		if err := json.Unmarshal(r.ParamsSafe, &params); err != nil {
			t.Fatalf("params unmarshal: %v", err)
		}
		persisted = append(persisted, params)
		if params["tool_name"] == "WebFetch" || params["tool_use_id"] == "toolu_fetch" {
			foundApprovalTrigger = true
		}
		if held, ok := params["held_tools"].([]any); ok {
			for _, item := range held {
				m, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if m["tool_name"] == "WebFetch" || m["tool_use_id"] == "toolu_fetch" {
					foundApprovalTrigger = true
				}
			}
		}
	}
	if !foundPending {
		t.Fatalf("expected a coalesced_approval_pending audit row for %s", requestID)
	}
	if !foundApprovalTrigger {
		t.Fatalf("coalesced pending audit lost approval-triggering WebFetch/toolu_fetch; persisted params: %+v", persisted)
	}
}

// Regression: legacy-path replay must fail closed when the underlying
// cache rejects the Hold. The first-pass body references approval
// prompts; if their per-tool holds can't be committed, the body
// would invite the user to type "yes" at a prompt that resolves to
// nothing. SkippedReason makes the handler emit 502 — the same
// shape the pre-buffering eval used to take when Hold failed inline.
func TestPostprocess_LegacyReplayFailureFailsClosed(t *testing.T) {
	body := []byte(`{
		"id":"msg_1",
		"type":"message",
		"role":"assistant",
		"model":"claude-haiku-4-5",
		"content":[
			{"type":"tool_use","id":"toolu_only","name":"WebFetch","input":{"url":"https://example.com/x"}}
		],
		"stop_reason":"tool_use"
	}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	// failFirst=999 → every Hold call fails. Single tool_use → no
	// coalescence; the legacy-replay path is the one exercised.
	inner := NewMemoryPendingApprovalCache(time.Minute)
	cache := &flakyHoldCache{inner: inner, failFirst: 999}

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		PendingApprovals: cache,
		ResponseRegistry: conversation.DefaultResponseRegistry(),
		CandidateTasks:   []*store.Task{},
		ToolRules: []*store.RuntimePolicyRule{
			{ID: "review-webfetch", UserID: userID, AgentID: &agentID, Kind: "tool", Action: "review", ToolName: "WebFetch", Reason: "review web fetch", Enabled: true},
		},
		EgressRules: []*store.RuntimePolicyRule{},
	})
	if got.Body != nil {
		t.Fatalf("replay failure must drop the body (handler emits 502); got %d bytes", len(got.Body))
	}
	if got.SkippedReason == "" || !strings.Contains(got.SkippedReason, "approval hold storage failed") {
		t.Fatalf("replay failure should surface SkippedReason naming hold storage; got %q", got.SkippedReason)
	}
}

// Regression: when a multi-hold replay fails partway through, the
// committed holds must be dropped so the cache doesn't carry an
// approval the response body never described. Tested directly
// against replayBufferedHolds because constructing this scenario
// through Postprocess proper would require a multi-hold turn that
// also skips coalescence — a configuration the production rule
// engine doesn't naturally produce.
func TestReplayBufferedHolds_PartialFailureRollsBackCommitted(t *testing.T) {
	ctx := context.Background()
	inner := NewMemoryPendingApprovalCache(time.Minute)
	// Three buffered holds; the second one's Hold call fails. The
	// first should be committed then dropped on the failure path;
	// the third should never be attempted.
	cache := &flakyHoldFromCallN{inner: inner, failOnCall: 2}
	sink := &capturedHoldSink{
		holds: []capturedHold{
			{Pending: PendingLiteApproval{UserID: "u", AgentID: "a", Provider: conversation.ProviderAnthropic, ToolUse: conversation.ToolUse{ID: "t1", Name: "Bash"}}},
			{Pending: PendingLiteApproval{UserID: "u", AgentID: "a", Provider: conversation.ProviderAnthropic, ToolUse: conversation.ToolUse{ID: "t2", Name: "Bash"}}},
			{Pending: PendingLiteApproval{UserID: "u", AgentID: "a", Provider: conversation.ProviderAnthropic, ToolUse: conversation.ToolUse{ID: "t3", Name: "Bash"}}},
		},
	}
	captures := []evalCapture{{Use: sink.holds[0].Pending.ToolUse}, {Use: sink.holds[1].Pending.ToolUse}, {Use: sink.holds[2].Pending.ToolUse}}

	err := replayBufferedHolds(ctx, PostprocessConfig{}, cache, sink, nil, captures)
	if err == nil {
		t.Fatalf("expected non-nil error from partial replay failure")
	}
	final := inner.snapshotHoldsForTest("u", "a", conversation.ProviderAnthropic)
	if len(final) != 0 {
		t.Fatalf("partial replay failure must roll back committed holds; got %d remaining: %+v", len(final), final)
	}
	if cache.calls != 2 {
		t.Fatalf("expected replay to stop after the first failure (2 Hold attempts); got %d", cache.calls)
	}
}

func TestHoldCapturingApprovalCacheBuffersThenReplayCommitsOnce(t *testing.T) {
	ctx := context.Background()
	inner := NewMemoryPendingApprovalCache(time.Minute)
	counting := &countingHoldCache{inner: inner}
	sink := &capturedHoldSink{}
	cache := newHoldCapturingApprovalCache(counting, sink)

	pending := PendingLiteApproval{
		UserID:   "u",
		AgentID:  "a",
		Provider: conversation.ProviderAnthropic,
		ToolUse:  conversation.ToolUse{ID: "toolu_1", Name: "Bash"},
	}
	if _, err := cache.Hold(ctx, pending); err != nil {
		t.Fatalf("capturing Hold: %v", err)
	}
	if counting.holdCalls != 0 {
		t.Fatalf("capturing cache must not commit immediately, got %d inner Hold calls", counting.holdCalls)
	}
	if len(sink.holds) != 1 {
		t.Fatalf("expected one buffered hold, got %d", len(sink.holds))
	}
	if err := replayBufferedHolds(ctx, PostprocessConfig{}, counting, sink, nil, []evalCapture{{Use: pending.ToolUse}}); err != nil {
		t.Fatalf("replayBufferedHolds: %v", err)
	}
	if counting.holdCalls != 1 {
		t.Fatalf("replay must commit exactly once, got %d inner Hold calls", counting.holdCalls)
	}
	final := inner.snapshotHoldsForTest("u", "a", conversation.ProviderAnthropic)
	if len(final) != 1 {
		t.Fatalf("expected one committed hold, got %d: %+v", len(final), final)
	}
}

// flakyHoldFromCallN fails the N-th Hold call only and otherwise
// passes through. Lets tests target a specific replay step.
type flakyHoldFromCallN struct {
	inner      PendingApprovalCache
	failOnCall int
	calls      int
}

func (c *flakyHoldFromCallN) Hold(ctx context.Context, p PendingLiteApproval) (HoldResult, error) {
	c.calls++
	if c.calls == c.failOnCall {
		return HoldResult{}, errFlakyHold
	}
	return c.inner.Hold(ctx, p)
}
func (c *flakyHoldFromCallN) Peek(ctx context.Context, r ResolveRequest) (*PendingLiteApproval, error) {
	return c.inner.Peek(ctx, r)
}
func (c *flakyHoldFromCallN) Resolve(ctx context.Context, r ResolveRequest) (*PendingLiteApproval, error) {
	return c.inner.Resolve(ctx, r)
}
func (c *flakyHoldFromCallN) Drop(ctx context.Context, r ResolveRequest) error {
	return c.inner.Drop(ctx, r)
}

type countingHoldCache struct {
	inner     PendingApprovalCache
	holdCalls int
}

func (c *countingHoldCache) Hold(ctx context.Context, p PendingLiteApproval) (HoldResult, error) {
	c.holdCalls++
	return c.inner.Hold(ctx, p)
}
func (c *countingHoldCache) Peek(ctx context.Context, r ResolveRequest) (*PendingLiteApproval, error) {
	return c.inner.Peek(ctx, r)
}
func (c *countingHoldCache) Resolve(ctx context.Context, r ResolveRequest) (*PendingLiteApproval, error) {
	return c.inner.Resolve(ctx, r)
}
func (c *countingHoldCache) Drop(ctx context.Context, r ResolveRequest) error {
	return c.inner.Drop(ctx, r)
}

// Regression: the buffering wrapper must NOT fabricate
// CreatedAt/ExpiresAt. The real cache configured with a non-default
// TTL must drive how long holds live; injecting a 10-minute default
// in the wrapper would bypass that configuration.
func TestPostprocess_BufferedHoldRespectsConfiguredCacheTTL(t *testing.T) {
	body := []byte(`{
		"id":"msg_1",
		"type":"message",
		"role":"assistant",
		"model":"claude-haiku-4-5",
		"content":[
			{"type":"tool_use","id":"toolu_only","name":"WebFetch","input":{"url":"https://example.com/x"}}
		],
		"stop_reason":"tool_use"
	}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	// Configure the cache with a 30-second TTL. The wrapper used to
	// default ExpiresAt = now + 10m; with that bug the persisted
	// hold would expire 9.5 minutes later than the configured TTL.
	cache := NewMemoryPendingApprovalCache(30 * time.Second)

	_ = Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		PendingApprovals: cache,
		ResponseRegistry: conversation.DefaultResponseRegistry(),
		CandidateTasks:   []*store.Task{},
		ToolRules: []*store.RuntimePolicyRule{
			{ID: "review-webfetch", UserID: userID, AgentID: &agentID, Kind: "tool", Action: "review", ToolName: "WebFetch", Reason: "review web fetch", Enabled: true},
		},
		EgressRules: []*store.RuntimePolicyRule{},
	})

	holds := cache.snapshotHoldsForTest(userID, agentID, conversation.ProviderAnthropic)
	if len(holds) != 1 {
		t.Fatalf("expected one hold, got %d", len(holds))
	}
	lived := holds[0].ExpiresAt.Sub(holds[0].CreatedAt)
	// Allow 1s slack for clock between buffering and replay.
	if lived < 29*time.Second || lived > 31*time.Second {
		t.Fatalf("hold lifetime = %s, want ~30s (the configured cache TTL); wrapper-default would have given 10m", lived)
	}
}

// Regression: a coalesced sibling that was auto-allowed (no per-tool
// Hold was created for it) must still carry its inspector verdict in
// the Additional entry. Without this, the release audit's
// target_host/method/path are empty for that sibling — it executes
// after approval but the audit trail can't say where it went.
func TestPostprocess_CoalescedSiblingCarriesInspectorMetadata(t *testing.T) {
	body := []byte(`{
		"id":"msg_1",
		"type":"message",
		"role":"assistant",
		"model":"claude-haiku-4-5",
		"content":[
			{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{"command":"ls -la"}},
			{"type":"tool_use","id":"toolu_fetch","name":"WebFetch","input":{"url":"https://example.com/x"}}
		],
		"stop_reason":"tool_use"
	}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	cache := NewMemoryPendingApprovalCache(time.Minute)

	_ = Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		PendingApprovals: cache,
		ResponseRegistry: conversation.DefaultResponseRegistry(),
		CandidateTasks:   []*store.Task{},
		ToolRules: []*store.RuntimePolicyRule{
			{ID: "review-webfetch", UserID: userID, AgentID: &agentID, Kind: "tool", Action: "review", ToolName: "WebFetch", Reason: "review web fetch", Enabled: true},
		},
		EgressRules: []*store.RuntimePolicyRule{},
	})
	holds := cache.snapshotHoldsForTest(userID, agentID, conversation.ProviderAnthropic)
	if len(holds) != 1 {
		t.Fatalf("want one coalesced hold, got %d", len(holds))
	}
	all := holds[0].AllHolds()
	if len(all) != 2 {
		t.Fatalf("expected 2 held tool_uses, got %d", len(all))
	}
	// Find the auto-allowed bash sibling.
	var bash HeldToolUse
	for _, h := range all {
		if h.Kind == HeldKindAllow {
			bash = h
			break
		}
	}
	if bash.ToolUse.ID != "toolu_bash" {
		t.Fatalf("expected auto-allow sibling to be the bash call, got %+v", bash)
	}
	// The inspector verdict on the auto-allow sibling must be the
	// real one from innerEval — not the zero value. SourceTriggerMiss
	// is what the inspector reports for a bash with no credential
	// placeholder. Reason should be non-empty for the same reason.
	if bash.Inspector.Source == "" {
		t.Fatalf("auto-allow sibling lost inspector verdict: %+v", bash.Inspector)
	}
}

// Regression: LogApprovalRelease emits one row per release event with
// per-tool detail under params.held_tools. Keeping that grouped shape
// avoids forcing dashboards to reconstruct one approval decision from
// several child audit rows.
func TestAuditEmitter_LogApprovalRelease_CoalescedEmitsOneRowWithPerToolDetail(t *testing.T) {
	ctx := context.Background()
	st, agent := newAuditTestStore(t)
	em := NewAuditEmitter(st, nil, nil)

	pending := &PendingLiteApproval{
		ID:       "cv-coalescedauditxxxxxxxxxx",
		UserID:   agent.UserID,
		AgentID:  agent.ID,
		Provider: conversation.ProviderAnthropic,
		ToolUse:  conversation.ToolUse{ID: "toolu_primary", Name: "WebFetch"},
		Inspector: inspector.Verdict{
			Host: "api.example.com", Method: "GET", Path: "/v1/x",
			Source: inspector.SourceDeterministic,
		},
		Reason: "review web fetch",
		Additional: []HeldToolUse{{
			ToolUse:   conversation.ToolUse{ID: "toolu_sibling", Name: "Bash"},
			Kind:      HeldKindAllow,
			Inspector: inspector.Verdict{Source: inspector.SourceTriggerMiss},
		}},
		PrimaryIndex: 1, // primary sits second in turn order
	}

	em.LogApprovalRelease(ctx, agent, "req-coalesce", pending, "allow", "approval_released", "approved")

	rows, _, err := st.ListAuditEntries(ctx, agent.UserID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	releaseRows := 0
	var got *store.AuditEntry
	for _, r := range rows {
		if r.Action == "lite_proxy.approval.release" {
			releaseRows++
			got = r
		}
	}
	if releaseRows != 1 {
		t.Fatalf("expected exactly one grouped release row; got %d", releaseRows)
	}
	var params map[string]any
	if err := json.Unmarshal(got.ParamsSafe, &params); err != nil {
		t.Fatalf("params unmarshal: %v", err)
	}
	if got, want := params["coalesced"], true; got != want {
		t.Fatalf("coalesced field = %v, want %v", got, want)
	}
	if got, want := params["hold_size"], float64(2); got != want {
		t.Fatalf("hold_size = %v, want %v", got, want)
	}
	held, ok := params["held_tools"].([]any)
	if !ok {
		t.Fatalf("held_tools missing or wrong type: %T = %v", params["held_tools"], params["held_tools"])
	}
	if len(held) != 2 {
		t.Fatalf("held_tools count = %d, want 2 (every held tool surfaced under one row)", len(held))
	}
	// Each entry must have tool_use_id and held_kind so dashboards
	// can expand the per-call view from one row.
	for i, e := range held {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("held_tools[%d] not an object: %T", i, e)
		}
		if m["tool_use_id"] == "" || m["held_kind"] == "" {
			t.Fatalf("held_tools[%d] missing tool_use_id/held_kind: %+v", i, m)
		}
	}
}

// Regression: when the user types "task" against a coalesced hold,
// the rewritten user turn (which the model sees on its next request)
// must enumerate EVERY held tool's name in expected_tools — not
// just the primary. Without this, the generated task scope covers
// only one of the held calls and the sibling reviewed calls re-prompt
// on retry, defeating the gesture.
func TestStartInlineTaskDefinition_CoalescedHoldCoversEveryHeldTool(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)
	held, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-coalescetaskxxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageTool,
		ToolUse:  conversation.ToolUse{ID: "toolu_fetch", Name: "WebFetch", Input: json.RawMessage(`{"url":"https://example.com/x"}`)},
		Additional: []HeldToolUse{{
			ToolUse: conversation.ToolUse{ID: "toolu_bash", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)},
			Kind:    HeldKindAllow,
		}},
		PrimaryIndex: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := RewriteTaskApprovalReply(ctx, TaskReplyRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            []byte(`{"messages":[{"role":"user","content":"task ` + held.Pending.ID + `"}]}`),
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Rewritten {
		t.Fatalf("expected task-reply rewrite to fire on coalesced hold")
	}
	rewritten := string(out.Body)
	// The prompt is embedded as a JSON string inside the Anthropic
	// request body so double-quotes are escaped; match the escaped
	// form to avoid coupling to the body's outer encoding.
	for _, name := range []string{`\"tool_name\": \"WebFetch\"`, `\"tool_name\": \"Bash\"`} {
		if !strings.Contains(rewritten, name) {
			t.Fatalf("rewritten user turn must enumerate every held tool's name; missing %s\nbody: %s", name, rewritten)
		}
	}
}

// flakyHoldCache fails the first failFirst Hold calls then passes
// through. Used to simulate a transient backend failure on the
// coalesced Hold (first call under pass-1 buffering) while letting
// the legacy-replay fallback succeed.
type flakyHoldCache struct {
	inner     PendingApprovalCache
	failFirst int
	calls     int
}

func (c *flakyHoldCache) Hold(ctx context.Context, p PendingLiteApproval) (HoldResult, error) {
	c.calls++
	if c.calls <= c.failFirst {
		return HoldResult{}, errFlakyHold
	}
	return c.inner.Hold(ctx, p)
}
func (c *flakyHoldCache) Peek(ctx context.Context, r ResolveRequest) (*PendingLiteApproval, error) {
	return c.inner.Peek(ctx, r)
}
func (c *flakyHoldCache) Resolve(ctx context.Context, r ResolveRequest) (*PendingLiteApproval, error) {
	return c.inner.Resolve(ctx, r)
}
func (c *flakyHoldCache) Drop(ctx context.Context, r ResolveRequest) error {
	return c.inner.Drop(ctx, r)
}

var errFlakyHold = errFlakyHoldType{}

type errFlakyHoldType struct{}

func (errFlakyHoldType) Error() string { return "flaky cache: hold refused" }

func padTo26(t *testing.T, s string) string {
	t.Helper()
	for len(s) < 26 {
		s += "x"
	}
	if len(s) > 26 {
		s = s[:26]
	}
	return s
}

func stringFromInt(t *testing.T, n int) string {
	t.Helper()
	return string(rune('0' + n))
}

// SyntheticApprovalToolUsesResponseWithDenyMessage with a 1-element
// slice must preserve the legacy single-call response semantics.
// Guards downstream callers that haven't migrated without coupling the
// test to byte-identical JSON rendering.
func TestSyntheticApprovalToolUses_SingletonMatchesLegacy(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	reqBody := []byte(`{"stream":false}`)
	legacy, ok := conversation.SyntheticApprovalToolUseResponseWithDenyMessage(
		req, conversation.ProviderAnthropic, reqBody, true,
		"toolu_x", "Bash", map[string]any{"command": "echo ok"},
		conversation.ApprovalDeniedMessage,
	)
	if !ok {
		t.Fatal("legacy synth refused")
	}
	multi, ok := conversation.SyntheticApprovalToolUsesResponseWithDenyMessage(
		req, conversation.ProviderAnthropic, reqBody, true,
		[]conversation.SyntheticToolCall{{ID: "toolu_x", Name: "Bash", Input: map[string]any{"command": "echo ok"}}},
		conversation.ApprovalDeniedMessage,
	)
	if !ok {
		t.Fatal("multi synth refused")
	}
	if legacy.ContentType != multi.ContentType {
		t.Fatalf("singleton content-type mismatch: legacy=%q multi=%q", legacy.ContentType, multi.ContentType)
	}
	var legacyJSON any
	if err := json.Unmarshal(legacy.Body, &legacyJSON); err != nil {
		t.Fatalf("legacy body is not JSON: %v\n%s", err, legacy.Body)
	}
	var multiJSON any
	if err := json.Unmarshal(multi.Body, &multiJSON); err != nil {
		t.Fatalf("multi body is not JSON: %v\n%s", err, multi.Body)
	}
	if !reflect.DeepEqual(legacyJSON, multiJSON) {
		t.Fatalf("singleton multi-synth diverges semantically from legacy:\nlegacy=%s\nmulti =%s", legacy.Body, multi.Body)
	}
}
