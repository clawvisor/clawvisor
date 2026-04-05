package smoke_test

import (
	"net/http"
	"testing"
)

func TestAuditLog(t *testing.T) {
	env := setup(t)

	resp := env.userDo("GET", "/api/audit", nil)
	m := mustStatus(t, resp, http.StatusOK)

	entries, ok := m["entries"].([]any)
	if !ok {
		t.Log("audit endpoint returned 200, entries may be empty or keyed differently")
		return
	}
	t.Logf("audit log has %d entries", len(entries))
}

func TestRestrictionsList(t *testing.T) {
	env := setup(t)

	// Restrictions returns a JSON array (not wrapped in an object).
	resp := env.userDo("GET", "/api/restrictions", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var restrictions []any
	decodeJSON(t, resp, &restrictions)
	t.Logf("restrictions: %d entries", len(restrictions))
}

func TestTasksList(t *testing.T) {
	env := setup(t)

	resp := env.userDo("GET", "/api/tasks", nil)
	m := mustStatus(t, resp, http.StatusOK)

	tasks, ok := m["tasks"].([]any)
	if ok {
		t.Logf("found %d task(s)", len(tasks))
	}
}

func TestApprovalsList(t *testing.T) {
	env := setup(t)

	resp := env.userDo("GET", "/api/approvals", nil)
	mustStatus(t, resp, http.StatusOK)
}
