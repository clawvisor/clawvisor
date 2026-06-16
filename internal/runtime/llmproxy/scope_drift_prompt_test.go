package llmproxy

import (
	"strings"
	"testing"
)

func TestRenderScopeDriftMenu_ContainsThreeOptionsAndImplicitPath(t *testing.T) {
	t.Parallel()
	got := renderScopeDriftMenu(MenuFields{
		DriftID:    "drift-x",
		Service:    "github",
		Action:     "post_issue",
		TaskID:     "task-1",
		ReasonText: "no covering scope",
		Source:     ScopeDriftSourceTaskScope,
	}, "https://clawvisor.local")
	for _, want := range []string{
		"drift-x",
		"(a) Expand",
		"task-1/expand",
		"drift_id",
		"(b) Create a new task",
		"(c) One-off",
		"<clawvisor:decision",
		"(implicit)",
		"github.post_issue",
		"no covering scope",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("menu missing %q\nfull:\n%s", want, got)
		}
	}
}

func TestRenderScopeDriftMenu_NoTaskIDExplainsSkipA(t *testing.T) {
	t.Parallel()
	got := renderScopeDriftMenu(MenuFields{
		DriftID: "drift-y",
		Service: "svc",
		Action:  "act",
	}, "https://clawvisor.local")
	if !strings.Contains(got, "No active task was matched") {
		t.Errorf("menu without TaskID should explain skip-a; got:\n%s", got)
	}
	// Should NOT render a /tasks/<id>/expand line.
	if strings.Contains(got, "/tasks//expand") {
		t.Errorf("menu rendered malformed expand URL: %s", got)
	}
}

func TestRenderScopeDriftMenu_EmptyDriftIDIsDefensive(t *testing.T) {
	t.Parallel()
	got := renderScopeDriftMenu(MenuFields{}, "https://clawvisor.local")
	if !strings.Contains(got, "no drift record was minted") {
		t.Fatalf("empty DriftID should produce defensive fallback; got: %s", got)
	}
}
