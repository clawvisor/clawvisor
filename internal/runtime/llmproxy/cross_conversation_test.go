package llmproxy

import (
	"context"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// Holds for distinct conversation IDs must land in separate cache buckets.
// A Peek scoped to conv-b must not see conv-a's hold, and vice versa.
// Explicit-ID resolution from the wrong conversation must return nil even
// when the caller supplies the exact approval ID.
func TestPendingApprovalCache_ConversationIDIsolatesHolds(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)
	const (
		userID  = "u-isolation"
		agentID = "a-isolation"
	)

	resA, err := cache.Hold(ctx, PendingLiteApproval{
		UserID:         userID,
		AgentID:        agentID,
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-a",
		ToolUse:        conversation.ToolUse{ID: "toolu_a", Name: "Bash"},
	})
	if err != nil {
		t.Fatalf("Hold conv-a: %v", err)
	}

	resB, err := cache.Hold(ctx, PendingLiteApproval{
		UserID:         userID,
		AgentID:        agentID,
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-b",
		ToolUse:        conversation.ToolUse{ID: "toolu_b", Name: "WebFetch"},
	})
	if err != nil {
		t.Fatalf("Hold conv-b: %v", err)
	}

	peekA, err := cache.Peek(ctx, ResolveRequest{
		UserID:         userID,
		AgentID:        agentID,
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-a",
	})
	if err != nil {
		t.Fatalf("Peek conv-a: %v", err)
	}
	if peekA == nil || peekA.ToolUse.ID != "toolu_a" {
		t.Fatalf("Peek conv-a: got %v, want toolu_a", peekA)
	}

	peekB, err := cache.Peek(ctx, ResolveRequest{
		UserID:         userID,
		AgentID:        agentID,
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-b",
	})
	if err != nil {
		t.Fatalf("Peek conv-b: %v", err)
	}
	if peekB == nil || peekB.ToolUse.ID != "toolu_b" {
		t.Fatalf("Peek conv-b: got %v, want toolu_b", peekB)
	}

	// Explicit-ID resolve from conv-b targeting conv-a's approval must return nil.
	crossAB, err := cache.Resolve(ctx, ResolveRequest{
		UserID:         userID,
		AgentID:        agentID,
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-b",
		ApprovalID:     resA.Pending.ID,
	})
	if err != nil {
		t.Fatalf("cross-resolve b→a: %v", err)
	}
	if crossAB != nil {
		t.Fatalf("cross-resolve b→a: expected nil, got hold for tool %s", crossAB.ToolUse.ID)
	}

	// Explicit-ID resolve from conv-a targeting conv-b's approval must return nil.
	crossBA, err := cache.Resolve(ctx, ResolveRequest{
		UserID:         userID,
		AgentID:        agentID,
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-a",
		ApprovalID:     resB.Pending.ID,
	})
	if err != nil {
		t.Fatalf("cross-resolve a→b: %v", err)
	}
	if crossBA != nil {
		t.Fatalf("cross-resolve a→b: expected nil, got hold for tool %s", crossBA.ToolUse.ID)
	}

	// Both holds must still be intact after the failed cross-resolves.
	stillA, _ := cache.Peek(ctx, ResolveRequest{UserID: userID, AgentID: agentID, Provider: conversation.ProviderAnthropic, ConversationID: "conv-a"})
	stillB, _ := cache.Peek(ctx, ResolveRequest{UserID: userID, AgentID: agentID, Provider: conversation.ProviderAnthropic, ConversationID: "conv-b"})
	if stillA == nil {
		t.Fatal("conv-a hold was consumed by a cross-resolve from conv-b")
	}
	if stillB == nil {
		t.Fatal("conv-b hold was consumed by a cross-resolve from conv-a")
	}
}

// A bare reply (no ApprovalID) from conv-b must not release a hold that
// belongs to conv-a. The user typing "y" always responds to the most-recent
// hold in their own conversation bucket; conv-b has no holds here, so the
// bare resolve must return nil and leave conv-a's hold untouched.
func TestPendingApprovalCache_BareReplyCannotCrossConversations(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)
	const (
		userID  = "u-barereply"
		agentID = "a-barereply"
	)

	if _, err := cache.Hold(ctx, PendingLiteApproval{
		UserID:         userID,
		AgentID:        agentID,
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-a",
		ToolUse:        conversation.ToolUse{ID: "toolu_victim", Name: "Bash"},
	}); err != nil {
		t.Fatalf("Hold conv-a: %v", err)
	}

	// Bare "y" from conv-b — no ApprovalID, wrong ConversationID.
	resolved, err := cache.Resolve(ctx, ResolveRequest{
		UserID:         userID,
		AgentID:        agentID,
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-b",
		// ApprovalID intentionally empty (simulates a bare chat reply)
	})
	if err != nil {
		t.Fatalf("bare Resolve from conv-b: %v", err)
	}
	if resolved != nil {
		t.Fatalf("bare reply from conv-b resolved conv-a's hold (tool=%s); must not cross conversation boundaries",
			resolved.ToolUse.ID)
	}

	// Conv-a's hold must still be intact.
	still, err := cache.Peek(ctx, ResolveRequest{
		UserID:         userID,
		AgentID:        agentID,
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-a",
	})
	if err != nil {
		t.Fatalf("Peek conv-a after failed cross-resolve: %v", err)
	}
	if still == nil || still.ToolUse.ID != "toolu_victim" {
		t.Fatalf("conv-a hold was incorrectly consumed by bare reply from conv-b (got %v)", still)
	}
}

// Empty ConversationID falls back to the pre-scoping shared bucket so older
// clients that don't surface a conversation identifier on the wire keep working.
func TestPendingApprovalCache_EmptyConversationIDIsSharedBucket(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)
	const (
		userID  = "u-legacy"
		agentID = "a-legacy"
	)

	if _, err := cache.Hold(ctx, PendingLiteApproval{
		UserID:   userID,
		AgentID:  agentID,
		Provider: conversation.ProviderAnthropic,
		// ConversationID intentionally empty (legacy client)
		ToolUse: conversation.ToolUse{ID: "toolu_legacy", Name: "Bash"},
	}); err != nil {
		t.Fatalf("Hold (legacy): %v", err)
	}

	resolved, err := cache.Resolve(ctx, ResolveRequest{
		UserID:   userID,
		AgentID:  agentID,
		Provider: conversation.ProviderAnthropic,
		// ConversationID intentionally empty (bare reply from legacy client)
	})
	if err != nil {
		t.Fatalf("Resolve (legacy): %v", err)
	}
	if resolved == nil || resolved.ToolUse.ID != "toolu_legacy" {
		t.Fatalf("legacy bare reply must resolve the shared-bucket hold, got %v", resolved)
	}
}
