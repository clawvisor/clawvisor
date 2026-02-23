package api_test

import (
	"fmt"
	"net/http"
	"testing"
)

// ── Notifications ──────────────────────────────────────────────────────────────

func TestNotifications_Empty(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	resp := s.do("GET", "/api/notifications", nil)
	var configs []any
	decode(t, resp, &configs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list notifications: expected 200, got %d", resp.StatusCode)
	}
	if len(configs) != 0 {
		t.Errorf("expected empty list, got %d entries", len(configs))
	}
}

func TestNotifications_UpsertAndList_Telegram(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	// Save Telegram config
	resp := s.do("PUT", "/api/notifications/telegram", map[string]any{
		"bot_token": "1234:ABCDEF",
		"chat_id":   "99999",
	})
	body := mustStatus(t, resp, http.StatusOK)

	if body["channel"] != "telegram" {
		t.Errorf("upsert telegram: expected channel=telegram, got %v", body["channel"])
	}

	// List should now contain the telegram entry
	resp = s.do("GET", "/api/notifications", nil)
	var configs []any
	decode(t, resp, &configs)
	if len(configs) != 1 {
		t.Fatalf("after upsert: expected 1 config, got %d", len(configs))
	}
	cfg := configs[0].(map[string]any)
	if cfg["channel"] != "telegram" {
		t.Errorf("list: expected channel=telegram, got %v", cfg["channel"])
	}
}

func TestNotifications_Upsert_MissingFields(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	resp := s.do("PUT", "/api/notifications/telegram", map[string]any{
		"bot_token": "1234:ABCDEF",
		// missing chat_id
	})
	mustStatus(t, resp, http.StatusBadRequest)
}

func TestNotifications_Delete_Telegram(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	// Create first
	s.do("PUT", "/api/notifications/telegram", map[string]any{
		"bot_token": "tok", "chat_id": "123",
	})

	// Delete
	resp := s.do("DELETE", "/api/notifications/telegram", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete telegram: expected 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestNotifications_IsolatedByUser(t *testing.T) {
	env := newTestEnv(t)
	s1 := newSession(t, env)
	s2 := newSession(t, env)

	s1.do("PUT", "/api/notifications/telegram", map[string]any{
		"bot_token": "tok1", "chat_id": "1",
	})

	// s2 should see empty list
	resp := s2.do("GET", "/api/notifications", nil)
	var configs []any
	decode(t, resp, &configs)
	if len(configs) != 0 {
		t.Errorf("isolation: user2 should see 0 configs, got %d", len(configs))
	}
}

// ── Agents: UpdateRole ────────────────────────────────────────────────────────

func TestAgents_UpdateRole(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	// Create a role
	resp := s.do("POST", "/api/roles", map[string]any{"name": "engineer"})
	roleBody := mustStatus(t, resp, http.StatusCreated)
	roleID := str(t, roleBody, "id")

	// Create an agent (no role)
	resp = s.do("POST", "/api/agents", map[string]any{"name": "my-agent"})
	agentBody := mustStatus(t, resp, http.StatusCreated)
	agentID := str(t, agentBody, "id")
	if agentBody["role_id"] != nil {
		t.Errorf("new agent should have nil role_id")
	}

	// Assign role
	resp = s.do("PATCH", fmt.Sprintf("/api/agents/%s", agentID), map[string]any{
		"role_id": roleID,
	})
	updated := mustStatus(t, resp, http.StatusOK)
	if str(t, updated, "role_id") != roleID {
		t.Errorf("after update: role_id mismatch, got %v", updated["role_id"])
	}

	// Remove role
	resp = s.do("PATCH", fmt.Sprintf("/api/agents/%s", agentID), map[string]any{
		"role_id": nil,
	})
	cleared := mustStatus(t, resp, http.StatusOK)
	if cleared["role_id"] != nil {
		t.Errorf("after clear: role_id should be nil, got %v", cleared["role_id"])
	}
}

func TestAgents_UpdateRole_NotFound(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	resp := s.do("PATCH", "/api/agents/nonexistent", map[string]any{"role_id": nil})
	mustStatus(t, resp, http.StatusNotFound)
}

// ── User: UpdateMe (password change) ─────────────────────────────────────────

func TestUser_UpdateMe_ChangePassword(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	// Change password
	resp := s.do("PUT", "/api/me", map[string]any{
		"current_password": "TestPass123!",
		"new_password":     "NewPass456!",
	})
	mustStatus(t, resp, http.StatusOK)

	// Old password should no longer work
	resp2 := env.do("POST", "/api/auth/login", "", map[string]any{
		"email": s.Email, "password": "TestPass123!",
	})
	mustStatus(t, resp2, http.StatusUnauthorized)

	// New password should work
	resp3 := env.do("POST", "/api/auth/login", "", map[string]any{
		"email": s.Email, "password": "NewPass456!",
	})
	mustStatus(t, resp3, http.StatusOK)
}

func TestUser_UpdateMe_WrongCurrentPassword(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	resp := s.do("PUT", "/api/me", map[string]any{
		"current_password": "WrongPassword!",
		"new_password":     "NewPass456!",
	})
	mustStatus(t, resp, http.StatusUnauthorized)
}

func TestUser_UpdateMe_MissingFields(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	resp := s.do("PUT", "/api/me", map[string]any{
		"current_password": "TestPass123!",
		// missing new_password
	})
	mustStatus(t, resp, http.StatusBadRequest)
}

// ── User: DeleteMe ────────────────────────────────────────────────────────────

func TestUser_DeleteMe(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	// Confirm user exists
	resp := s.do("GET", "/api/me", nil)
	mustStatus(t, resp, http.StatusOK)

	// Delete account
	resp = s.do("DELETE", "/api/me", map[string]any{"password": "TestPass123!"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete me: expected 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Login should fail after deletion
	resp2 := env.do("POST", "/api/auth/login", "", map[string]any{
		"email": s.Email, "password": "TestPass123!",
	})
	mustStatus(t, resp2, http.StatusUnauthorized)
}

func TestUser_DeleteMe_WrongPassword(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	resp := s.do("DELETE", "/api/me", map[string]any{"password": "WrongPassword!"})
	mustStatus(t, resp, http.StatusUnauthorized)
}

func TestUser_DeleteMe_MissingPassword(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)

	resp := s.do("DELETE", "/api/me", map[string]any{})
	mustStatus(t, resp, http.StatusBadRequest)
}
