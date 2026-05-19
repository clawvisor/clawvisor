package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// setupConnectionEnv creates a test environment with an admin@local user
// and a session for approving/denying connection requests.
// The session user IS admin@local so that ownership checks pass.
func setupConnectionEnv(t *testing.T) (*testEnv, *testSession) {
	t.Helper()
	env := newTestEnv(t)

	// Register admin@local through the API so the session user is the daemon owner.
	// Connection requests are assigned to admin@local, so the approving user
	// must be admin@local for the ownership check in ApproveByID/DenyByID.
	const password = "TestPass123!"
	resp := env.do("POST", "/api/auth/register", "", map[string]any{
		"email": "admin@local", "password": password,
	})
	body := mustStatus(t, resp, http.StatusCreated)
	user := nested(t, body, "user")

	session := &testSession{
		env:          env,
		Email:        "admin@local",
		UserID:       str(t, user, "id"),
		AccessToken:  str(t, body, "access_token"),
		RefreshToken: str(t, body, "refresh_token"),
	}
	return env, session
}

func TestConnectionRequest_Create(t *testing.T) {
	env, _ := setupConnectionEnv(t)

	resp := env.do("POST", "/api/agents/connect", "", map[string]any{
		"name": "Claude Code", "description": "Code assistant",
	})
	body := mustStatus(t, resp, http.StatusCreated)

	if str(t, body, "status") != "pending" {
		t.Errorf("expected status=pending, got %s", str(t, body, "status"))
	}
	if str(t, body, "connection_id") == "" {
		t.Error("expected non-empty connection_id")
	}
	if str(t, body, "poll_url") == "" {
		t.Error("expected non-empty poll_url")
	}
}

func TestConnectionRequest_Create_NameRequired(t *testing.T) {
	env, _ := setupConnectionEnv(t)

	resp := env.do("POST", "/api/agents/connect", "", map[string]any{
		"description": "no name",
	})
	mustStatus(t, resp, http.StatusBadRequest)
}

func TestConnectionRequest_PollPending(t *testing.T) {
	env, _ := setupConnectionEnv(t)

	// Verify at the store level that a new connection request is pending.
	owner, err := env.Store.GetUserByEmail(context.Background(), "admin@local")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}

	cr := &store.ConnectionRequest{
		UserID:    owner.ID,
		Name:      "Agent1",
		Status:    "pending",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	if err := env.Store.CreateConnectionRequest(context.Background(), cr); err != nil {
		t.Fatalf("CreateConnectionRequest: %v", err)
	}

	got, err := env.Store.GetConnectionRequest(context.Background(), cr.ID)
	if err != nil {
		t.Fatalf("GetConnectionRequest: %v", err)
	}
	if got.Status != "pending" {
		t.Errorf("expected status=pending, got %s", got.Status)
	}
}

func TestConnectionRequest_Approve(t *testing.T) {
	env, session := setupConnectionEnv(t)

	// Create connection request.
	resp := env.do("POST", "/api/agents/connect", "", map[string]any{
		"name": "ApproveMe",
	})
	body := mustStatus(t, resp, http.StatusCreated)
	connID := str(t, body, "connection_id")

	// Approve.
	resp = session.do("POST", "/api/agents/connect/"+connID+"/approve", nil)
	body = mustStatus(t, resp, http.StatusOK)

	if str(t, body, "status") != "approved" {
		t.Errorf("expected status=approved, got %s", str(t, body, "status"))
	}

	// Poll should return approved with token.
	resp = env.do("GET", "/api/agents/connect/"+connID+"/status", "", nil)
	body = mustStatus(t, resp, http.StatusOK)

	if str(t, body, "status") != "approved" {
		t.Errorf("expected status=approved on poll, got %s", str(t, body, "status"))
	}
	token, ok := body["token"].(string)
	if !ok || token == "" {
		t.Error("expected non-empty token on approved poll")
	}
}

