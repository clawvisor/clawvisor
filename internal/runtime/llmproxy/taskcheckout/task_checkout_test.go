package taskcheckout

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestMemoryStore_RequiresConversationID pins the per-conversation
// isolation invariant at the storage layer: an empty ConversationID
// is refused on Set and returns not-found on Get, with no fallback to
// a shared (user, agent) bucket. The pre-strict behavior of silently
// writing to that bucket was the cross-conversation scope leak the
// fix targets.
func TestMemoryStore_RequiresConversationID(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore(time.Hour)
	ctx := context.Background()

	t.Run("Set without ConversationID returns ErrConversationIDRequired", func(t *testing.T) {
		err := store.Set(ctx, Key{UserID: "u", AgentID: "a"}, "task-1", time.Hour)
		if !errors.Is(err, ErrConversationIDRequired) {
			t.Fatalf("Set without CID: err=%v, want ErrConversationIDRequired", err)
		}
	})

	t.Run("Get without ConversationID returns not-found", func(t *testing.T) {
		_, ok, err := store.Get(ctx, Key{UserID: "u", AgentID: "a"})
		if err != nil {
			t.Fatalf("Get without CID: err=%v, want nil", err)
		}
		if ok {
			t.Fatalf("Get without CID returned ok=true; want false (no legacy fallback)")
		}
	})

	t.Run("Set with ConversationID succeeds", func(t *testing.T) {
		err := store.Set(ctx, Key{UserID: "u", AgentID: "a", ConversationID: "conv-A"}, "task-1", time.Hour)
		if err != nil {
			t.Fatalf("Set with CID: %v", err)
		}
	})

	t.Run("Get with conv-A round-trips; sibling Get with conv-B sees nothing", func(t *testing.T) {
		got, ok, err := store.Get(ctx, Key{UserID: "u", AgentID: "a", ConversationID: "conv-A"})
		if err != nil || !ok || got.TaskID != "task-1" {
			t.Fatalf("Get conv-A: got=%+v ok=%v err=%v, want task-1", got, ok, err)
		}
		got, ok, err = store.Get(ctx, Key{UserID: "u", AgentID: "a", ConversationID: "conv-B"})
		if err != nil {
			t.Fatalf("Get conv-B: err=%v", err)
		}
		if ok {
			t.Fatalf("sibling conv-B saw conv-A's checkout (leak): %+v", got)
		}
	})
}
