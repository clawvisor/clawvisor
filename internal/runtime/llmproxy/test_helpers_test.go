package llmproxy

// Test helpers shared by tests that stay in the llmproxy package
// (release_test, placeholder_validation_test, etc.) after the
// postprocess migration. These mirror the same-named helpers in
// internal/runtime/llmproxy/postproc/postprocess_test.go +
// taskscope_test.go; the duplication is small and keeps the two
// packages' tests self-contained.

import (
	"context"
	"path/filepath"
	"testing"

	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

// newTaskscopeStore mirrors the helper in postproc/taskscope_test.go.
func newTaskscopeStore(t *testing.T) (store.Store, *store.User, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "ts.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "ts@example.com", "x", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "ts-agent", "tok-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return st, user, agent
}

// seedActiveTask mirrors the helper in postproc/taskscope_test.go.
func seedActiveTask(t *testing.T, st store.Store, user *store.User, agent *store.Agent, actions []store.TaskAction) *store.Task {
	t.Helper()
	task := &store.Task{
		ID:                "task-" + agent.ID,
		UserID:            user.ID,
		AgentID:           agent.ID,
		Purpose:           "test",
		Status:            "active",
		Lifetime:          "session",
		AuthorizedActions: actions,
	}
	if err := st.CreateTask(context.Background(), task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return task
}

// seedPostprocessStore + seedPostprocessStoreWithService mirror the
// helpers in postproc/postprocess_test.go.
func seedPostprocessStore(t *testing.T, placeholder string) (store.Store, string, string) {
	return seedPostprocessStoreWithService(t, placeholder, "github")
}

func seedPostprocessStoreWithService(t *testing.T, placeholder, serviceID string) (store.Store, string, string) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "pp.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "pp@example.com", "x", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "pp-agent", "tok-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := st.CreateRuntimePlaceholder(ctx, &store.RuntimePlaceholder{
		Placeholder: placeholder,
		UserID:      user.ID,
		AgentID:     agent.ID,
		ServiceID:   serviceID,
	}); err != nil {
		t.Fatalf("CreateRuntimePlaceholder: %v", err)
	}
	return st, user.ID, agent.ID
}

// stubIntentVerifier mirrors the helper in postproc/postprocess_test.go.
type stubIntentVerifier struct {
	verdict     *IntentVerdict
	allow       bool
	reason      string
	explanation string
	called      bool
	calls       []runtimedecision.IntentVerifyRequest
	err         error
}

func (v *stubIntentVerifier) Verify(_ context.Context, req IntentVerifyRequest) (*IntentVerdict, error) {
	v.called = true
	v.calls = append(v.calls, runtimedecision.IntentVerifyRequest{
		TaskPurpose: req.TaskPurpose,
		ExpectedUse: req.ExpectedUse,
		Service:     req.Service,
		Action:      req.Action,
		Params:      req.Params,
		Reason:      req.Reason,
		TaskID:      req.TaskID,
		Lenient:     req.Lenient,
	})
	if v.err != nil {
		return nil, v.err
	}
	if v.verdict != nil {
		return v.verdict, nil
	}
	return &IntentVerdict{Allow: v.allow, Explanation: v.explanation}, nil
}
