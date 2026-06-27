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

func sampleDrift(agentID, convID, toolUseID string) ScopeDrift {
	return ScopeDrift{
		UserID:         "user-1",
		AgentID:        agentID,
		ConversationID: convID,
		Provider:       conversation.ProviderAnthropic,
		ToolUse: conversation.ToolUse{
			ID:    toolUseID,
			Name:  "Bash",
			Input: []byte(`{"command":"ls"}`),
		},
		Service:    "svc",
		Action:     "act",
		Host:       "h",
		Method:     "POST",
		Path:       "/p",
		Source:     ScopeDriftSourceTaskScope,
		ReasonText: "no covering task",
	}
}

func TestRedisScopeDriftRegistry_CrossInstanceRegisterGet(t *testing.T) {
	rdb := testRedisClient(t)
	minting := NewRedisScopeDriftRegistry(rdb, time.Minute)
	resolver := NewRedisScopeDriftRegistry(rdb, time.Minute)
	ctx := context.Background()

	stored, err := minting.Register(ctx, sampleDrift("agent-1", "conv-1", "tu-1"))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if stored.ID == "" {
		t.Fatal("Register: empty ID")
	}

	got, err := resolver.Get(ctx, stored.ID)
	if err != nil {
		t.Fatalf("Get on resolver: %v", err)
	}
	if got.AgentID != "agent-1" || got.Service != "svc" || got.Provider != conversation.ProviderAnthropic {
		t.Fatalf("Get returned wrong record: %+v", got)
	}
	if string(got.ToolUse.Input) != `{"command":"ls"}` {
		t.Fatalf("ToolUse.Input not preserved: %q", string(got.ToolUse.Input))
	}
	if got.Fingerprint() != stored.Fingerprint() {
		t.Fatalf("Fingerprint mismatch across instances: %q vs %q", got.Fingerprint(), stored.Fingerprint())
	}
}

func TestRedisScopeDriftRegistry_ClaimOptionIsOneShot(t *testing.T) {
	rdb := testRedisClient(t)
	a := NewRedisScopeDriftRegistry(rdb, time.Minute)
	b := NewRedisScopeDriftRegistry(rdb, time.Minute)
	ctx := context.Background()

	stored, err := a.Register(ctx, sampleDrift("agent-claim", "conv-claim", "tu-claim"))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	claimed, err := a.ClaimOption(ctx, stored.ID, ScopeDriftOptionOneOff, "note-a")
	if err != nil {
		t.Fatalf("ClaimOption on a: %v", err)
	}
	if claimed.ChosenOption != ScopeDriftOptionOneOff || claimed.Outcome != ScopeDriftOutcomePending || claimed.AgentNote != "note-a" {
		t.Fatalf("ClaimOption result: %+v", claimed)
	}
	// Second claim on a different instance must surface ErrDriftAlreadyResolved.
	if _, err := b.ClaimOption(ctx, stored.ID, ScopeDriftOptionExpand, "note-b"); !errors.Is(err, ErrDriftAlreadyResolved) {
		t.Fatalf("second ClaimOption: want ErrDriftAlreadyResolved, got %v", err)
	}
}

func TestRedisScopeDriftRegistry_SetOutcomeSucceededMintsPreClearAcrossInstances(t *testing.T) {
	rdb := testRedisClient(t)
	a := NewRedisScopeDriftRegistry(rdb, time.Minute)
	b := NewRedisScopeDriftRegistry(rdb, time.Minute)
	ctx := context.Background()

	stored, err := a.Register(ctx, sampleDrift("agent-set", "conv-set", "tu-set"))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := a.ClaimOption(ctx, stored.ID, ScopeDriftOptionExpand, ""); err != nil {
		t.Fatalf("ClaimOption: %v", err)
	}
	if err := a.SetOutcome(ctx, stored.ID, ScopeDriftOutcomeSucceeded); err != nil {
		t.Fatalf("SetOutcome on a: %v", err)
	}
	driftID, ok := b.LookupPreClear(ctx, "agent-set", stored.Fingerprint())
	if !ok || driftID != stored.ID {
		t.Fatalf("LookupPreClear on b: ok=%v id=%q want id=%q", ok, driftID, stored.ID)
	}
}

func TestRedisScopeDriftRegistry_RollbackClaimDeletesPreClearAcrossInstances(t *testing.T) {
	rdb := testRedisClient(t)
	a := NewRedisScopeDriftRegistry(rdb, time.Minute)
	b := NewRedisScopeDriftRegistry(rdb, time.Minute)
	ctx := context.Background()

	stored, err := a.Register(ctx, sampleDrift("agent-rb", "conv-rb", "tu-rb"))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := a.ClaimOption(ctx, stored.ID, ScopeDriftOptionNewTask, ""); err != nil {
		t.Fatalf("ClaimOption: %v", err)
	}
	if err := a.SetOutcome(ctx, stored.ID, ScopeDriftOutcomeSucceeded); err != nil {
		t.Fatalf("SetOutcome: %v", err)
	}
	// Downstream failure on a triggers a rollback. b must NOT see a
	// surviving pre-clear afterward — the lockstep invariant.
	if err := a.RollbackClaim(ctx, stored.ID); err != nil {
		t.Fatalf("RollbackClaim: %v", err)
	}
	if _, ok := b.LookupPreClear(ctx, "agent-rb", stored.Fingerprint()); ok {
		t.Fatal("LookupPreClear on b after RollbackClaim: want miss, got hit (stale pre-clear leaked past rollback)")
	}
	got, err := b.Get(ctx, stored.ID)
	if err != nil {
		t.Fatalf("Get on b after rollback: %v", err)
	}
	if got.ChosenOption != "" || got.Outcome != "" || got.AgentNote != "" {
		t.Fatalf("RollbackClaim left claim fields populated: %+v", got)
	}
}

