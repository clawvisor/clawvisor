package llmproxy

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/redis/go-redis/v9"
)

func testRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func TestRedisPendingApprovalCacheResolvesBareApprovalLIFO(t *testing.T) {
	cache := NewRedisPendingApprovalCache(testRedisClient(t), time.Minute)
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
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved == nil || resolved.ID != "cv-second" {
		t.Fatalf("first resolve = %+v, want cv-second", resolved)
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
		t.Fatalf("second resolve = %+v, want cv-first", resolved)
	}
}

func TestRedisPendingApprovalCacheStageResolveLeavesOtherHolds(t *testing.T) {
	cache := NewRedisPendingApprovalCache(testRedisClient(t), time.Minute)
	ctx := context.Background()

	if _, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-tool",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageTool,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-task",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageAwaitingTaskApproval,
	}); err != nil {
		t.Fatal(err)
	}

	resolved, err := cache.Resolve(ctx, ResolveRequest{
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageAwaitingTaskApproval,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved == nil || resolved.ID != "cv-task" {
		t.Fatalf("stage resolve = %+v, want cv-task", resolved)
	}

	resolved, err = cache.Resolve(ctx, ResolveRequest{
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved == nil || resolved.ID != "cv-tool" {
		t.Fatalf("remaining resolve = %+v, want cv-tool", resolved)
	}
}

// TestRedisPendingApprovalCacheScopesByConversationID asserts the
// redis-backed cache also partitions holds per conversation. Without
// this, two Claude Code sessions sharing a token would collide on the
// same redis key and bare-verb replies could cross conversations.
func TestRedisPendingApprovalCacheScopesByConversationID(t *testing.T) {
	cache := NewRedisPendingApprovalCache(testRedisClient(t), time.Minute)
	ctx := context.Background()

	if _, err := cache.Hold(ctx, PendingLiteApproval{
		ID:             "cv-A",
		UserID:         "user-1",
		AgentID:        "agent-1",
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-A",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Hold(ctx, PendingLiteApproval{
		ID:             "cv-B",
		UserID:         "user-1",
		AgentID:        "agent-1",
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-B",
	}); err != nil {
		t.Fatal(err)
	}

	resolvedA, err := cache.Resolve(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1",
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-A",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolvedA == nil || resolvedA.ID != "cv-A" {
		t.Fatalf("conv-A resolved %+v, want cv-A", resolvedA)
	}

	resolvedB, err := cache.Resolve(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1",
		Provider:       conversation.ProviderAnthropic,
		ConversationID: "conv-B",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolvedB == nil || resolvedB.ID != "cv-B" {
		t.Fatalf("conv-B resolved %+v, want cv-B", resolvedB)
	}
}

func TestRedisInlineApprovalOutcomeStoreRecordAndLookup(t *testing.T) {
	store := NewRedisInlineApprovalOutcomeStore(testRedisClient(t), time.Minute)
	key := InlineApprovalOutcomeKey{UserID: "user-1", AgentID: "agent-1", ApprovalID: "cv-1"}

	store.Record(key, InlineApprovalOutcome{
		Decision:  "allow",
		Outcome:   "inline_task_approved",
		Succeeded: true,
		TaskID:    "task-1",
		Credentials: []InlineTaskCredentialPlaceholder{
			{VaultItemID: "api_key", Placeholder: "cv_secret_1"},
		},
		RequestID: "req-1",
	})

	out, ok := store.Lookup(key)
	if !ok || !out.Succeeded || out.TaskID != "task-1" || out.RequestID != "req-1" {
		t.Fatalf("lookup = (%+v, %v)", out, ok)
	}
	if len(out.Credentials) != 1 || out.Credentials[0].Placeholder != "cv_secret_1" {
		t.Fatalf("credentials not preserved: %+v", out.Credentials)
	}
	if _, ok := store.Lookup(InlineApprovalOutcomeKey{UserID: "user-1", AgentID: "agent-2", ApprovalID: "cv-1"}); ok {
		t.Fatal("lookup should be scoped by agent")
	}
}
