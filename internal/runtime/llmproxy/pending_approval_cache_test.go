package llmproxy

import (
	"context"
	"errors"
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

func TestMemoryPendingApprovalCacheResolvesMultipleSameScopeFIFO(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	ctx := context.Background()

	_, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-first",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-second",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
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
	if resolved == nil || resolved.ID != "cv-first" {
		t.Fatalf("resolved = %+v, want first", resolved)
	}
	resolved, err = cache.Resolve(ctx, ResolveRequest{
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

func TestMemoryPendingApprovalCachePeekDoesNotConsume(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	ctx := context.Background()

	if _, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-first",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
	}); err != nil {
		t.Fatal(err)
	}
	peeked, err := cache.Peek(ctx, ResolveRequest{
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if peeked == nil || peeked.ID != "cv-first" {
		t.Fatalf("peeked = %+v, want first", peeked)
	}
	resolved, err := cache.Resolve(ctx, ResolveRequest{
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved == nil || resolved.ID != "cv-first" {
		t.Fatalf("resolved after peek = %+v, want first", resolved)
	}
}

func TestMemoryPendingApprovalCacheExplicitIDResolvesMatchingPending(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	ctx := context.Background()

	for _, id := range []string{"cv-first", "cv-second"} {
		if _, err := cache.Hold(ctx, PendingLiteApproval{
			ID:       id,
			UserID:   "user-1",
			AgentID:  "agent-1",
			Provider: conversation.ProviderAnthropic,
		}); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := cache.Resolve(ctx, ResolveRequest{
		UserID:     "user-1",
		AgentID:    "agent-1",
		Provider:   conversation.ProviderAnthropic,
		ApprovalID: "cv-second",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved == nil || resolved.ID != "cv-second" {
		t.Fatalf("resolved = %+v, want second", resolved)
	}
	resolved, err = cache.Resolve(ctx, ResolveRequest{
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved == nil || resolved.ID != "cv-first" {
		t.Fatalf("resolved = %+v, want first still pending", resolved)
	}
}

func TestMemoryPendingApprovalCacheEvictsOldestOnOverflow(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	cache.max = 2
	ctx := context.Background()

	for _, id := range []string{"cv-first", "cv-second", "cv-third"} {
		held, err := cache.Hold(ctx, PendingLiteApproval{
			ID:       id,
			UserID:   "user-1",
			AgentID:  "agent-1",
			Provider: conversation.ProviderAnthropic,
		})
		if err != nil {
			t.Fatal(err)
		}
		if id == "cv-third" && (held.Evicted == nil || held.Evicted.ID != "cv-first") {
			t.Fatalf("evicted = %+v, want first", held.Evicted)
		}
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

func TestMemoryPendingApprovalCacheFailsClosedWhenIDGenerationFails(t *testing.T) {
	old := liteApprovalRandRead
	liteApprovalRandRead = func(_ []byte) (int, error) {
		return 0, errors.New("no entropy")
	}
	t.Cleanup(func() { liteApprovalRandRead = old })

	cache := NewMemoryPendingApprovalCache(time.Minute)
	_, err := cache.Hold(context.Background(), PendingLiteApproval{
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
	})
	if err == nil || !strings.Contains(err.Error(), "generate approval id") {
		t.Fatalf("Hold error = %v, want ID generation error", err)
	}
}
