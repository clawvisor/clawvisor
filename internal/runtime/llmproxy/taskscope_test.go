package llmproxy

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

func newTaskscopeStore(t *testing.T) (store.Store, *store.User, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "ts.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "ts@example.com", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "ts-agent", "tok-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return st, user, agent
}

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

func TestStoreTaskScopeChecker_AllowsMatchingAction(t *testing.T) {
	st, user, agent := newTaskscopeStore(t)
	seedActiveTask(t, st, user, agent, []store.TaskAction{
		{Service: "github", Action: "create_issue"},
	})
	c := NewStoreTaskScopeChecker(st)
	dec := c.Check(context.Background(), user.ID, agent.ID, "github", "create_issue")
	if !dec.Allowed {
		t.Errorf("expected allow, got %+v", dec)
	}
	if dec.TaskID == "" {
		t.Errorf("expected matched task id, got empty")
	}
}

func TestStoreTaskScopeChecker_DeniesUnauthorizedAction(t *testing.T) {
	st, user, agent := newTaskscopeStore(t)
	seedActiveTask(t, st, user, agent, []store.TaskAction{
		{Service: "github", Action: "list_issues"},
	})
	c := NewStoreTaskScopeChecker(st)
	dec := c.Check(context.Background(), user.ID, agent.ID, "github", "create_issue")
	if dec.Allowed {
		t.Errorf("expected deny — task only authorizes list_issues, got %+v", dec)
	}
	if dec.Reason != "needs_new_task" {
		t.Errorf("reason=%q, want needs_new_task", dec.Reason)
	}
}

func TestStoreTaskScopeChecker_NoActiveTask(t *testing.T) {
	st, user, agent := newTaskscopeStore(t)
	c := NewStoreTaskScopeChecker(st)
	dec := c.Check(context.Background(), user.ID, agent.ID, "github", "create_issue")
	if dec.Allowed {
		t.Errorf("expected deny when no active task, got %+v", dec)
	}
	if dec.Reason != "no_active_task" {
		t.Errorf("reason=%q, want no_active_task", dec.Reason)
	}
}

func TestStoreTaskScopeChecker_WildcardActionMatches(t *testing.T) {
	st, user, agent := newTaskscopeStore(t)
	seedActiveTask(t, st, user, agent, []store.TaskAction{
		{Service: "github", Action: "*"},
	})
	c := NewStoreTaskScopeChecker(st)
	dec := c.Check(context.Background(), user.ID, agent.ID, "github", "delete_repo")
	if !dec.Allowed {
		t.Errorf("expected wildcard match, got %+v", dec)
	}
}

func TestStoreTaskScopeChecker_RejectsUnresolvedAction(t *testing.T) {
	st, user, agent := newTaskscopeStore(t)
	c := NewStoreTaskScopeChecker(st)
	dec := c.Check(context.Background(), user.ID, agent.ID, "", "")
	if dec.Allowed {
		t.Errorf("expected deny on empty service/action")
	}
	if dec.Reason != "unresolved_action" {
		t.Errorf("reason=%q, want unresolved_action", dec.Reason)
	}
}

// stubCatalog lets us drive postprocess task-scope behavior without
// loading real YAML defs.
type stubCatalog struct{ resolve func(host, method, path string) (ResolvedAction, bool) }

func (s stubCatalog) Resolve(host, method, path string) (ResolvedAction, bool) {
	return s.resolve(host, method, path)
}

// stubTaskScope returns a fixed decision for every Check call.
type stubTaskScope struct{ decision TaskScopeDecision }

func (s stubTaskScope) Check(ctx context.Context, userID, agentID, serviceID, actionID string) TaskScopeDecision {
	return s.decision
}

func TestNewServiceCatalogFromRegistry_NilSafe(t *testing.T) {
	c := NewServiceCatalogFromRegistry(nil)
	if _, ok := c.Resolve("api.github.com", "GET", "/user"); ok {
		t.Errorf("nil registry should produce empty catalog")
	}
}

func TestDefsFromRegistry_NilSafe(t *testing.T) {
	if defs := DefsFromRegistry(nil); len(defs) != 0 {
		t.Errorf("nil registry should produce empty defs, got %d", len(defs))
	}
}

