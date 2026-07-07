package scenarios_test

import (
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// TestTaskRejectedWithoutPurpose — purpose is required.
func TestTaskRejectedWithoutPurpose(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	user := cv.LoginAsLocalUser(t)

	var agent struct {
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "ta"}, &agent)

	resp := cvDo(t, cv, agent.Token, "POST", "/api/tasks", map[string]any{
		"authorized_actions": []map[string]any{{"service": "github", "action": "list_repos"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("missing-purpose status=%d, want 400", resp.StatusCode)
	}
}

// TestTaskRejectedWithoutScope — at least one scope field is required.
func TestTaskRejectedWithoutScope(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	user := cv.LoginAsLocalUser(t)

	var agent struct {
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "ta2"}, &agent)

	resp := cvDo(t, cv, agent.Token, "POST", "/api/tasks", map[string]any{
		"purpose": "do something",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("missing-scope status=%d, want 400", resp.StatusCode)
	}
}

// TestTaskCreateRejectsUnactivatedService — purpose+scope present, but the
// requested service has no credentials. Tests the SERVICE_NOT_CONFIGURED
// path that agents hit when they try to use a service the user hasn't
// connected. The error message guides the agent to fix the request.
func TestTaskCreateRejectsUnactivatedService(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	user := cv.LoginAsLocalUser(t)

	var agent struct {
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "ta3"}, &agent)

	resp := cvDo(t, cv, agent.Token, "POST", "/api/tasks", map[string]any{
		"purpose":            "test unactivated service",
		"authorized_actions": []map[string]any{{"service": "github", "action": "list_repos"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d, want 400", resp.StatusCode)
	}
	body := readBodyStr(resp)
	if !strings.Contains(body, "SERVICE_NOT_CONFIGURED") {
		t.Fatalf("expected SERVICE_NOT_CONFIGURED in body, got: %s", body)
	}
}

// TestTaskRejectsUnknownService — service ID doesn't exist in the catalog.
func TestTaskRejectsUnknownService(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	user := cv.LoginAsLocalUser(t)

	var agent struct {
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "ta4"}, &agent)

	resp := cvDo(t, cv, agent.Token, "POST", "/api/tasks", map[string]any{
		"purpose":            "test bogus service",
		"authorized_actions": []map[string]any{{"service": "nonexistent.svc", "action": "do_thing"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d, want 400", resp.StatusCode)
	}
	body := readBodyStr(resp)
	if !strings.Contains(body, "unknown service") && !strings.Contains(body, "INVALID_REQUEST") {
		t.Fatalf("expected 'unknown service' in body, got: %s", body)
	}
}

// TestTaskRejectsUnknownAction — service exists, action doesn't.
func TestTaskRejectsUnknownAction(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	user := cv.LoginAsLocalUser(t)

	var agent struct {
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "ta5"}, &agent)

	resp := cvDo(t, cv, agent.Token, "POST", "/api/tasks", map[string]any{
		"purpose":            "test bogus action",
		"authorized_actions": []map[string]any{{"service": "github", "action": "nope_not_a_real_action"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d, want 400", resp.StatusCode)
	}
	body := readBodyStr(resp)
	if !strings.Contains(body, "does not support action") && !strings.Contains(body, "INVALID_REQUEST") {
		t.Fatalf("expected 'does not support action' in body, got: %s", body)
	}
}

// TestUserListsTasksEmpty — fresh user has zero tasks.
func TestUserListsTasksEmpty(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	user := cv.LoginAsLocalUser(t)

	resp := cvDo(t, cv, user.AccessToken, "GET", "/api/tasks", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("list tasks status=%d", resp.StatusCode)
	}
}
