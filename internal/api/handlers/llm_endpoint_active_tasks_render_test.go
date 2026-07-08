package handlers

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

// TestRenderActiveTasksSnapshot_SlidingExpiryStable pins the cache-stability
// fix: a sliding-lifetime task's ExpiresAt is bumped on every authorized
// tool_use, so embedding the literal timestamp in the system-prompt
// snapshot would mutate prefix bytes every turn and bust Anthropic's
// prompt cache. The renderer must emit a fixed sentinel ("auto-extends")
// for sliding tasks so the snapshot bytes do not drift as expiry rolls
// forward.
func TestRenderActiveTasksSnapshot_SlidingExpiryStable(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "snapshot.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "snapshot@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "agent", "token")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	t0 := time.Now().UTC().Add(10 * time.Minute).Truncate(time.Second)
	task := &store.Task{
		ID:        "task-sliding",
		UserID:    user.ID,
		AgentID:   agent.ID,
		Purpose:   "Edit web files",
		Status:    "active",
		Lifetime:  "sliding",
		ExpiresAt: &t0,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	h := &LLMEndpointHandler{
		Store:  st,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	first := h.renderActiveTasksSnapshot(ctx, user.ID, agent.ID)
	if !strings.Contains(first, "task-sliding") {
		t.Fatalf("snapshot missing task id: %q", first)
	}
	if !strings.Contains(first, "expires=auto-extends") {
		t.Fatalf("sliding task must render a stable expires sentinel, got: %q", first)
	}
	if strings.Contains(first, t0.Format("15:04")) {
		t.Fatalf("sliding task must NOT embed the literal expiry timestamp (cache busts every tool_use); got: %q", first)
	}

	// Simulate a sliding-lifetime bump: ExpiresAt moves forward, which
	// would have changed the snapshot bytes under the old renderer.
	t1 := t0.Add(10 * time.Minute)
	if err := st.UpdateTaskExpiresAt(ctx, task.ID, t1); err != nil {
		t.Fatalf("UpdateTaskExpiresAt: %v", err)
	}
	second := h.renderActiveTasksSnapshot(ctx, user.ID, agent.ID)
	if first != second {
		t.Fatalf("snapshot drifted between turns; cache will bust\nfirst:  %q\nsecond: %q", first, second)
	}
}

// TestRenderActiveTasksSnapshot_SessionExpiryRendered confirms that for
// session-lifetime tasks (which DON'T auto-extend), the actual UTC
// timestamp still appears — those bytes are stable and informative.
func TestRenderActiveTasksSnapshot_SessionExpiryRendered(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "snapshot-session.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "session@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "agent", "token")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	expiry := time.Date(2030, 1, 2, 3, 4, 0, 0, time.UTC)
	task := &store.Task{
		ID:        "task-session",
		UserID:    user.ID,
		AgentID:   agent.ID,
		Purpose:   "One-shot job",
		Status:    "active",
		Lifetime:  "session",
		ExpiresAt: &expiry,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	h := &LLMEndpointHandler{
		Store:  st,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	got := h.renderActiveTasksSnapshot(ctx, user.ID, agent.ID)
	if !strings.Contains(got, "expires=2030-01-02T03:04Z") {
		t.Fatalf("session task should render the literal expiry, got: %q", got)
	}
}
