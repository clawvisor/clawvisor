package review

import (
	"testing"
	"time"
)

func TestApprovalCacheHoldResolveAndExpire(t *testing.T) {
	t.Parallel()

	cache := NewApprovalCache()
	now := time.Now()
	cache.nowFn = func() time.Time { return now }
	cache.IdleTTL = time.Minute

	held, ok := cache.Hold("sess", "tool-1", "fetch_messages", map[string]any{"max": 10}, "needs review")
	if !ok {
		t.Fatal("Hold should succeed")
	}
	if _, ok := cache.Hold("sess", "tool-2", "other", nil, "blocked"); ok {
		t.Fatal("second Hold should fail while first is active")
	}
	if got := cache.Get("sess"); got == nil || got.ID != held.ID {
		t.Fatalf("Get returned %+v", got)
	}
	if got := cache.Resolve("sess", held.ID); got == nil || got.ToolUseID != "tool-1" {
		t.Fatalf("Resolve returned %+v", got)
	}
	if got := cache.Get("sess"); got != nil {
		t.Fatalf("expected cache empty after resolve, got %+v", got)
	}

	held, ok = cache.Hold("sess", "tool-3", "fetch_messages", nil, "again")
	if !ok {
		t.Fatal("Hold should succeed after resolve")
	}
	now = now.Add(2 * time.Minute)
	if got := cache.Get("sess"); got != nil {
		t.Fatalf("expected expired held approval, got %+v", got)
	}
	if got := cache.Resolve("sess", held.ID); got != nil {
		t.Fatalf("expected nil resolve after expiry, got %+v", got)
	}
}
