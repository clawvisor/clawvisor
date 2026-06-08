package postproc

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// Postprocess must copy cfg.AuditContext.ConversationID onto the coalesced
// hold so it lands in the correct per-conversation bucket. This is the
// regression introduced by #439 (coalesceFromCaptures does not touch
// ConversationID) and fixed by #441.
//
// The test verifies:
//   - the hold is retrievable via the correct ConversationID
//   - the hold is NOT visible from a different conversation's bucket
//   - the legacy (empty-ID) bucket is empty — the hold didn't fall through
func TestPostprocess_CoalescedHoldInheritsConversationID(t *testing.T) {
	const convID = "conv-test-abc"
	body := []byte(`{
		"id":"msg_1",
		"type":"message",
		"role":"assistant",
		"model":"claude-haiku-4-5",
		"content":[
			{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{"command":"ls"}},
			{"type":"tool_use","id":"toolu_fetch","name":"WebFetch","input":{"url":"https://example.com/x"}}
		],
		"stop_reason":"tool_use"
	}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Minute)

	result := Postprocess(req, body, "application/json", llmproxy.PostprocessConfig{
		ToolUseEvaluatorFactory: pipelineFactory,
		AgentContext: llmproxy.AgentContext{
			AgentUserID: userID,
			AgentID:     agentID,
		},
		AuditContext: llmproxy.AuditContext{
			ConversationID: convID,
		},
		AuthorizationContext: llmproxy.AuthorizationContext{
			CandidateTasks: []*store.Task{},
			ToolRules: []*store.RuntimePolicyRule{
				{ID: "review-webfetch", UserID: userID, AgentID: &agentID, Kind: "tool", Action: "review", ToolName: "WebFetch", Reason: "review web fetch", Enabled: true},
			},
			EgressRules: []*store.RuntimePolicyRule{},
		},
		ApprovalContext: llmproxy.ApprovalContext{
			PendingApprovals: cache,
		},
		RewriteContext: llmproxy.RewriteContext{
			Inspector:    insp,
			RewriteOpts:  inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
			CallerNonces: llmproxy.NewMemoryCallerNonceCache(time.Minute),
			Store:        st,
		},
		RoutingContext: llmproxy.RoutingContext{
			ResponseRegistry: conversation.DefaultResponseRegistry(),
		},
	})
	if !result.Rewritten {
		t.Fatalf("expected coalesced rewrite, got skipped: %s", result.SkippedReason)
	}

	ctx := context.Background()

	// Hold must be retrievable from the correct conversation bucket.
	hold, err := cache.Peek(ctx, llmproxy.ResolveRequest{
		UserID:         userID,
		AgentID:        agentID,
		Provider:       conversation.ProviderAnthropic,
		ConversationID: convID,
	})
	if err != nil {
		t.Fatalf("Peek with ConversationID=%q: %v", convID, err)
	}
	if hold == nil {
		t.Fatalf("coalesced hold not found under ConversationID %q — "+
			"AuditContext.ConversationID was not propagated to coalesced.ConversationID (regression from #439)", convID)
	}
	if hold.ConversationID != convID {
		t.Fatalf("hold.ConversationID = %q, want %q", hold.ConversationID, convID)
	}

	// Hold must NOT be visible from an unrelated conversation.
	wrong, err := cache.Peek(ctx, llmproxy.ResolveRequest{
		UserID:         userID,
		AgentID:        agentID,
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-other",
	})
	if err != nil {
		t.Fatalf("Peek wrong conversation: %v", err)
	}
	if wrong != nil {
		t.Fatalf("coalesced hold leaked into unrelated conversation bucket (tool=%s)", wrong.ToolUse.ID)
	}

	// Hold must NOT have fallen into the legacy (empty-ConversationID) shared bucket.
	legacy := cache.SnapshotHoldsForTest(userID, agentID, conversation.ProviderAnthropic)
	if len(legacy) != 0 {
		t.Fatalf("coalesced hold unexpectedly landed in legacy empty-ConversationID bucket (%d holds)", len(legacy))
	}
}

// Adversarial end-to-end: conversation A produces a coalesced hold for two
// WebFetch calls. A bare "y" from conversation B (which has no pending holds)
// must return nil, and an explicit resolve of A's hold ID from B must also
// return nil. Conv-A's hold must survive both spoofing attempts and be
// resolvable only by conversation A itself.
func TestPostprocess_ConcurrentConversationsCannotSpoof(t *testing.T) {
	twoWebFetchBody := func(id1, id2, url1, url2 string) []byte {
		return []byte(`{
			"id":"msg_x",
			"type":"message",
			"role":"assistant",
			"model":"claude-haiku-4-5",
			"content":[
				{"type":"tool_use","id":"` + id1 + `","name":"WebFetch","input":{"url":"` + url1 + `"}},
				{"type":"tool_use","id":"` + id2 + `","name":"WebFetch","input":{"url":"` + url2 + `"}}
			],
			"stop_reason":"tool_use"
		}`)
	}

	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Minute)
	rules := []*store.RuntimePolicyRule{
		{ID: "review-webfetch", UserID: userID, AgentID: &agentID, Kind: "tool", Action: "review", ToolName: "WebFetch", Reason: "review web fetch", Enabled: true},
	}

	cfgFor := func(convID string) llmproxy.PostprocessConfig {
		return llmproxy.PostprocessConfig{
			ToolUseEvaluatorFactory: pipelineFactory,
			AgentContext: llmproxy.AgentContext{
				AgentUserID: userID,
				AgentID:     agentID,
			},
			AuditContext: llmproxy.AuditContext{
				ConversationID: convID,
			},
			AuthorizationContext: llmproxy.AuthorizationContext{
				CandidateTasks: []*store.Task{},
				ToolRules:      rules,
				EgressRules:    []*store.RuntimePolicyRule{},
			},
			ApprovalContext: llmproxy.ApprovalContext{
				PendingApprovals: cache,
			},
			RewriteContext: llmproxy.RewriteContext{
				Inspector:    insp,
				RewriteOpts:  inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
				CallerNonces: llmproxy.NewMemoryCallerNonceCache(time.Minute),
				Store:        st,
			},
			RoutingContext: llmproxy.RoutingContext{
				ResponseRegistry: conversation.DefaultResponseRegistry(),
			},
		}
	}

	// Conversation A produces a coalesced hold.
	reqA := httptest.NewRequest("POST", "/v1/messages", nil)
	resultA := Postprocess(reqA,
		twoWebFetchBody("toolu_a1", "toolu_a2", "https://example.com/a1", "https://example.com/a2"),
		"application/json", cfgFor("conv-a"))
	if !resultA.Rewritten {
		t.Fatalf("conv-a: expected coalesced rewrite, got skipped: %s", resultA.SkippedReason)
	}

	ctx := context.Background()

	holdA, err := cache.Peek(ctx, llmproxy.ResolveRequest{
		UserID:         userID,
		AgentID:        agentID,
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-a",
	})
	if err != nil || holdA == nil {
		t.Fatalf("conv-a hold not found after Postprocess: err=%v hold=%v", err, holdA)
	}

	// Spoofing attempt 1: bare "y" from conv-b (conv-b has no pending holds).
	// Must return nil; must not consume conv-a's hold.
	bareSpoof, err := cache.Resolve(ctx, llmproxy.ResolveRequest{
		UserID:         userID,
		AgentID:        agentID,
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-b",
		// ApprovalID empty — simulates a bare "y" in the conv-b chat window
	})
	if err != nil {
		t.Fatalf("bare spoof attempt: %v", err)
	}
	if bareSpoof != nil {
		t.Fatalf("bare reply from conv-b resolved a hold (tool=%s, id=%s); expected nil",
			bareSpoof.ToolUse.ID, bareSpoof.ID)
	}

	// Spoofing attempt 2: explicit ID targeting conv-a's hold, issued from conv-b.
	explicitSpoof, err := cache.Resolve(ctx, llmproxy.ResolveRequest{
		UserID:         userID,
		AgentID:        agentID,
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-b",
		ApprovalID:     holdA.ID,
	})
	if err != nil {
		t.Fatalf("explicit-ID spoof attempt: %v", err)
	}
	if explicitSpoof != nil {
		t.Fatalf("explicit ApprovalID from conv-b resolved conv-a's hold (tool=%s, id=%s); must not cross conversation boundaries",
			explicitSpoof.ToolUse.ID, explicitSpoof.ID)
	}

	// Conv-a's hold must be intact after both spoofing attempts.
	stillA, err := cache.Peek(ctx, llmproxy.ResolveRequest{
		UserID:         userID,
		AgentID:        agentID,
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-a",
	})
	if err != nil {
		t.Fatalf("Peek conv-a after spoof attempts: %v", err)
	}
	if stillA == nil {
		t.Fatal("conv-a hold was consumed by a spoofed resolution from conv-b")
	}

	// Conv-a can resolve its own hold.
	selfResolved, err := cache.Resolve(ctx, llmproxy.ResolveRequest{
		UserID:         userID,
		AgentID:        agentID,
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-a",
	})
	if err != nil {
		t.Fatalf("conv-a self-resolve: %v", err)
	}
	if selfResolved == nil {
		t.Fatal("conv-a could not resolve its own hold")
	}
}