func TestRedisScopeDriftRegistry_LookupPreClearConsumesOnce(t *testing.T) {
	rdb := testRedisClient(t)
	a := NewRedisScopeDriftRegistry(rdb, time.Minute)
	b := NewRedisScopeDriftRegistry(rdb, time.Minute)
	ctx := context.Background()

	stored, err := a.Register(ctx, sampleDrift("agent-cons", "conv-cons", "tu-cons"))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := a.ClaimOption(ctx, stored.ID, ScopeDriftOptionOneOff, ""); err != nil {
		t.Fatalf("ClaimOption: %v", err)
	}
	if err := a.SetOutcome(ctx, stored.ID, ScopeDriftOutcomeSucceeded); err != nil {
		t.Fatalf("SetOutcome: %v", err)
	}
	if _, ok := b.LookupPreClear(ctx, "agent-cons", stored.Fingerprint()); !ok {
		t.Fatal("first LookupPreClear on b: want hit, got miss")
	}
	if _, ok := a.LookupPreClear(ctx, "agent-cons", stored.Fingerprint()); ok {
		t.Fatal("second LookupPreClear on a: want miss (consumed), got hit")
	}
}

func TestRedisScopeDriftRegistry_PendingSubstitutionCrossInstanceRoundTrip(t *testing.T) {
	rdb := testRedisClient(t)
	a := NewRedisScopeDriftRegistry(rdb, time.Minute)
	b := NewRedisScopeDriftRegistry(rdb, time.Minute)
	ctx := context.Background()

	key := PendingSubstitutionKey{AgentID: "agent-sub", ConversationID: "conv-sub", ToolUseID: "tu-sub"}
	value := PendingSubstitution{
		DriftID:           "drift-sub",
		MenuText:          "<scope_drift_notice>blocked: foo</scope_drift_notice>",
		OriginalToolName:  "Bash",
		OriginalToolInput: []byte(`{"command":"curl evil"}`),
	}
	if err := a.RegisterPendingSubstitution(ctx, key, value); err != nil {
		t.Fatalf("Register on a: %v", err)
	}
	// Lookup on b must see the same substitution byte-for-byte.
	got, ok := b.LookupPendingSubstitution(ctx, key)
	if !ok {
		t.Fatal("Lookup on b: want hit, got miss")
	}
	if got.DriftID != value.DriftID || got.MenuText != value.MenuText || got.OriginalToolName != value.OriginalToolName {
		t.Fatalf("Lookup on b returned wrong substitution: %+v", got)
	}
	if string(got.OriginalToolInput) != string(value.OriginalToolInput) {
		t.Fatalf("OriginalToolInput corrupted: got %q, want %q", got.OriginalToolInput, value.OriginalToolInput)
	}
	// Lookup must NOT consume — repeat reads on a must still see it.
	if _, ok := a.LookupPendingSubstitution(ctx, key); !ok {
		t.Fatal("second Lookup on a: want hit (Lookup is not consuming), got miss")
	}
	// Delete on a is observable from b.
	a.DeletePendingSubstitution(ctx, key)
	if _, ok := b.LookupPendingSubstitution(ctx, key); ok {
		t.Fatal("Lookup on b after Delete on a: want miss, got hit")
	}
}

// A caller-supplied ExpiresAt that's longer than the registry's
// default ttl must be honored. The memory impl stores ExpiresAt
// verbatim and Get checks against it; the Redis impl had been
// defaulting the key PEXPIRE to r.ttl, which evicted long-lived
// drifts and surfaced premature ErrDriftNotFound.
func TestRedisScopeDriftRegistry_RegisterHonorsCallerExpiresAt(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	// Registry default ttl is short; caller asks for much longer.
	reg := NewRedisScopeDriftRegistry(rdb, 5*time.Second)
	ctx := context.Background()

	tmpl := sampleDrift("agent-exp", "conv-exp", "tu-exp")
	tmpl.ExpiresAt = time.Now().UTC().Add(60 * time.Second)
	stored, err := reg.Register(ctx, tmpl)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// 10 s past the registry default ttl, well before the caller's
	// 60 s ExpiresAt. The drift must still be retrievable.
	mr.FastForward(10 * time.Second)
	got, err := reg.Get(ctx, stored.ID)
	if err != nil {
		t.Fatalf("Get with long ExpiresAt after 10s: want hit, got %v", err)
	}
	if got.ID != stored.ID {
		t.Fatalf("Get returned wrong drift: %+v", got)
	}
}

func TestRedisScopeDriftRegistry_TTLExpiry(t *testing.T) {
	// Use a real miniredis so we can FastForward time deterministically.
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	reg := NewRedisScopeDriftRegistry(rdb, 30*time.Second)
	ctx := context.Background()
	stored, err := reg.Register(ctx, sampleDrift("agent-ttl", "conv-ttl", "tu-ttl"))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	mr.FastForward(31 * time.Second)
	if _, err := reg.Get(ctx, stored.ID); !errors.Is(err, ErrDriftNotFound) {
		t.Fatalf("Get after TTL: want ErrDriftNotFound, got %v", err)
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
