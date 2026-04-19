package handlers

import (
	"context"
	"testing"
	"time"
)

func TestMemoryExtractionTracker_BasicLifecycle(t *testing.T) {
	tr := NewMemoryExtractionTracker(time.Minute)
	ctx := context.Background()

	if tr.HasPending(ctx, "task-1", "sess-1") {
		t.Fatal("fresh tracker should have no pending")
	}

	tr.MarkPending(ctx, "task-1", "sess-1", "audit-1")
	if !tr.HasPending(ctx, "task-1", "sess-1") {
		t.Error("expected pending after MarkPending")
	}

	tr.MarkDone(ctx, "task-1", "sess-1", "audit-1")
	if tr.HasPending(ctx, "task-1", "sess-1") {
		t.Error("expected no pending after MarkDone")
	}
}

func TestMemoryExtractionTracker_MultipleConcurrentPerSession(t *testing.T) {
	// Two extractions can overlap for the same session (e.g. list + get).
	// HasPending should return true while ANY entry is pending, and only
	// flip to false once all are done.
	tr := NewMemoryExtractionTracker(time.Minute)
	ctx := context.Background()

	tr.MarkPending(ctx, "task-1", "sess-1", "audit-1")
	tr.MarkPending(ctx, "task-1", "sess-1", "audit-2")

	if !tr.HasPending(ctx, "task-1", "sess-1") {
		t.Error("expected pending with 2 entries")
	}

	tr.MarkDone(ctx, "task-1", "sess-1", "audit-1")
	if !tr.HasPending(ctx, "task-1", "sess-1") {
		t.Error("expected pending after only 1 of 2 done")
	}

	tr.MarkDone(ctx, "task-1", "sess-1", "audit-2")
	if tr.HasPending(ctx, "task-1", "sess-1") {
		t.Error("expected no pending after both done")
	}
}

func TestMemoryExtractionTracker_SessionIsolation(t *testing.T) {
	// Work on one session should not register as pending for another.
	tr := NewMemoryExtractionTracker(time.Minute)
	ctx := context.Background()

	tr.MarkPending(ctx, "task-1", "sess-1", "audit-1")
	if tr.HasPending(ctx, "task-1", "sess-2") {
		t.Error("sess-2 should not see sess-1's pending")
	}
	if tr.HasPending(ctx, "task-2", "sess-1") {
		t.Error("task-2 should not see task-1's pending")
	}
}

func TestMemoryExtractionTracker_TTLExpires(t *testing.T) {
	// Orphaned entries (crashed instance) eventually expire.
	tr := NewMemoryExtractionTracker(50 * time.Millisecond)
	ctx := context.Background()

	tr.MarkPending(ctx, "task-1", "sess-1", "audit-orphan")
	if !tr.HasPending(ctx, "task-1", "sess-1") {
		t.Fatal("expected pending immediately after MarkPending")
	}

	time.Sleep(75 * time.Millisecond)
	if tr.HasPending(ctx, "task-1", "sess-1") {
		t.Error("expected pending to expire after TTL")
	}
}

func TestMemoryExtractionTracker_IgnoresEmptyKeys(t *testing.T) {
	// Defensive: empty taskID/sessionID/auditID should not register as
	// pending (nothing to correlate them with).
	tr := NewMemoryExtractionTracker(time.Minute)
	ctx := context.Background()

	tr.MarkPending(ctx, "", "sess-1", "audit-1")
	tr.MarkPending(ctx, "task-1", "", "audit-1")
	tr.MarkPending(ctx, "task-1", "sess-1", "")

	if tr.HasPending(ctx, "", "sess-1") {
		t.Error("empty taskID: HasPending should be false")
	}
	if tr.HasPending(ctx, "task-1", "") {
		t.Error("empty sessionID: HasPending should be false")
	}
	if tr.HasPending(ctx, "task-1", "sess-1") {
		t.Error("entries with any empty field should not register as pending")
	}
}

func TestMemoryExtractionTracker_MarkDoneUnknownAudit(t *testing.T) {
	// MarkDone for an auditID that was never pending should be a no-op,
	// not a panic.
	tr := NewMemoryExtractionTracker(time.Minute)
	ctx := context.Background()

	tr.MarkDone(ctx, "task-1", "sess-1", "never-existed")

	tr.MarkPending(ctx, "task-1", "sess-1", "audit-1")
	tr.MarkDone(ctx, "task-1", "sess-1", "never-existed")
	if !tr.HasPending(ctx, "task-1", "sess-1") {
		t.Error("spurious MarkDone should not affect unrelated pending entry")
	}
}