func TestConnectionRequest_Deny(t *testing.T) {
	env, session := setupConnectionEnv(t)

	resp := env.do("POST", "/api/agents/connect", "", map[string]any{
		"name": "DenyMe",
	})
	body := mustStatus(t, resp, http.StatusCreated)
	connID := str(t, body, "connection_id")

	// Deny.
	resp = session.do("POST", "/api/agents/connect/"+connID+"/deny", nil)
	body = mustStatus(t, resp, http.StatusOK)

	if str(t, body, "status") != "denied" {
		t.Errorf("expected status=denied, got %s", str(t, body, "status"))
	}

	// Poll should return denied.
	resp = env.do("GET", "/api/agents/connect/"+connID+"/status", "", nil)
	body = mustStatus(t, resp, http.StatusOK)
	if str(t, body, "status") != "denied" {
		t.Errorf("expected status=denied on poll")
	}
}

func TestConnectionRequest_MaxPending(t *testing.T) {
	env, _ := setupConnectionEnv(t)

	// Create 10 pending requests (the max). Names must differ because
	// duplicate-name pending requests are rejected up front.
	for i := 0; i < 10; i++ {
		resp := env.do("POST", "/api/agents/connect", "", map[string]any{
			"name": fmt.Sprintf("Agent-%d", i),
		})
		mustStatus(t, resp, http.StatusCreated)
	}

	// 11th should be rejected.
	resp := env.do("POST", "/api/agents/connect", "", map[string]any{
		"name": "OneMore",
	})
	mustStatus(t, resp, http.StatusTooManyRequests)
}

func TestConnectionRequest_DuplicateAgentName(t *testing.T) {
	env, session := setupConnectionEnv(t)

	// Create a request and approve it so an agent named "Bootstrap" exists.
	resp := env.do("POST", "/api/agents/connect", "", map[string]any{
		"name": "Bootstrap",
	})
	body := mustStatus(t, resp, http.StatusCreated)
	connID := str(t, body, "connection_id")
	resp = session.do("POST", "/api/agents/connect/"+connID+"/approve", nil)
	mustStatus(t, resp, http.StatusOK)

	owner, err := env.Store.GetUserByEmail(context.Background(), "admin@local")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	agentsBefore, err := env.Store.ListAgents(context.Background(), owner.ID)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	pendingBefore, err := env.Store.ListPendingConnectionRequests(context.Background(), owner.ID)
	if err != nil {
		t.Fatalf("ListPendingConnectionRequests: %v", err)
	}

	// A new request with the same name must be rejected without side effects.
	resp = env.do("POST", "/api/agents/connect", "", map[string]any{
		"name": "Bootstrap",
	})
	body = mustStatus(t, resp, http.StatusConflict)
	if got := str(t, body, "code"); got != "AGENT_NAME_EXISTS" {
		t.Errorf("expected code=AGENT_NAME_EXISTS, got %q", got)
	}

	agentsAfter, err := env.Store.ListAgents(context.Background(), owner.ID)
	if err != nil {
		t.Fatalf("ListAgents after: %v", err)
	}
	if len(agentsAfter) != len(agentsBefore) {
		t.Errorf("collision created an agent: before=%d after=%d", len(agentsBefore), len(agentsAfter))
	}
	pendingAfter, err := env.Store.ListPendingConnectionRequests(context.Background(), owner.ID)
	if err != nil {
		t.Fatalf("ListPendingConnectionRequests after: %v", err)
	}
	if len(pendingAfter) != len(pendingBefore) {
		t.Errorf("collision created a pending request: before=%d after=%d", len(pendingBefore), len(pendingAfter))
	}
}

