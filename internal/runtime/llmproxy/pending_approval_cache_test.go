package llmproxy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
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

// Bare "approve" (no ApprovalID) resolves the MOST RECENT hold first,
// not the oldest. The user is replying to the most recent approval
// prompt the harness rendered; resolving the oldest hold first was
// the cause of "I approved but nothing happened" — older unresolved
// holds shadowed newer ones.
func TestMemoryPendingApprovalCacheResolvesMultipleSameScopeLIFO(t *testing.T) {
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
	if resolved == nil || resolved.ID != "cv-second" {
		t.Fatalf("first bare resolve = %+v, want most-recent (cv-second)", resolved)
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
		t.Fatalf("second bare resolve = %+v, want older (cv-first) after newer consumed", resolved)
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
	// Eviction dropped cv-first (oldest); cv-second and cv-third
	// remain. Bare resolve picks the most recent (cv-third) under
	// the LIFO default.
	if resolved == nil || resolved.ID != "cv-third" {
		t.Fatalf("resolved = %+v, want cv-third (most recent surviving)", resolved)
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

func TestMemoryPendingApprovalCacheUpdateTransitionsStage(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	ctx := context.Background()

	if _, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-tool",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
	}); err != nil {
		t.Fatal(err)
	}

	updated, err := cache.Update(ctx, UpdateRequest{
		UserID:     "user-1",
		AgentID:    "agent-1",
		Provider:   conversation.ProviderAnthropic,
		ApprovalID: "cv-tool",
		Stage:      StageAwaitingTaskDefinition,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated == nil || updated.Stage != StageAwaitingTaskDefinition {
		t.Fatalf("updated = %+v, want stage=awaiting_task_definition", updated)
	}

	peeked, err := cache.Peek(ctx, ResolveRequest{
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if peeked == nil || peeked.Stage != StageAwaitingTaskDefinition {
		t.Fatalf("peeked = %+v, want stage transition persisted", peeked)
	}
}

func TestMemoryPendingApprovalCacheUpdateRefreshesExpiry(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	start := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return start }
	ctx := context.Background()

	held, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-stale",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	original := held.Pending.ExpiresAt

	fresh := original.Add(5 * time.Minute)
	if _, err := cache.Update(ctx, UpdateRequest{
		UserID:     "user-1",
		AgentID:    "agent-1",
		Provider:   conversation.ProviderAnthropic,
		ApprovalID: "cv-stale",
		ExpiresAt:  fresh,
	}); err != nil {
		t.Fatal(err)
	}

	peeked, err := cache.Peek(ctx, ResolveRequest{
		UserID:     "user-1",
		AgentID:    "agent-1",
		Provider:   conversation.ProviderAnthropic,
		ApprovalID: "cv-stale",
	})
	if err != nil {
		t.Fatal(err)
	}
	if peeked == nil || !peeked.ExpiresAt.Equal(fresh) {
		t.Fatalf("peeked.ExpiresAt = %v, want %v", peeked.ExpiresAt, fresh)
	}
}

func TestMemoryPendingApprovalCacheUpdateReturnsNilForUnknownID(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	ctx := context.Background()
	updated, err := cache.Update(ctx, UpdateRequest{
		UserID:     "user-1",
		AgentID:    "agent-1",
		Provider:   conversation.ProviderAnthropic,
		ApprovalID: "cv-missing",
		Stage:      StageAwaitingTaskDefinition,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated != nil {
		t.Fatalf("update on missing returned %+v, want nil", updated)
	}
}

func TestPendingLiteApprovalCarriesNewFields(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	ctx := context.Background()

	taskDef := &runtimetasks.TaskCreateRequest{
		Purpose:                "Build a landing page",
		IntentVerificationMode: "strict",
	}
	held, err := cache.Hold(ctx, PendingLiteApproval{
		ID:              "cv-inner",
		UserID:          "user-1",
		AgentID:         "agent-1",
		Provider:        conversation.ProviderAnthropic,
		Stage:           StageAwaitingTaskApproval,
		AwaitingTaskFor: "cv-outer",
		TaskDefinition:  taskDef,
	})
	if err != nil {
		t.Fatal(err)
	}
	if held.Pending.Stage != StageAwaitingTaskApproval {
		t.Fatalf("held.Stage = %q, want awaiting_task_approval", held.Pending.Stage)
	}
	if held.Pending.AwaitingTaskFor != "cv-outer" {
		t.Fatalf("held.AwaitingTaskFor = %q, want cv-outer", held.Pending.AwaitingTaskFor)
	}
	if held.Pending.TaskDefinition == nil || held.Pending.TaskDefinition.Purpose != taskDef.Purpose {
		t.Fatalf("held.TaskDefinition = %+v, want round-trip", held.Pending.TaskDefinition)
	}
	resolved, err := cache.Resolve(ctx, ResolveRequest{
		UserID:     "user-1",
		AgentID:    "agent-1",
		Provider:   conversation.ProviderAnthropic,
		ApprovalID: "cv-inner",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved == nil ||
		resolved.Stage != StageAwaitingTaskApproval ||
		resolved.AwaitingTaskFor != "cv-outer" ||
		resolved.TaskDefinition == nil ||
		resolved.TaskDefinition.Purpose != taskDef.Purpose {
		t.Fatalf("resolved did not preserve fields: %+v", resolved)
	}
}

// Two StageAwaitingTaskApproval holds alive at once must resolve LIFO
// when a stage-filtered lookup runs without an ApprovalID. The user
// is replying to the MOST RECENT inline prompt the harness rendered;
// resolving the older one would silently land on stale state — the
// same failure pattern the no-stage LIFO fix addressed, just in the
// stage-filtered code path.
func TestMemoryPendingApprovalCache_StageFilteredLookupIsLIFO(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	ctx := context.Background()

	older, err := cache.Hold(ctx, PendingLiteApproval{
		ID: "cv-older", UserID: "user-1", AgentID: "agent-1",
		Provider: conversation.ProviderAnthropic, Stage: StageAwaitingTaskApproval,
	})
	if err != nil {
		t.Fatal(err)
	}
	newer, err := cache.Hold(ctx, PendingLiteApproval{
		ID: "cv-newer", UserID: "user-1", AgentID: "agent-1",
		Provider: conversation.ProviderAnthropic, Stage: StageAwaitingTaskApproval,
	})
	if err != nil {
		t.Fatal(err)
	}

	peeked, err := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageAwaitingTaskApproval,
	})
	if err != nil {
		t.Fatal(err)
	}
	if peeked == nil || peeked.ID != newer.Pending.ID {
		t.Fatalf("stage-filtered no-ID Peek returned %+v, want most recent %q", peeked, newer.Pending.ID)
	}
	_ = older
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
