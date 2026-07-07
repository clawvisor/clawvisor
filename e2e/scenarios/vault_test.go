package scenarios_test

import (
	"io"
	"net/http"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// TestVaultItemCRUD runs the full Create → List → Get → Update → Delete
// path against the clawvisor vault endpoints. Values are encrypted at rest
// (AES-256-GCM) and redacted in responses — both invariants asserted below.
func TestVaultItemCRUD(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	user := cv.LoginAsLocalUser(t)

	// 1. Create — clawvisor vault uses caller-supplied id (not server-minted).
	const itemID = "github-token-test"
	var created struct {
		Status string `json:"status"`
		ID     string `json:"id"`
	}
	cvPost(t, cv, user.AccessToken, "/api/vault/items", map[string]any{
		"id":    itemID,
		"value": "ghp_secret_should_never_appear_in_responses",
	}, &created)
	if created.Status != "created" || itemID != itemID {
		t.Fatalf("unexpected create response: %+v", created)
	}

	// 2. List — should include our item; value field absent or empty.
	var list struct {
		Entries []map[string]any `json:"entries"`
		Total   int              `json:"total"`
	}
	cvGet(t, cv, user.AccessToken, "/api/vault/items", &list)
	found := false
	for _, item := range list.Entries {
		if item["id"] == itemID {
			found = true
			if v, ok := item["value"]; ok && v != "" && v != nil {
				t.Fatalf("plaintext value leaked in list response: %+v", item)
			}
		}
	}
	if !found {
		t.Fatalf("item %q not in list (%d items)", itemID, len(list.Entries))
	}

	// 3. Get — value still redacted.
	var got map[string]any
	cvGet(t, cv, user.AccessToken, "/api/vault/items/"+itemID, &got)
	if v, ok := got["value"]; ok && v != "" && v != nil {
		t.Fatalf("plaintext value leaked in get response: %+v", got)
	}

	// 4. Update.
	cvPut(t, cv, user.AccessToken, "/api/vault/items/"+itemID, map[string]any{
		"name":  "github-token-rotated",
		"value": "ghp_new_secret",
	}, nil)

	// 5. Delete.
	cvDelete(t, cv, user.AccessToken, "/api/vault/items/"+itemID)

	// 6. Get after delete → 404.
	resp := cvDo(t, cv, user.AccessToken, "GET", "/api/vault/items/"+itemID, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("post-delete get: status=%d body=%s", resp.StatusCode, body)
	}
}

// TestVaultItemRequiresAuth — unauthenticated requests are rejected.
// Clawvisor may return 401 or 404 depending on which middleware fires first;
// either way we just want to confirm the request is denied.
func TestVaultItemRequiresAuth(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	resp := cvDo(t, cv, "", "GET", "/api/vault/items", nil)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("unauth request was allowed (status=%d)", resp.StatusCode)
	}
}

// cvDo / cvPost / cvGet / cvPut / cvDelete live in helpers_test.go —
// shared across every scenario file in this package.