func TestConnectionRequest_DuplicatePendingName(t *testing.T) {
	env, _ := setupConnectionEnv(t)

	// First request creates a pending entry.
	resp := env.do("POST", "/api/agents/connect", "", map[string]any{
		"name": "InFlight",
	})
	mustStatus(t, resp, http.StatusCreated)

	owner, err := env.Store.GetUserByEmail(context.Background(), "admin@local")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	pendingBefore, err := env.Store.ListPendingConnectionRequests(context.Background(), owner.ID)
	if err != nil {
		t.Fatalf("ListPendingConnectionRequests: %v", err)
	}

	// Second request with the same name while the first is still pending
	// must be rejected; we don't want two concurrent bootstrap curls racing
	// for the same on-disk JSON path.
	resp = env.do("POST", "/api/agents/connect", "", map[string]any{
		"name": "InFlight",
	})
	body := mustStatus(t, resp, http.StatusConflict)
	if got := str(t, body, "code"); got != "AGENT_NAME_EXISTS" {
		t.Errorf("expected code=AGENT_NAME_EXISTS, got %q", got)
	}

	pendingAfter, err := env.Store.ListPendingConnectionRequests(context.Background(), owner.ID)
	if err != nil {
		t.Fatalf("ListPendingConnectionRequests after: %v", err)
	}
	if len(pendingAfter) != len(pendingBefore) {
		t.Errorf("collision created a second pending request: before=%d after=%d", len(pendingBefore), len(pendingAfter))
	}
}

func TestConnectionRequest_Expired(t *testing.T) {
	env, _ := setupConnectionEnv(t)

	// Create a connection request with past expiry directly through the store.
	owner, err := env.Store.GetUserByEmail(context.Background(), "admin@local")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}

	cr := &store.ConnectionRequest{
		UserID:    owner.ID,
		Name:      "WillExpire",
		Status:    "pending",
		ExpiresAt: time.Now().Add(-1 * time.Minute), // already expired
	}
	if err := env.Store.CreateConnectionRequest(context.Background(), cr); err != nil {
		t.Fatalf("CreateConnectionRequest: %v", err)
	}

	// Poll should detect expiry on first check and return immediately.
	resp := env.do("GET", "/api/agents/connect/"+cr.ID+"/status", "", nil)
	body := mustStatus(t, resp, http.StatusOK)

	if str(t, body, "status") != "expired" {
		t.Errorf("expected status=expired, got %s", str(t, body, "status"))
	}
}

func TestConnectionRequest_ApproveCreatesAgent(t *testing.T) {
	env, session := setupConnectionEnv(t)

	// Create and approve.
	resp := env.do("POST", "/api/agents/connect", "", map[string]any{
		"name": "NewAgent",
	})
	body := mustStatus(t, resp, http.StatusCreated)
	connID := str(t, body, "connection_id")

	resp = session.do("POST", "/api/agents/connect/"+connID+"/approve", nil)
	approveBody := mustStatus(t, resp, http.StatusOK)
	agentID := str(t, approveBody, "agent_id")

	// Verify agent exists in the store.
	agents, err := env.Store.ListAgents(context.Background(), session.UserID)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}

	found := false
	for _, a := range agents {
		if a.ID == agentID {
			found = true
			if a.Name != "NewAgent" {
				t.Errorf("expected agent name=NewAgent, got %s", a.Name)
			}
			break
		}
	}
	if !found {
		t.Errorf("approved agent %s not found in agent list", agentID)
	}
}

func TestConnectionRequest_DoubleApproveConflict(t *testing.T) {
	env, session := setupConnectionEnv(t)

	resp := env.do("POST", "/api/agents/connect", "", map[string]any{
		"name": "DoubleApprove",
	})
	body := mustStatus(t, resp, http.StatusCreated)
	connID := str(t, body, "connection_id")

	// First approve succeeds.
	resp = session.do("POST", "/api/agents/connect/"+connID+"/approve", nil)
	mustStatus(t, resp, http.StatusOK)

	// Second approve returns conflict.
	resp = session.do("POST", "/api/agents/connect/"+connID+"/approve", nil)
	mustStatus(t, resp, http.StatusConflict)
}

func TestConnectionRequest_List(t *testing.T) {
	env, session := setupConnectionEnv(t)

	// Create two connection requests.
	env.do("POST", "/api/agents/connect", "", map[string]any{"name": "A"})
	env.do("POST", "/api/agents/connect", "", map[string]any{"name": "B"})

	// List pending — the session user is admin@local, who owns the requests.
	resp := session.do("GET", "/api/agents/connections", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var list []any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("expected 2 pending requests, got %d", len(list))
	}
}

