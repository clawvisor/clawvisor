package llmproxy

import (
	"testing"
	"time"
)

func TestMemoryActiveTasksSnapshotCache_RoundTrip(t *testing.T) {
	c := NewMemoryActiveTasksSnapshotCache(time.Hour, 0)
	key := ActiveTasksSnapshotKey{UserID: "u", AgentID: "a", ConversationID: "c"}
	if _, ok := c.Lookup(key); ok {
		t.Fatal("empty cache should miss")
	}
	c.Record(key, "- task-1 · purpose=\"do thing\" · lifetime=sliding · expires=auto-extends")
	got, ok := c.Lookup(key)
	if !ok {
		t.Fatal("Record then Lookup must hit")
	}
	if got == "" {
		t.Fatal("cached snapshot lost")
	}
}

// TestMemoryActiveTasksSnapshotCache_FrozenAcrossUpdates pins the
// cache-stability guarantee: once a snapshot is recorded for a
// conversation, the SAME bytes come back on every later Lookup even
// if the underlying task list would now render differently. The
// agent learns about mid-conversation task creations via the
// augmenter's task-approved notice (below the cache breakpoint), not
// via this snapshot.
func TestMemoryActiveTasksSnapshotCache_FrozenAcrossUpdates(t *testing.T) {
	c := NewMemoryActiveTasksSnapshotCache(time.Hour, 0)
	key := ActiveTasksSnapshotKey{UserID: "u", AgentID: "a", ConversationID: "c"}
	first := "- task-1 · purpose=\"first\" · lifetime=sliding · expires=auto-extends"
	c.Record(key, first)
	got, ok := c.Lookup(key)
	if !ok || got != first {
		t.Fatalf("first lookup: ok=%v got=%q want=%q", ok, got, first)
	}
	// A second lookup must return the same bytes, byte-for-byte.
	again, ok := c.Lookup(key)
	if !ok || again != first {
		t.Fatalf("second lookup drifted: ok=%v got=%q want=%q", ok, again, first)
	}
}

func TestMemoryActiveTasksSnapshotCache_EmptyConversationIDDoesNotCache(t *testing.T) {
	c := NewMemoryActiveTasksSnapshotCache(time.Hour, 0)
	key := ActiveTasksSnapshotKey{UserID: "u", AgentID: "a", ConversationID: ""}
	c.Record(key, "ignored")
	if _, ok := c.Lookup(key); ok {
		t.Fatal("empty conversationID must not be cached (would collide across conversations)")
	}
}

func TestMemoryActiveTasksSnapshotCache_DifferentConversations(t *testing.T) {
	c := NewMemoryActiveTasksSnapshotCache(time.Hour, 0)
	k1 := ActiveTasksSnapshotKey{UserID: "u", AgentID: "a", ConversationID: "conv-1"}
	k2 := ActiveTasksSnapshotKey{UserID: "u", AgentID: "a", ConversationID: "conv-2"}
	c.Record(k1, "snapshot-1")
	c.Record(k2, "snapshot-2")
	g1, _ := c.Lookup(k1)
	g2, _ := c.Lookup(k2)
	if g1 != "snapshot-1" || g2 != "snapshot-2" {
		t.Fatalf("conversation keys collided: g1=%q g2=%q", g1, g2)
	}
}

func TestMemoryActiveTasksSnapshotCache_TTL(t *testing.T) {
	c := NewMemoryActiveTasksSnapshotCache(time.Millisecond, 0)
	key := ActiveTasksSnapshotKey{UserID: "u", AgentID: "a", ConversationID: "c"}
	c.Record(key, "snap")
	time.Sleep(5 * time.Millisecond)
	if _, ok := c.Lookup(key); ok {
		t.Fatal("entry should have expired")
	}
}

func TestMemoryActiveTasksSnapshotCache_SizeCap(t *testing.T) {
	c := NewMemoryActiveTasksSnapshotCache(time.Hour, 3)
	for i := range 5 {
		c.Record(ActiveTasksSnapshotKey{
			UserID: "u", AgentID: "a", ConversationID: string(rune('a' + i)),
		}, "snap")
	}
	// Soft cap: at most maxSize entries remain.
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n > 3 {
		t.Fatalf("size cap not enforced: %d entries", n)
	}
}
