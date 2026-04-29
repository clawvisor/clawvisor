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

	held, ok := cache.Hold("sess", "approval-1", "task-1", "tool-1", "fetch_messages", map[string]any{"max": 10}, "needs review")
	if !ok {
		t.Fatal("Hold should succeed")
	}
	second, ok := cache.Hold("sess", "approval-2", "task-2", "tool-2", "other", nil, "blocked")
	if !ok {
		t.Fatal("second Hold should succeed for a different approval")
	}
	if got := cache.Get("sess"); got == nil || got.ID != held.ID {
		t.Fatalf("Get returned %+v", got)
	}
	if got := cache.GetByApprovalRecord("sess", "approval-2"); got == nil || got.ID != second.ID {
		t.Fatalf("GetByApprovalRecord returned %+v", got)
	}
	if got := cache.Count("sess"); got != 2 {
		t.Fatalf("Count = %d, want 2", got)
	}
	if got := cache.Resolve("sess", held.ID); got == nil || got.ToolUseID != "tool-1" {
		t.Fatalf("Resolve returned %+v", got)
	}
	if got := cache.Get("sess"); got == nil || got.ID != second.ID {
		t.Fatalf("expected second held approval to remain, got %+v", got)
	}

	held, ok = cache.Hold("sess", "approval-3", "task-3", "tool-3", "fetch_messages", nil, "again")
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
