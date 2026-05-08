package llmproxy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func TestMemoryPendingApprovalCacheResolveValidatesScopeAndConsumesOnce(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	ctx := context.Background()

	held, err := cache.Hold(ctx, PendingLiteApproval{
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if held.Pending.ID == "" || !strings.HasPrefix(held.Pending.ID, "cv-") {
		t.Fatalf("generated ID = %q, want cv-*", held.Pending.ID)
	}

	wrong, err := cache.Resolve(ctx, ResolveRequest{
		UserID:   "user-2",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if wrong != nil {
		t.Fatalf("wrong user resolved approval: %+v", wrong)
	}

	wrongID, err := cache.Resolve(ctx, ResolveRequest{
		UserID:     "user-1",
		AgentID:    "agent-1",
		Provider:   conversation.ProviderAnthropic,
		ApprovalID: "cv-wrongid1234",
	})
	if err != nil {
		t.Fatal(err)
	}
	if wrongID != nil {
		t.Fatalf("wrong ID resolved approval: %+v", wrongID)
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
		t.Fatalf("resolved = %+v, want %q", resolved, held.Pending.ID)
	}

	again, err := cache.Resolve(ctx, ResolveRequest{
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if again != nil {
		t.Fatalf("approval resolved twice: %+v", again)
	}
}

func TestMemoryPendingApprovalCacheHoldSupersedesSameScope(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	ctx := context.Background()

	first, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-first",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-second",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Evicted == nil || second.Evicted.ID != first.Pending.ID {
		t.Fatalf("evicted = %+v, want first", second.Evicted)
	}
	resolved, err := cache.Resolve(ctx, ResolveRequest{
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved == nil || resolved.ID != "cv-second" {
		t.Fatalf("resolved = %+v, want second", resolved)
	}
}

func TestMemoryPendingApprovalCacheExpires(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }
	ctx := context.Background()

	_, err := cache.Hold(ctx, PendingLiteApproval{
		ID:        "cv-expired",
		UserID:    "user-1",
		AgentID:   "agent-1",
		Provider:  conversation.ProviderAnthropic,
		ExpiresAt: now.Add(-time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := cache.Resolve(ctx, ResolveRequest{
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved != nil {
		t.Fatalf("expired approval resolved: %+v", resolved)
	}
}
