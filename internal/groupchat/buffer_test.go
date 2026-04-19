package groupchat

import (
	"testing"
	"time"
)

func TestMessagesForKeyPrefix_ScopesByUser(t *testing.T) {
	buf := NewMessageBuffer(20, 15*time.Minute)
	now := time.Now()

	// Two users' conversations. We'll ask for just user-A's scope and
	// expect to see only those entries, keyed by the full storage key.
	buf.Append(UserScopedKey("user-A", "slack:C1/thread:a"), BufferedMessage{
		Text: "A-hi", SenderName: "alice", Timestamp: now, Role: "user", BridgeID: "bridge-A",
	})
	buf.Append(UserScopedKey("user-A", "telegram:1"), BufferedMessage{
		Text: "A-telegram", SenderName: "alice", Timestamp: now, Role: "user", BridgeID: "bridge-A",
	})
	buf.Append(UserScopedKey("user-B", "slack:C1/thread:a"), BufferedMessage{
		Text: "B-hi", SenderName: "bob", Timestamp: now, Role: "user", BridgeID: "bridge-B",
	})

	// Stale message in user-A's scope — must be filtered out by maxAge.
	buf.Append(UserScopedKey("user-A", "stale"), BufferedMessage{
		Text: "stale", SenderName: "alice", Timestamp: now.Add(-20 * time.Minute), Role: "user",
	})

	got := buf.MessagesForKeyPrefix("u:user-A:")

	if _, ok := got[UserScopedKey("user-B", "slack:C1/thread:a")]; ok {
		t.Fatal("user-B entry leaked into user-A's prefix scan")
	}
	if _, ok := got[UserScopedKey("user-A", "stale")]; ok {
		t.Fatal("stale message should have been filtered out by maxAge")
	}
	if len(got[UserScopedKey("user-A", "slack:C1/thread:a")]) != 1 {
		t.Fatalf("missing user-A slack entry: %+v", got)
	}
	if len(got[UserScopedKey("user-A", "telegram:1")]) != 1 {
		t.Fatalf("missing user-A telegram entry: %+v", got)
	}
}