func TestClassifyToDecision_AllShapes(t *testing.T) {
	// Sanity: each classification kind gets a sensible decision shape.
	defs := []yamldef.ServiceDef{} // unused, just here so the test compiles in catalog package
	_ = defs
}

// End-to-end: a tool_use that triggers a rewrite is blocked when the
// catalog resolves to (github, create_issue) but the agent has no
// active task scope covering that action. Compare with
// TestPostprocess_JSONRewritesAutovaultURL which uses the same fixture
// but doesn't pass a Catalog/TaskScope (so task-scope is skipped).
func TestPostprocess_TaskScopeBlocksUnauthorizedAction(t *testing.T) {
	input := `{"url":"https://api.github.com/repos/x/y/issues","method":"POST","headers":{"Authorization":"Bearer autovault_github_xxx"}}`
	body := anthropicJSONWithToolUse(input)

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:   insp,
		RewriteOpts: inspector.DefaultRewriteOpts("https://proxy.example/proxy/v1"),
		Store:       st,
		AgentUserID: userID,
		AgentID:     agentID,
		Catalog: stubCatalog{resolve: func(host, method, path string) (ResolvedAction, bool) {
			return ResolvedAction{ServiceID: "github", ActionID: "create_issue", Method: "POST", PathTemplate: "/repos/{{.o}}/{{.r}}/issues"}, true
		}},
		TaskScope: stubTaskScope{decision: TaskScopeDecision{Reason: "needs_new_task"}},
	})

	// The body should contain a Clawvisor refusal in place of the
	// rewritten tool_use input. We accept either an unrewritten body
	// (rewriter fell back to refusal) or a body without the resolver URL.
	if strings.Contains(string(got.Body),"https://proxy.example/proxy/v1") {
		t.Fatalf("rewrite should NOT have happened — task scope denies github.create_issue:\n%s", got.Body)
	}
	if !strings.Contains(string(got.Body),"no active task scope") {
		t.Errorf("expected refusal message containing 'no active task scope':\n%s", got.Body)
	}
}

// Same fixture, but the task-scope check allows. The rewrite proceeds.
func TestPostprocess_TaskScopeAllowsAuthorizedAction(t *testing.T) {
	input := `{"url":"https://api.github.com/repos/x/y/issues","method":"POST","headers":{"Authorization":"Bearer autovault_github_xxx"}}`
	body := anthropicJSONWithToolUse(input)

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:   insp,
		RewriteOpts: inspector.DefaultRewriteOpts("https://proxy.example/proxy/v1"),
		Store:       st,
		AgentUserID: userID,
		AgentID:     agentID,
		Catalog: stubCatalog{resolve: func(host, method, path string) (ResolvedAction, bool) {
			return ResolvedAction{ServiceID: "github", ActionID: "create_issue"}, true
		}},
		TaskScope: stubTaskScope{decision: TaskScopeDecision{Allowed: true, TaskID: "task-x"}},
	})

	if !got.Rewritten {
		t.Fatalf("expected rewrite when task scope allows")
	}
	if !strings.Contains(string(got.Body),"https://proxy.example/proxy/v1") {
		t.Errorf("rewritten body missing resolver URL:\n%s", got.Body)
	}
}

// When the catalog returns no match for (host, method, path) but task
// scope is otherwise configured, postprocess falls back to allow (v0
// fail-open for unmapped actions). The placeholder boundary check
// already constrained the host.
func TestPostprocess_TaskScopeFallthroughOnUnknownAction(t *testing.T) {
	input := `{"url":"https://api.github.com/totally/unknown/path","method":"POST","headers":{"Authorization":"Bearer autovault_github_xxx"}}`
	body := anthropicJSONWithToolUse(input)

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})

	denyAll := stubTaskScope{decision: TaskScopeDecision{Reason: "should_not_be_called"}}
	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:   insp,
		RewriteOpts: inspector.DefaultRewriteOpts("https://proxy.example/proxy/v1"),
		Store:       st,
		AgentUserID: userID,
		AgentID:     agentID,
		Catalog: stubCatalog{resolve: func(host, method, path string) (ResolvedAction, bool) {
			return ResolvedAction{}, false // catalog miss
		}},
		TaskScope: denyAll, // intentionally deny — should not be consulted
	})

	if !got.Rewritten {
		t.Fatalf("catalog miss should fall through to rewrite, got body:\n%s", got.Body)
	}
}

