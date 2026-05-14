package llmproxy

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestTryReleasePendingApprovalWrongExplicitIDDoesNotConsume(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)
	held, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-abcdefghijklmnopqrstuvwxyz",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}

	result := TryReleasePendingApproval(ctx, ReleaseRequest{
		Provider:        conversation.ProviderAnthropic,
		Body:            []byte(`{"messages":[{"role":"user","content":"approve cv-wrongwrong12"}]}`),
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
	})
	if !result.Handled || result.HTTPStatus != 404 {
		t.Fatalf("wrong explicit ID should be handled as not found: %+v", result)
	}

	resolved, err := cache.Resolve(ctx, ResolveRequest{
		UserID:     "user-1",
		AgentID:    "agent-1",
		Provider:   conversation.ProviderAnthropic,
		ApprovalID: held.Pending.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved == nil || resolved.ID != held.Pending.ID {
		t.Fatalf("approval was consumed by wrong ID; resolved=%+v", resolved)
	}
}

func TestTryReleasePendingApprovalParsesLongExplicitID(t *testing.T) {
	verb, id := conversation.ParseApprovalReplyText("please approve\napprove cv-abcdefghijklmnopqrstuvwxyz")
	if verb != "approve" || id != "cv-abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("long approval ID did not parse: verb=%q id=%q", verb, id)
	}
	verb, id = conversation.ParseApprovalReplyText(strings.ToUpper("deny cv-abcdef123456"))
	if verb != "deny" || id != "cv-abcdef123456" {
		t.Fatalf("short approval ID compatibility broke: verb=%q id=%q", verb, id)
	}
	verb, id = conversation.ParseApprovalReplyText("task")
	if verb != "task" || id != "" {
		t.Fatalf("bare task did not parse: verb=%q id=%q", verb, id)
	}
}

// A bare reply belongs to the newest visible prompt. If a stale
// inline-task hold is still pending but a newer regular tool prompt
// was rendered afterward, release should consume the newer tool hold
// rather than fail closed just because any inline hold exists.
func TestTryReleasePendingApproval_BareReplyTargetsNewerToolHoldDespiteOlderInline(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)

	// Older inline hold: stale, but no longer the user's latest prompt.
	inlineHeld, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-inlineolderxxxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageAwaitingTaskApproval,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Newer tool-stage hold — the LIFO winner of a plain Peek.
	toolHeld, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-toolnewerxxxxxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageTool,
		ToolUse: conversation.ToolUse{
			ID:    "toolu_newer",
			Name:  "Bash",
			Input: json.RawMessage(`{"command":"echo ok"}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result := TryReleasePendingApproval(ctx, ReleaseRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            []byte(`{"messages":[{"role":"user","content":"approve"}]}`),
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
		Inspector:       inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{}),
	})
	if !result.Handled || result.Decision != "allow" || result.Outcome != "approval_released" {
		t.Fatalf("bare approve should release newest tool hold, got %+v", result)
	}
	// The stale inline hold remains for an explicit retry or TTL expiry.
	peekedInline, _ := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1",
		Provider: conversation.ProviderAnthropic, ApprovalID: inlineHeld.Pending.ID,
	})
	if peekedInline == nil {
		t.Fatal("older inline hold should not be consumed by newer tool approval")
	}
	peekedTool, _ := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1",
		Provider: conversation.ProviderAnthropic, ApprovalID: toolHeld.Pending.ID,
	})
	if peekedTool != nil {
		t.Fatalf("newer tool hold should be consumed; got %+v", peekedTool)
	}
}

// The same routing rule applies to deny: a bare deny on the newest
// regular tool prompt must deny that tool hold, not trip the stale
// inline preprocessor guard.
func TestTryReleasePendingApproval_BareDenyTargetsNewerToolHoldDespiteOlderInline(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)

	inlineHeld, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-inlineolderxxxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageAwaitingTaskApproval,
	})
	if err != nil {
		t.Fatal(err)
	}
	toolHeld, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-toolnewerxxxxxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageTool,
		ToolUse:  conversation.ToolUse{ID: "toolu_newer", Name: "Bash"},
	})
	if err != nil {
		t.Fatal(err)
	}

	result := TryReleasePendingApproval(ctx, ReleaseRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            []byte(`{"messages":[{"role":"user","content":"deny"}]}`),
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
	})
	if !result.Handled || result.Decision != "deny" || result.Outcome != "approval_denied" {
		t.Fatalf("bare deny should deny newest tool hold, got %+v", result)
	}
	if p, _ := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1",
		Provider: conversation.ProviderAnthropic, ApprovalID: inlineHeld.Pending.ID,
	}); p == nil {
		t.Fatal("older inline hold should not be consumed by newer tool denial")
	}
	if p, _ := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1",
		Provider: conversation.ProviderAnthropic, ApprovalID: toolHeld.Pending.ID,
	}); p != nil {
		t.Fatalf("newer tool hold should be consumed; got %+v", p)
	}
}

// Explicit IDs are unambiguous: even if an unrelated inline hold is
// pending, "approve <tool-id>" should release the named tool hold.
func TestTryReleasePendingApproval_ExplicitToolIDIgnoresUnrelatedInlineHold(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)

	inlineHeld, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-inlineolderxxxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageAwaitingTaskApproval,
	})
	if err != nil {
		t.Fatal(err)
	}
	toolHeld, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-toolnewerxxxxxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageTool,
		ToolUse: conversation.ToolUse{
			ID:    "toolu_newer",
			Name:  "Bash",
			Input: json.RawMessage(`{"command":"echo ok"}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result := TryReleasePendingApproval(ctx, ReleaseRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            []byte(`{"messages":[{"role":"user","content":"approve ` + toolHeld.Pending.ID + `"}]}`),
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
		Inspector:       inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{}),
	})
	if !result.Handled || result.Decision != "allow" || result.Outcome != "approval_released" {
		t.Fatalf("explicit approve should release named tool hold, got %+v", result)
	}
	if p, _ := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1",
		Provider: conversation.ProviderAnthropic, ApprovalID: inlineHeld.Pending.ID,
	}); p == nil {
		t.Fatal("unrelated inline hold should remain after explicit tool approval")
	}
	if p, _ := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1",
		Provider: conversation.ProviderAnthropic, ApprovalID: toolHeld.Pending.ID,
	}); p != nil {
		t.Fatalf("explicitly approved tool hold should be consumed; got %+v", p)
	}
}

