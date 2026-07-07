package scenarios_test

import (
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// TestRestrictionCRUD — user creates, lists, deletes restrictions.
func TestRestrictionCRUD(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	user := cv.LoginAsLocalUser(t)

	// Create a restriction.
	var created struct {
		ID      string `json:"id"`
		Service string `json:"service"`
		Action  string `json:"action"`
	}
	cvPost(t, cv, user.AccessToken, "/api/restrictions", map[string]any{
		"service": "github",
		"action":  "delete_repo",
		"reason":  "test restriction",
	}, &created)
	if created.ID == "" {
		t.Fatal("create: no ID")
	}

	// List includes it.
	var list []map[string]any
	cvGet(t, cv, user.AccessToken, "/api/restrictions", &list)
	found := false
	for _, r := range list {
		if r["id"] == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("restriction not in list (%d entries)", len(list))
	}

	// Delete it.
	cvDelete(t, cv, user.AccessToken, "/api/restrictions/"+created.ID)
}

// TestRestrictionWildcard — `*` action blocks all actions on a service.
func TestRestrictionWildcard(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	user := cv.LoginAsLocalUser(t)

	var created struct {
		ID string `json:"id"`
	}
	cvPost(t, cv, user.AccessToken, "/api/restrictions", map[string]any{
		"service": "slack",
		"action":  "*",
		"reason":  "block all slack",
	}, &created)
	if created.ID == "" {
		t.Fatal("create wildcard: no ID")
	}
}

// TestRestrictionRequiresAuth — unauthenticated POST is rejected.
func TestRestrictionRequiresAuth(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	resp := cvDo(t, cv, "", "POST", "/api/restrictions", map[string]any{
		"service": "x", "action": "*",
	})
	defer resp.Body.Close()
	if resp.StatusCode == 200 || resp.StatusCode == 201 {
		t.Fatalf("unauth restriction was allowed (status=%d)", resp.StatusCode)
	}
}

// TestAuditListReturnsJSON — empty list on a fresh install.
func TestAuditListReturnsJSON(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	user := cv.LoginAsLocalUser(t)

	resp := cvDo(t, cv, user.AccessToken, "GET", "/api/audit", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("audit list status=%d", resp.StatusCode)
	}
	body := readBodyStr(resp)
	if !strings.Contains(body, "[") && !strings.Contains(body, "{") {
		t.Fatalf("audit list returned non-JSON: %q", body)
	}
}

// TestAuditMutesCRUD — create/list/delete audit-mute patterns.
func TestAuditMutesCRUD(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	user := cv.LoginAsLocalUser(t)

	var created struct {
		ID string `json:"id"`
	}
	cvPost(t, cv, user.AccessToken, "/api/audit/mutes", map[string]any{
		"host":   "api.github.com",
		"path":   "/user/repos",
		"reason": "routine list call",
	}, &created)
	if created.ID == "" {
		t.Fatal("audit mute create: no ID")
	}

	// List — assert the created mute appears (a 200 with an empty array
	// is not enough — that's the failure mode this test is here to catch).
	// Envelope shape is {"entries": [...], "total": N}, matching audit.go's
	// ListMutes handler.
	var list struct {
		Entries []map[string]any `json:"entries"`
		Total   int              `json:"total"`
	}
	cvGet(t, cv, user.AccessToken, "/api/audit/mutes", &list)
	found := false
	for _, m := range list.Entries {
		if id, _ := m["id"].(string); id == created.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("audit mute %s not in list response (%d entries): %+v", created.ID, list.Total, list.Entries)
	}

	// Delete.
	cvDelete(t, cv, user.AccessToken, "/api/audit/mutes/"+created.ID)
}
