package scenarios_test

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// mintInvite mints a single-use member invite as an admin and returns the
// plaintext cvinv_ token (revealed once). Mirrors what the Terraform
// clawvisor_user resource does under the hood (POST /api/users/invites).
func mintInvite(t *testing.T, cv *testapp.Server, adminTok, email, role string) string {
	t.Helper()
	body := map[string]any{}
	if email != "" {
		body["email"] = email
	}
	if role != "" {
		body["role"] = role
	}
	var out struct {
		InviteToken string `json:"invite_token"`
		InviteURL   string `json:"invite_url"`
	}
	cvPost(t, cv, adminTok, "/api/users/invites", body, &out)
	if out.InviteToken == "" {
		t.Fatalf("invite mint returned empty token")
	}
	return out.InviteToken
}

// enrollResp is the /api/agents/enroll success shape.
type enrollResp struct {
	AgentID string `json:"agent_id"`
	UserID  string `json:"user_id"`
	Token   string `json:"token"`
	Status  string `json:"status"`
}

// enroll POSTs /api/agents/enroll UNAUTHENTICATED (as the installer script
// does): the invite rides in the body, the server claims it and returns a
// per-user agent token. This is exactly the round-trip the `--invite-stdin`
// shell path performs (printf "$CV_INVITE" | curl … /api/agents/enroll).
func enroll(t *testing.T, cv *testapp.Server, invite, name string) enrollResp {
	t.Helper()
	var out enrollResp
	cvPost(t, cv, "", "/api/agents/enroll",
		map[string]any{"invite_token": invite, "name": name}, &out)
	return out
}

// TestEnrollViaInvite_FullFlow drives the whole gap-fixed installer path over
// HTTP against a live testapp server: admin mints an invite → the (unauth)
// enroll endpoint the `--invite-stdin` script calls claims it → a member
// account materializes, verified inline → a per-user agent is registered under
// that member → the member's LLM request round-trips through proxy-lite and is
// attributed to the member (by agent ownership), NOT the admin.
func TestEnrollViaInvite_FullFlow(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	// Observe/passthrough posture — the invite install flow's default posture,
	// where the member keeps their own provider key and it flows through.
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC":   upstream.URL(),
		"CLAWVISOR_PROXY_LITE_UPSTREAM_AUTH": "passthrough",
	})
	admin := cv.LoginAsLocalUser(t)

	const memberEmail = "employee@example.com"
	invite := mintInvite(t, cv, admin.AccessToken, memberEmail, "member")

	// The installer's stdin path: claim + register in one unauthenticated call.
	got := enroll(t, cv, invite, "employee-laptop")
	if got.Token == "" || got.AgentID == "" || got.UserID == "" {
		t.Fatalf("enroll returned incomplete result: %+v", got)
	}
	if got.Status != "approved" {
		t.Fatalf("enroll status = %q, want approved", got.Status)
	}

	// Attribution: the new agent is owned by a fresh member, not the admin.
	if got.UserID == admin.UserID {
		t.Fatalf("enrolled agent attributed to the admin (%s); expected a new member", admin.UserID)
	}

	// The member exists as a verified, member-role user matching the enroll id.
	var users struct {
		Users []struct {
			ID       string `json:"id"`
			Email    string `json:"email"`
			Role     string `json:"role"`
			Verified bool   `json:"verified"`
		} `json:"users"`
	}
	cvGet(t, cv, admin.AccessToken, "/api/users", &users)
	var found bool
	for _, u := range users.Users {
		if u.ID != got.UserID {
			continue
		}
		found = true
		if u.Email != memberEmail {
			t.Fatalf("enrolled user email = %q, want %q", u.Email, memberEmail)
		}
		if u.Role != "member" {
			t.Fatalf("enrolled user role = %q, want member", u.Role)
		}
		if !u.Verified {
			t.Fatal("enrolled user must be verified inline (magic-link claim == possession proof)")
		}
	}
	if !found {
		t.Fatalf("enrolled user %s not present in /api/users", got.UserID)
	}

	// The agent belongs to the member: it must NOT appear under the admin's
	// own account (ownership attribution, same invariant as token-minted
	// agents).
	var adminAgents []map[string]any
	cvGet(t, cv, admin.AccessToken, "/api/agents", &adminAgents)
	if agentListed(adminAgents, got.AgentID) {
		t.Fatal("member's enrolled agent must not be listed under the admin account")
	}

	// The member's request round-trips through proxy-lite (passthrough) with
	// the enrolled cvis_ token driving it — proof the token is live and the
	// request is attributed to the member's agent.
	const memberBearer = "Bearer ant-employee-passthrough-token"
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("X-Clawvisor-Agent-Token", got.Token)
	req.Header.Set("Authorization", memberBearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("/api/v1/messages: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("member LLM request status=%d body=%s", resp.StatusCode, body)
	}
	if upstream.Count() != 1 {
		t.Fatalf("upstream hits=%d, want 1", upstream.Count())
	}
	if got := upstream.Last().Headers.Get("Authorization"); got != memberBearer {
		t.Fatalf("upstream Authorization=%q, want the member's passthrough bearer", got)
	}

	// Single-use: the invite is burned — a second enroll is rejected.
	resp2 := cvDo(t, cv, "", "POST", "/api/agents/enroll",
		map[string]any{"invite_token": invite, "name": "second-laptop"})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("re-enroll with used invite = %d, want 409", resp2.StatusCode)
	}
	if !bodyHasCodeStr(readBodyStr(resp2), "INVITE_ALREADY_USED") {
		t.Fatal("re-enroll should return INVITE_ALREADY_USED")
	}
}

// TestEnrollViaInvite_MemberOnlyDowngrade — invite security rule 1: an invite
// that pinned role:admin, when claimed over the unauthenticated enrollment
// channel, may only ever produce a member. Admin is granted deliberately by an
// existing admin (PUT /api/users/{id}/role), never by riding an invite through
// the installer.
func TestEnrollViaInvite_MemberOnlyDowngrade(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	admin := cv.LoginAsLocalUser(t)

	// A pinned-email admin invite is allowed at mint time (only *any-email*
	// admin invites are rejected). The downgrade happens at claim.
	invite := mintInvite(t, cv, admin.AccessToken, "boss@example.com", "admin")
	got := enroll(t, cv, invite, "boss-laptop")

	var users struct {
		Users []struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"users"`
	}
	cvGet(t, cv, admin.AccessToken, "/api/users", &users)
	for _, u := range users.Users {
		if u.ID == got.UserID && u.Role != "member" {
			t.Fatalf("invite-enrolled account role = %q, want member (rule 1 downgrade)", u.Role)
		}
	}
}

// TestEnrollViaInvite_InvalidInviteRejected — a bogus / unknown invite token is
// rejected, and no agent or account is created.
func TestEnrollViaInvite_InvalidInviteRejected(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	cv.LoginAsLocalUser(t)

	resp := cvDo(t, cv, "", "POST", "/api/agents/enroll",
		map[string]any{"invite_token": "cvinv_deadbeef", "name": "nope"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("enroll with bogus invite = %d, want 403", resp.StatusCode)
	}
}