// TOCTOU: between the release path's Peek (LIFO) and Resolve (LIFO),
// a concurrent Hold can change which hold is "newest." If Resolve
// re-runs the LIFO selection it could consume a NEWER hold than the
// one the stage guard inspected — including a newly-created inline
// hold. The fix pins Resolve to peeked.ID. Simulated here by holding
// a tool hold, peeking (handled internally by TryReleasePendingApproval),
// then racing in a new inline hold before Resolve runs. We can't
// actually inject between Peek and Resolve from a test, but we CAN
// verify the resolution is pinned by ID: after release, the
// most-recent hold should still be in the cache untouched.
func TestTryReleasePendingApproval_ResolveIsPinnedToPeekedID(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)
	// Tool hold the user is actually replying to.
	toolHeld, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-toolnowxxxxxxxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageTool,
		ToolUse: conversation.ToolUse{
			ID:    "toolu_now",
			Name:  "Bash",
			Input: json.RawMessage(`{"command":"echo ok"}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Drive release. With Resolve pinned to peeked.ID, this consumes
	// exactly the tool hold, even if other holds existed at peek time.
	result := TryReleasePendingApproval(ctx, ReleaseRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            []byte(`{"messages":[{"role":"user","content":"approve"}]}`),
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
		Inspector:       inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{}),
	})
	if !result.Handled || result.Decision != "allow" {
		t.Fatalf("release didn't allow; %+v", result)
	}
	if p, _ := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1",
		Provider: conversation.ProviderAnthropic, ApprovalID: toolHeld.Pending.ID,
	}); p != nil {
		t.Fatalf("peeked tool hold should be consumed; got %+v", p)
	}
}

// If preprocess is misconfigured and a StageAwaitingTaskApproval hold
// reaches TryReleasePendingApproval, the path fails closed (500) — but
// the hold itself must remain in the cache so a subsequent retry once
// preprocess is restored can drive the inline flow. Resolving up front
// would destroy the hold and lock the user out until TTL expiry.
func TestTryReleasePendingApproval_InlineHoldSurvivesPreprocessGuard(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)
	held, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-inlinexxxxxxxxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageAwaitingTaskApproval,
	})
	if err != nil {
		t.Fatal(err)
	}

	result := TryReleasePendingApproval(ctx, ReleaseRequest{
		Provider:        conversation.ProviderAnthropic,
		Body:            []byte(`{"messages":[{"role":"user","content":"approve"}]}`),
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
	})
	if !result.Handled || result.HTTPStatus != 500 {
		t.Fatalf("preprocess-missing guard should respond 500; got %+v", result)
	}
	if result.Outcome != "inline_task_preprocess_missing" {
		t.Fatalf("outcome = %q, want inline_task_preprocess_missing", result.Outcome)
	}

	// The hold MUST still be peekable — destroying it on the
	// fail-closed path would lock the user out of the inline flow
	// until TTL expiry, even after preprocess is restored.
	peeked, err := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1",
		Provider: conversation.ProviderAnthropic, ApprovalID: held.Pending.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if peeked == nil {
		t.Fatal("inline-task hold was destroyed by the fail-closed guard; retry path is broken")
	}
}

func TestRewriteTaskApprovalReplyRewritesAndDropsHold(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)
	held, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-abcdefghijklmnopqrstuvwxyz",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		ToolUse: conversation.ToolUse{
			ID:    "toolu_1",
			Name:  "Bash",
			Input: json.RawMessage(`{"command":"ls /tmp/ | grep -i greet"}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := RewriteTaskApprovalReply(ctx, TaskReplyRewriteRequest{
		Provider:        conversation.ProviderAnthropic,
		Body:            []byte(`{"messages":[{"role":"user","content":"task"}]}`),
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Rewritten {
		t.Fatalf("task reply result = %+v", result)
	}
	if !strings.Contains(string(result.Body), "https://clawvisor.local/control/tasks") ||
		!strings.Contains(string(result.Body), "ls /tmp/ | grep -i greet") {
		t.Fatalf("task guidance missing expected content: %s", result.Body)
	}

	// Hold must be dropped — there's no way back to approving the
	// original tool, so leaving it in the cache risks an orphan
	// being resolved later by a bare "approve" on something else.
	resolved, err := cache.Resolve(ctx, ResolveRequest{
		UserID:     "user-1",
		AgentID:    "agent-1",
		Provider:   conversation.ProviderAnthropic,
		ApprovalID: held.Pending.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved != nil {
		t.Fatalf("task reply must drop the hold; got resolved=%+v", resolved)
	}
}
