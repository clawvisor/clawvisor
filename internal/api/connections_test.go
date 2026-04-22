package api_test

import (
	"context"
	"encoding/json"
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

	// Create 10 pending requests (the max).
	for i := 0; i < 10; i++ {
		resp := env.do("POST", "/api/agents/connect", "", map[string]any{
			"name": "Agent",
		})
		mustStatus(t, resp, http.StatusCreated)
	}

	// 11th should be rejected.
	resp := env.do("POST", "/api/agents/connect", "", map[string]any{
		"name": "OneMore",
	})
	mustStatus(t, resp, http.StatusTooManyRequests)
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