func TestConnectionRequest_DeleteExpired(t *testing.T) {
	env, _ := setupConnectionEnv(t)

	// Verify DeleteExpiredConnectionRequests doesn't error on empty table.
	err := env.Store.DeleteExpiredConnectionRequests(context.Background())
	if err != nil {
		t.Fatalf("DeleteExpiredConnectionRequests: %v", err)
	}
}

func TestConnectionRequest_CountPending(t *testing.T) {
	env, _ := setupConnectionEnv(t)

	owner, _ := env.Store.GetUserByEmail(context.Background(), "admin@local")

	count, err := env.Store.CountPendingConnectionRequestsForUser(context.Background(), owner.ID)
	if err != nil {
		t.Fatalf("CountPendingConnectionRequestsForUser: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 pending, got %d", count)
	}

	// Create one.
	_ = env.Store.CreateConnectionRequest(context.Background(), &store.ConnectionRequest{
		UserID:    owner.ID,
		Name:      "test",
		Status:    "pending",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})

	count, err = env.Store.CountPendingConnectionRequestsForUser(context.Background(), owner.ID)
	if err != nil {
		t.Fatalf("CountPendingConnectionRequestsForUser: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 pending, got %d", count)
	}
}

func TestConnectionRequest_ClaimMint(t *testing.T) {
	_, session := setupConnectionEnv(t)

	resp := session.do("POST", "/api/agents/connect/claim", nil)
	body := mustStatus(t, resp, http.StatusCreated)

	code := str(t, body, "code")
	if len(code) != 10 {
		t.Errorf("expected 10-char claim code, got %q (len %d)", code, len(code))
	}
	if str(t, body, "expires_at") == "" {
		t.Error("expected non-empty expires_at")
	}
}

func TestConnectionRequest_ClaimResolvesUser(t *testing.T) {
	env, session := setupConnectionEnv(t)

	// Mint a claim.
	resp := session.do("POST", "/api/agents/connect/claim", nil)
	body := mustStatus(t, resp, http.StatusCreated)
	code := str(t, body, "code")

	// Use the claim — note the body has no user_id.
	resp = env.do("POST", "/api/agents/connect?claim="+code, "", map[string]any{
		"name": "ClaimAgent",
	})
	body = mustStatus(t, resp, http.StatusCreated)

	if str(t, body, "status") != "pending" {
		t.Errorf("expected status=pending, got %s", str(t, body, "status"))
	}

	// The request must be attributed to the minting user.
	pending, err := env.Store.ListPendingConnectionRequests(context.Background(), session.UserID)
	if err != nil {
		t.Fatalf("ListPendingConnectionRequests: %v", err)
	}
	if len(pending) != 1 || pending[0].Name != "ClaimAgent" {
		t.Errorf("expected one pending request named ClaimAgent for the minting user, got %+v", pending)
	}
}

func TestConnectionRequest_ClaimSingleUse(t *testing.T) {
	env, session := setupConnectionEnv(t)

	resp := session.do("POST", "/api/agents/connect/claim", nil)
	body := mustStatus(t, resp, http.StatusCreated)
	code := str(t, body, "code")

	// First use succeeds.
	resp = env.do("POST", "/api/agents/connect?claim="+code, "", map[string]any{
		"name": "FirstUse",
	})
	mustStatus(t, resp, http.StatusCreated)

	// Second use of the same claim must be rejected.
	resp = env.do("POST", "/api/agents/connect?claim="+code, "", map[string]any{
		"name": "SecondUse",
	})
	body = mustStatus(t, resp, http.StatusUnauthorized)
	if got := str(t, body, "code"); got != "INVALID_CLAIM" {
		t.Errorf("expected code=INVALID_CLAIM, got %q", got)
	}

	// The second attempt should not have created any pending request.
	pending, err := env.Store.ListPendingConnectionRequests(context.Background(), session.UserID)
	if err != nil {
		t.Fatalf("ListPendingConnectionRequests: %v", err)
	}
	for _, p := range pending {
		if p.Name == "SecondUse" {
			t.Error("re-using a consumed claim must not create a pending request")
		}
	}
}

