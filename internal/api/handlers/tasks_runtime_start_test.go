package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestTasksHandlerStartBindsRuntimeSessionToTask(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "start-task.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "runtime-start@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "runtime-agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	task := &store.Task{
		ID:               "task-start-1",
		UserID:           user.ID,
		AgentID:          agent.ID,
		Purpose:          "bind runtime session to task",
		Status:           "active",
		Lifetime:         "session",
		SchemaVersion:    2,
		ExpiresInSeconds: 1800,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	sess := &store.RuntimeSession{
		ID:                    "runtime-session-1",
		UserID:                user.ID,
		AgentID:               agent.ID,
		Mode:                  "proxy",
		ProxyBearerSecretHash: "secret-hash",
		ExpiresAt:             time.Now().UTC().Add(10 * time.Minute),
	}
	if err := st.CreateRuntimeSession(ctx, sess); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"runtime_session_id": sess.ID})
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/"+task.ID+"/start", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(store.WithAgent(req.Context(), agent))
	req.SetPathValue("id", task.ID)

	rec := httptest.NewRecorder()
	h := &TasksHandler{st: st}
	h.Start(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Start status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["session_id"] != sess.ID {
		t.Fatalf("expected session_id %q, got %v", sess.ID, resp["session_id"])
	}
	active, err := st.GetActiveTaskSession(ctx, task.ID, sess.ID)
	if err != nil {
		t.Fatalf("GetActiveTaskSession: %v", err)
	}
	if active.Status != "active" {
		t.Fatalf("active task session status=%q, want active", active.Status)
	}
}

func TestTasksHandlerEndClearsRuntimeSessionBindingWithoutCompletingTask(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "end-task.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)

	user, err := st.CreateUser(ctx, "runtime-end@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "runtime-agent", "token-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	task := &store.Task{
		ID:               "task-end-1",
		UserID:           user.ID,
		AgentID:          agent.ID,
		Purpose:          "end runtime session binding",
		Status:           "active",
		Lifetime:         "standing",
		SchemaVersion:    2,
		ExpiresInSeconds: 1800,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	sess := &store.RuntimeSession{
		ID:                    "runtime-session-end-1",
		UserID:                user.ID,
		AgentID:               agent.ID,
		Mode:                  "proxy",
		ProxyBearerSecretHash: "secret-hash",
		ExpiresAt:             time.Now().UTC().Add(10 * time.Minute),
	}
	if err := st.CreateRuntimeSession(ctx, sess); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	if err := st.UpsertActiveTaskSession(ctx, &store.ActiveTaskSession{
		TaskID:     task.ID,
		SessionID:  sess.ID,
		UserID:     user.ID,
		AgentID:    agent.ID,
		Status:     "active",
		StartedAt:  time.Now().UTC(),
		LastSeenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpsertActiveTaskSession: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"runtime_session_id": sess.ID})
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/"+task.ID+"/end", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(store.WithAgent(req.Context(), agent))
	req.SetPathValue("id", task.ID)

	rec := httptest.NewRecorder()
	h := &TasksHandler{st: st}
	h.End(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("End status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := st.GetActiveTaskSession(ctx, task.ID, sess.ID); err != store.ErrNotFound {
		t.Fatalf("GetActiveTaskSession err=%v, want ErrNotFound after end", err)
	}
	storedTask, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if storedTask.Status != "active" {
		t.Fatalf("task status=%q, want active", storedTask.Status)
	}
}

func TestHighestRiskLevelPrefersConcreteSeverityOverUnknown(t *testing.T) {
	if got := highestRiskLevel("unknown", "critical"); got != "critical" {
		t.Fatalf("highestRiskLevel(unknown, critical) = %q, want critical", got)
	}
	if got := highestRiskLevel("high", "unknown"); got != "high" {
		t.Fatalf("highestRiskLevel(high, unknown) = %q, want high", got)
	}
}
