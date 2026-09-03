package slack

import (
	"context"
	"testing"
	"time"
)

// The Slack signature stays valid for the whole skew window, so without a
// guard a captured payload can be resubmitted freely inside it. The previous
// default was a no-op, which meant no replay protection at all.
func TestMemoryReplayGuard_RejectsRepeatedSignature(t *testing.T) {
	g := newMemoryReplayGuard()
	ctx := context.Background()

	seen, err := g.SeenBefore(ctx, "sig-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if seen {
		t.Fatal("first submission reported as a replay")
	}

	seen, err = g.SeenBefore(ctx, "sig-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("replayed signature was not rejected")
	}

	// A different payload must still get through.
	if seen, _ := g.SeenBefore(ctx, "sig-b", time.Minute); seen {
		t.Fatal("distinct signature rejected as a replay")
	}
}

// Entries are only useful for the skew window; they must not accumulate for
// the process lifetime.
func TestMemoryReplayGuard_ForgetsExpiredSignatures(t *testing.T) {
	g := newMemoryReplayGuard()
	ctx := context.Background()

	if _, err := g.SeenBefore(ctx, "sig-a", -time.Second); err != nil {
		t.Fatal(err)
	}
	if seen, _ := g.SeenBefore(ctx, "sig-a", time.Minute); seen {
		t.Fatal("an expired signature still counted as a replay")
	}

	g.mu.Lock()
	n := len(g.seen)
	g.mu.Unlock()
	if n != 1 {
		t.Fatalf("guard retained %d entries, want only the live one", n)
	}
}