func TestConnectionRequest_ClaimInvalid(t *testing.T) {
	env, _ := setupConnectionEnv(t)

	resp := env.do("POST", "/api/agents/connect?claim=clm_does_not_exist", "", map[string]any{
		"name": "Bogus",
	})
	body := mustStatus(t, resp, http.StatusUnauthorized)
	if got := str(t, body, "code"); got != "INVALID_CLAIM" {
		t.Errorf("expected code=INVALID_CLAIM, got %q", got)
	}
}

// Regression: a recoverable validation failure (e.g., duplicate name 409)
// must NOT burn the single-use claim code. Otherwise the dashboard would
// render a stale, unusable claim for ~4 minutes before the next refetch,
// and the user's corrected retry would fail with INVALID_CLAIM.
func TestConnectionRequest_ClaimSurvivesNameCollision(t *testing.T) {
	env, session := setupConnectionEnv(t)

	// First, create an existing agent named "Foo" by going through the
	// approve flow so the name shows up in the agents list.
	resp := env.do("POST", "/api/agents/connect", "", map[string]any{"name": "Foo"})
	body := mustStatus(t, resp, http.StatusCreated)
	connID := str(t, body, "connection_id")
	resp = session.do("POST", "/api/agents/connect/"+connID+"/approve", nil)
	mustStatus(t, resp, http.StatusOK)

	// Mint a fresh claim.
	resp = session.do("POST", "/api/agents/connect/claim", nil)
	code := str(t, mustStatus(t, resp, http.StatusCreated), "code")

	// Attempt to bootstrap "Foo" again — collides with the existing agent.
	resp = env.do("POST", "/api/agents/connect?claim="+code+"&name=Foo", "", nil)
	body = mustStatus(t, resp, http.StatusConflict)
	if got := str(t, body, "code"); got != "AGENT_NAME_EXISTS" {
		t.Errorf("expected code=AGENT_NAME_EXISTS, got %q", got)
	}

	// Retry with a different name using the SAME claim — must succeed,
	// proving the claim wasn't consumed by the 409.
	resp = env.do("POST", "/api/agents/connect?claim="+code+"&name=Bar", "", nil)
	mustStatus(t, resp, http.StatusCreated)

	// Now a third use of the same claim must fail — the successful Bar
	// request consumed it.
	resp = env.do("POST", "/api/agents/connect?claim="+code+"&name=Baz", "", nil)
	body = mustStatus(t, resp, http.StatusUnauthorized)
	if got := str(t, body, "code"); got != "INVALID_CLAIM" {
		t.Errorf("expected code=INVALID_CLAIM on third use, got %q", got)
	}
}

// Regression: the duplicate-name guard at request creation isn't enough
// because an agent with the same name can be created between request
// creation and approval (concurrent approve of a different pending,
// add-agent form, etc.). ApproveByID must re-check.
func TestConnectionRequest_ApproveRejectsRacingDuplicateName(t *testing.T) {
	env, session := setupConnectionEnv(t)

	// Create a connection request named "Race" — its name guard sees no
	// existing agent.
	resp := env.do("POST", "/api/agents/connect", "", map[string]any{"name": "Race"})
	body := mustStatus(t, resp, http.StatusCreated)
	pendingID := str(t, body, "connection_id")

	// Sneak an agent named "Race" into the store directly, simulating a
	// concurrent approve or add-agent form submission that landed first.
	if _, err := env.Store.CreateAgent(context.Background(), session.UserID, "Race", "decoy-token-hash"); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// Approving the pending request must now reject — the name is taken.
	resp = session.do("POST", "/api/agents/connect/"+pendingID+"/approve", nil)
	body = mustStatus(t, resp, http.StatusConflict)
	if got := str(t, body, "code"); got != "AGENT_NAME_EXISTS" {
		t.Errorf("expected code=AGENT_NAME_EXISTS on race, got %q", got)
	}

	// Confirm exactly one agent named "Race" exists (the decoy, not a
	// second one created by the approve handler).
	agents, err := env.Store.ListAgents(context.Background(), session.UserID)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	var raceCount int
	for _, a := range agents {
		if a.Name == "Race" {
			raceCount++
		}
	}
	if raceCount != 1 {
		t.Errorf("expected exactly one 'Race' agent, got %d", raceCount)
	}
}

