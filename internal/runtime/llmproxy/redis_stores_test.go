package llmproxy

import (
	"context"
	"errors"
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

func TestRedisScriptSessionCacheCrossInstanceAuthorizeAndAccount(t *testing.T) {
	rdb := testRedisClient(t)
	minting := NewRedisScriptSessionCache(rdb)
	resolver := NewRedisScriptSessionCache(rdb)
	ctx := context.Background()

	tok := mustMintSession(t, minting, sampleSession())
	got, err := resolver.Authorize(ctx, tok, ScriptSessionRequest{
		Host:        "gmail.googleapis.com:443",
		Method:      "get",
		Path:        "/gmail/v1/users/me/messages/abc?format=metadata",
		Placeholder: "autovault_x",
	})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if got.UsedCount != 1 {
		t.Fatalf("UsedCount after authorize = %d, want 1", got.UsedCount)
	}
	if got.TotalBytesUsed != got.MaxRequestBytes {
		t.Fatalf("TotalBytesUsed after reservation = %d, want %d", got.TotalBytesUsed, got.MaxRequestBytes)
	}

	got, err = minting.RecordBytes(ctx, tok, 12)
	if err != nil {
		t.Fatalf("record bytes: %v", err)
	}
	if got.TotalBytesUsed != 12 {
		t.Fatalf("TotalBytesUsed after true-up = %d, want 12", got.TotalBytesUsed)
	}
}

func TestRedisScriptSessionCacheRecordBytesClearsStaleSnapshotAfterRetryMiss(t *testing.T) {
	rdb := testRedisClient(t)
	cache := NewRedisScriptSessionCache(rdb)
	ctx := context.Background()

	tok := mustMintSession(t, cache, sampleSession())
	if _, err := cache.Authorize(ctx, tok, validRequest()); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	var raced bool
	cache.beforeRecordBytesCommit = func(token string) {
		if raced {
			return
		}
		raced = true
		if err := rdb.Del(ctx, redisScriptSessionKey(token)).Err(); err != nil {
			t.Fatalf("delete during record race: %v", err)
		}
	}

	got, err := cache.RecordBytes(ctx, tok, 12)
	if err != nil {
		t.Fatalf("record bytes: %v", err)
	}
	if !raced {
		t.Fatal("test hook did not exercise retry path")
	}
	if got.ID != "" {
		t.Fatalf("RecordBytes returned stale session after retry miss: %+v", got)
	}
}

func TestRedisScriptSessionCacheReleaseAuthorizeUndoesUseAndReservation(t *testing.T) {
	cache := NewRedisScriptSessionCache(testRedisClient(t))
	ctx := context.Background()

	tok := mustMintSession(t, cache, sampleSession())
	if _, err := cache.Authorize(ctx, tok, validRequest()); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if err := cache.ReleaseAuthorize(ctx, tok); err != nil {
		t.Fatalf("release authorize: %v", err)
	}
	got, err := cache.Authorize(ctx, tok, validRequest())
	if err != nil {
		t.Fatalf("authorize after release: %v", err)
	}
	if got.UsedCount != 1 {
		t.Fatalf("UsedCount after release+authorize = %d, want 1", got.UsedCount)
	}
	if got.TotalBytesUsed != got.MaxRequestBytes {
		t.Fatalf("TotalBytesUsed after release+authorize = %d, want %d", got.TotalBytesUsed, got.MaxRequestBytes)
	}
}

func TestRedisScriptSessionCacheScopeMismatchDoesNotConsumeUse(t *testing.T) {
	cache := NewRedisScriptSessionCache(testRedisClient(t))
	ctx := context.Background()

	tok := mustMintSession(t, cache, sampleSession())
	_, err := cache.Authorize(ctx, tok, ScriptSessionRequest{
		Host: "evil.example", Method: "GET", Path: "/gmail/v1/users/me/messages", Placeholder: "autovault_x",
	})
	if !errors.Is(err, ErrScriptSessionScopeMismatch) {
		t.Fatalf("authorize mismatch err = %v, want scope mismatch", err)
	}
	got, err := cache.Authorize(ctx, tok, validRequest())
	if err != nil {
		t.Fatalf("authorize after mismatch: %v", err)
	}
	if got.UsedCount != 1 {
		t.Fatalf("UsedCount after mismatch+authorize = %d, want 1", got.UsedCount)
	}
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

// Bare reply with a Stage filter must NOT walk past a newer
// different-stage hold to find an older same-stage one. Redis
// counterpart to the memory cache's
// TestMemoryPendingApprovalCache_BareReplyDoesNotWalkPastNewest —
// the Lua-script bare branch and the Go find()'s bare branch must
// agree: newest doesn't match → no match.
func TestRedisPendingApprovalCache_BareReplyDoesNotWalkPastNewest(t *testing.T) {
	cache := NewRedisPendingApprovalCache(testRedisClient(t), time.Minute)
	ctx := context.Background()

	if _, err := cache.Hold(ctx, PendingLiteApproval{
		ID: "cv-older-inline", UserID: "user-1", AgentID: "agent-1",
		Provider: conversation.ProviderAnthropic, Stage: StageAwaitingTaskApproval,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Hold(ctx, PendingLiteApproval{
		ID: "cv-newer-tool", UserID: "user-1", AgentID: "agent-1",
		Provider: conversation.ProviderAnthropic, Stage: StageTool,
	}); err != nil {
		t.Fatal(err)
	}

	// Peek (Go find() branch).
	got, err := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageAwaitingTaskApproval,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("bare stage-filtered Peek returned %+v, want nil", got)
	}

	// Resolve (Lua-script branch) — separate path, same rule.
	got, err = cache.Resolve(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageAwaitingTaskApproval,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("bare stage-filtered Resolve returned %+v, want nil", got)
	}

	// Both holds must still be in the cache — a no-match bare reply
	// must not consume anything.
	if got, _ := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1",
		Provider:   conversation.ProviderAnthropic,
		ApprovalID: "cv-older-inline",
	}); got == nil {
		t.Fatal("older inline hold should still be in cache")
	}
	if got, _ := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1",
		Provider:   conversation.ProviderAnthropic,
		ApprovalID: "cv-newer-tool",
	}); got == nil {
		t.Fatal("newer tool hold should still be in cache")
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

// Hold's key TTL must honor per-hold ExpiresAt; otherwise the Redis
// key (and every hold inside it) is evicted at c.ttl from the last
// LPush, regardless of the longer ExpiresAt written into the JSON.
func TestRedisPendingApprovalCacheHoldKeyTTLHonorsPerHoldExpiresAt(t *testing.T) {
	rdb := testRedisClient(t)
	cache := NewRedisPendingApprovalCache(rdb, 10*time.Minute)
	ctx := context.Background()

	now := time.Now().UTC()
	if _, err := cache.Hold(ctx, PendingLiteApproval{
		ID:        "cv-long",
		UserID:    "user-1",
		AgentID:   "agent-1",
		Provider:  conversation.ProviderAnthropic,
		ExpiresAt: now.Add(24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	key := redisPendingApprovalKey("user-1", "agent-1", conversation.ProviderAnthropic, "")
	ttl, err := rdb.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatal(err)
	}
	// Floor: must exceed the cache's default c.ttl (10 min). We expect
	// roughly 24h; allow generous slack for clock-tick variance.
	if ttl < 23*time.Hour {
		t.Fatalf("key PTTL = %v, want ≥ 23h (per-hold ExpiresAt was 24h)", ttl)
	}

	// A subsequent short-TTL hold pushed onto the same key must NOT
	// shrink the existing 24h key TTL — otherwise the still-pending
	// 24h hold would be evicted along with the key.
	if _, err := cache.Hold(ctx, PendingLiteApproval{
		ID:        "cv-short",
		UserID:    "user-1",
		AgentID:   "agent-1",
		Provider:  conversation.ProviderAnthropic,
		ExpiresAt: now.Add(2 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	ttl, err = rdb.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl < 23*time.Hour {
		t.Fatalf("after short-TTL Hold, key PTTL = %v, want ≥ 23h (sibling 24h hold must not be evicted)", ttl)
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

func TestRedisPendingApprovalCache_PeekByIDRoundTrip(t *testing.T) {
	rdb := testRedisClient(t)
	cache := NewRedisPendingApprovalCache(rdb, time.Minute)
	ctx := context.Background()

	held, err := cache.Hold(ctx, PendingLiteApproval{
		ID:            "cv-approval1",
		UserID:        "user-1",
		AgentID:       "agent-1",
		Provider:      conversation.ProviderAnthropic,
		PendingTaskID: "task-abc",
	})
	if err != nil {
		t.Fatalf("Hold failed: %v", err)
	}
	_ = held

	// 1. PeekByID using approval ID (starts with cv-)
	peekedAppr, err := cache.PeekByID(ctx, "cv-approval1")
	if err != nil {
		t.Fatalf("PeekByID by approval ID failed: %v", err)
	}
	if peekedAppr == nil || peekedAppr.ID != "cv-approval1" {
		t.Fatalf("PeekByID by approval ID returned %+v, want cv-approval1", peekedAppr)
	}

	// 2. PeekByID using task ID (does not start with cv-)
	peekedTask, err := cache.PeekByID(ctx, "task-abc")
	if err != nil {
		t.Fatalf("PeekByID by task ID failed: %v", err)
	}
	if peekedTask == nil || peekedTask.ID != "cv-approval1" {
		t.Fatalf("PeekByID by task ID returned %+v, want cv-approval1", peekedTask)
	}

	// 3. PeekByID using an unknown ID
	peekedUnknown, err := cache.PeekByID(ctx, "unknown-id")
	if err != nil {
		t.Fatalf("PeekByID by unknown ID failed: %v", err)
	}
	if peekedUnknown != nil {
		t.Fatalf("expected nil for unknown ID, got %+v", peekedUnknown)
	}

	// 4. Verify exact key namespaces in Redis
	valAppr, err := rdb.Get(ctx, "clawvisor:lite_pending_approval_map:approval:cv-approval1").Result()
	if err != nil {
		t.Fatalf("Get approval map key failed: %v", err)
	}
	if valAppr == "" {
		t.Fatalf("expected non-empty key for approval map key")
	}

	valTask, err := rdb.Get(ctx, "clawvisor:lite_pending_approval_map:task:task-abc").Result()
	if err != nil {
		t.Fatalf("Get task map key failed: %v", err)
	}
	if valTask == "" {
		t.Fatalf("expected non-empty key for task map key")
	}

	// 5. Drop the hold and verify keys are deleted
	err = cache.Drop(ctx, ResolveRequest{
		UserID:     "user-1",
		AgentID:    "agent-1",
		Provider:   conversation.ProviderAnthropic,
		ApprovalID: "cv-approval1",
	})
	if err != nil {
		t.Fatalf("Drop failed: %v", err)
	}

	// Verify keys are deleted from map
	_, err = rdb.Get(ctx, "clawvisor:lite_pending_approval_map:approval:cv-approval1").Result()
	if !errors.Is(err, redis.Nil) {
		t.Fatalf("expected approval map key to be deleted, got err: %v", err)
	}
	_, err = rdb.Get(ctx, "clawvisor:lite_pending_approval_map:task:task-abc").Result()
	if !errors.Is(err, redis.Nil) {
		t.Fatalf("expected task map key to be deleted, got err: %v", err)
	}
}