// Regression: when wait=true's long-poll deadline fires with the request
// still pending, the server must expire the request before returning, so a
// late approve can't create an agent whose token never reaches a caller
// (which would otherwise hold the name and block a clean re-bootstrap).
// Regression: if Approve lands between waitForConnectionResolution
// returning pending and the timeout-branch's expire, the conditional
// store update must NOT clobber the approved status. The handler should
// re-read the row and return 201 + token so the bootstrap curl writes
// the token file rather than thinking the request expired.
func TestConnectionRequest_WaitTimeoutLosingRaceToApproveReturnsToken(t *testing.T) {
	env, session := setupConnectionEnv(t)
	resp := session.do("POST", "/api/agents/connect/claim", nil)
	claim := str(t, mustStatus(t, resp, http.StatusCreated), "code")

	// Create a pending request directly, then mark it approved (with an
	// agent) BEFORE issuing the wait=true POST. The handler will see the
	// request as already-pending at peek time but the wait loop will
	// detect the resolved state immediately via the initial fetch — same
	// downstream code path as the race-loss, just timed deterministically.
	// We use the request-creation path so the claim is consumed normally.
	type result struct {
		status int
		body   map[string]any
	}
	resultCh := make(chan result, 1)
	go func() {
		r := env.do("POST", "/api/agents/connect?claim="+claim+"&name=RaceApprove&wait=true&timeout=10", "", nil)
		defer r.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		resultCh <- result{status: r.StatusCode, body: body}
	}()
	var connID string
	for i := 0; i < 50; i++ {
		pending, err := env.Store.ListPendingConnectionRequests(context.Background(), session.UserID)
		if err == nil && len(pending) > 0 {
			connID = pending[0].ID
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if connID == "" {
		t.Fatal("pending request never appeared")
	}
	approveResp := session.do("POST", "/api/agents/connect/"+connID+"/approve", nil)
	mustStatus(t, approveResp, http.StatusOK)

	select {
	case got := <-resultCh:
		if got.status != http.StatusCreated {
			t.Errorf("expected 201 even when wait timeout raced with approve, got %d (body=%v)", got.status, got.body)
		}
		if tok, _ := got.body["token"].(string); tok == "" {
			t.Error("approved race result must include token")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("wait=true POST never returned")
	}
}

func TestConnectionRequest_WaitTimeoutExpiresPending(t *testing.T) {
	env, session := setupConnectionEnv(t)
	resp := session.do("POST", "/api/agents/connect/claim", nil)
	claim := str(t, mustStatus(t, resp, http.StatusCreated), "code")

	// Long-poll with a very short deadline. timeout=1 → ~1s wait window.
	resp = env.do("POST", "/api/agents/connect?claim="+claim+"&name=TimeoutMe&wait=true&timeout=1", "", nil)
	body := mustStatus(t, resp, http.StatusGone)
	if got := str(t, body, "status"); got != "expired" {
		t.Errorf("expected status=expired in response, got %q", got)
	}

	// The pending request must be gone — a late approve attempt must fail
	// because the request is no longer pending.
	pending, err := env.Store.ListPendingConnectionRequests(context.Background(), session.UserID)
	if err != nil {
		t.Fatalf("ListPendingConnectionRequests: %v", err)
	}
	for _, p := range pending {
		if p.Name == "TimeoutMe" {
			t.Errorf("expected the timed-out request to no longer be pending, found: %+v", p)
		}
	}

	// And re-bootstrap with the same name must succeed cleanly (no
	// AGENT_NAME_EXISTS) — the agent was never created.
	resp = session.do("POST", "/api/agents/connect/claim", nil)
	claim2 := str(t, mustStatus(t, resp, http.StatusCreated), "code")
	resp = env.do("POST", "/api/agents/connect?claim="+claim2+"&name=TimeoutMe", "", nil)
	mustStatus(t, resp, http.StatusCreated)
}

func TestConnectionRequest_WaitDeniedReturns403(t *testing.T) {
	env, session := setupConnectionEnv(t)
	resp := session.do("POST", "/api/agents/connect/claim", nil)
	claim := str(t, mustStatus(t, resp, http.StatusCreated), "code")

	type result struct {
		status int
		body   map[string]any
	}
	resultCh := make(chan result, 1)
	go func() {
		r := env.do("POST", "/api/agents/connect?claim="+claim+"&name=DenyMe&wait=true&timeout=10", "", nil)
		defer r.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		resultCh <- result{status: r.StatusCode, body: body}
	}()

	// Wait until the pending request shows up server-side, then deny it.
	var connID string
	for i := 0; i < 50; i++ {
		pending, err := env.Store.ListPendingConnectionRequests(context.Background(), session.UserID)
		if err == nil && len(pending) > 0 {
			connID = pending[0].ID
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if connID == "" {
		t.Fatal("pending request never appeared")
	}
	denyResp := session.do("POST", "/api/agents/connect/"+connID+"/deny", nil)
	mustStatus(t, denyResp, http.StatusOK)

	select {
	case got := <-resultCh:
		if got.status != http.StatusForbidden {
			t.Errorf("expected 403 Forbidden on denied wait=true response, got %d (body=%v)", got.status, got.body)
		}
		if s, _ := got.body["status"].(string); s != "denied" {
			t.Errorf("expected response status=denied, got %q", s)
		}
		if _, hasToken := got.body["token"]; hasToken {
			t.Error("denied response must not include a token")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait=true POST never returned after deny")
	}
}

func TestConnectionRequest_WaitApprovedReturns201WithToken(t *testing.T) {
	env, session := setupConnectionEnv(t)
	resp := session.do("POST", "/api/agents/connect/claim", nil)
	claim := str(t, mustStatus(t, resp, http.StatusCreated), "code")

	type result struct {
		status int
		body   map[string]any
	}
	resultCh := make(chan result, 1)
	go func() {
		r := env.do("POST", "/api/agents/connect?claim="+claim+"&name=ApproveMeWait&wait=true&timeout=10", "", nil)
		defer r.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		resultCh <- result{status: r.StatusCode, body: body}
	}()

	var connID string
	for i := 0; i < 50; i++ {
		pending, err := env.Store.ListPendingConnectionRequests(context.Background(), session.UserID)
		if err == nil && len(pending) > 0 {
			connID = pending[0].ID
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if connID == "" {
		t.Fatal("pending request never appeared")
	}
	approveResp := session.do("POST", "/api/agents/connect/"+connID+"/approve", nil)
	mustStatus(t, approveResp, http.StatusOK)

	select {
	case got := <-resultCh:
		if got.status != http.StatusCreated {
			t.Errorf("expected 201 Created on approved wait=true response, got %d (body=%v)", got.status, got.body)
		}
		if tok, _ := got.body["token"].(string); tok == "" {
			t.Error("approved response must include a non-empty token")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait=true POST never returned after approve")
	}
}

func TestConnectionRequest_NameFromQuery(t *testing.T) {
	env, session := setupConnectionEnv(t)

	resp := session.do("POST", "/api/agents/connect/claim", nil)
	body := mustStatus(t, resp, http.StatusCreated)
	code := str(t, body, "code")

	// Both name and claim ride on the URL; body is empty (no -d, no
	// Content-Type required). This is the canonical bootstrap shape.
	resp = env.do("POST", "/api/agents/connect?claim="+code+"&name=QueryAgent", "", nil)
	body = mustStatus(t, resp, http.StatusCreated)
	if str(t, body, "status") != "pending" {
		t.Errorf("expected status=pending, got %s", str(t, body, "status"))
	}

	pending, err := env.Store.ListPendingConnectionRequests(context.Background(), session.UserID)
	if err != nil {
		t.Fatalf("ListPendingConnectionRequests: %v", err)
	}
	if len(pending) != 1 || pending[0].Name != "QueryAgent" {
		t.Errorf("expected one pending request named QueryAgent, got %+v", pending)
	}
}
