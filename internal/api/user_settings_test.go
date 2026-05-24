package api_test

import (
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

// ── Agent runtime settings: ConversationAutoApproveThreshold ────────────────

// createTestAgent provisions an agent under the current session and
// returns its ID. Each conversation-auto-approve test needs an agent
// to scope the runtime-settings endpoint against.
func createTestAgent(t *testing.T, s *testSession) string {
	t.Helper()
	resp := s.do("POST", "/api/agents", map[string]any{"name": "test-agent"})
	body := mustStatus(t, resp, http.StatusCreated)
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("agent create returned no id; body=%v", body)
	}
	return id
}

// runtimeSettingsBody returns a minimal payload for PUT runtime-settings
// with the supplied conversation-auto-approve threshold and everything
// else at safe defaults. The handler requires all the non-optional
// fields, so we always send the full shape.
func runtimeSettingsBody(threshold *string) map[string]any {
	body := map[string]any{
		"runtime_enabled":          true,
		"runtime_mode":             "observe",
		"starter_profile":          "none",
		"outbound_credential_mode": "inherit",
		"inject_stored_bearer":     false,
	}
	if threshold != nil {
		body["conversation_auto_approve_threshold"] = *threshold
	}
	return body
}

func TestAgentRuntimeSettings_ConversationAutoApprove_DefaultIsOff(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)
	agentID := createTestAgent(t, s)

	resp := s.do("GET", "/api/agents/"+agentID+"/runtime-settings", nil)
	body := mustStatus(t, resp, http.StatusOK)
	if got, _ := body["conversation_auto_approve_threshold"].(string); got != "off" {
		t.Errorf("default threshold = %q, want %q", got, "off")
	}
}

func TestAgentRuntimeSettings_ConversationAutoApprove_SetLow(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)
	agentID := createTestAgent(t, s)
	low := "low"

	resp := s.do("PUT", "/api/agents/"+agentID+"/runtime-settings", runtimeSettingsBody(&low))
	body := mustStatus(t, resp, http.StatusOK)
	if got, _ := body["conversation_auto_approve_threshold"].(string); got != "low" {
		t.Errorf("threshold after PUT = %q, want %q", got, "low")
	}

	resp = s.do("GET", "/api/agents/"+agentID+"/runtime-settings", nil)
	body = mustStatus(t, resp, http.StatusOK)
	if got, _ := body["conversation_auto_approve_threshold"].(string); got != "low" {
		t.Errorf("threshold on subsequent GET = %q, want %q", got, "low")
	}
}

func TestAgentRuntimeSettings_ConversationAutoApprove_SetMediumAtCap(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)
	agentID := createTestAgent(t, s)
	medium := "medium"

	resp := s.do("PUT", "/api/agents/"+agentID+"/runtime-settings", runtimeSettingsBody(&medium))
	body := mustStatus(t, resp, http.StatusOK)
	if got, _ := body["conversation_auto_approve_threshold"].(string); got != "medium" {
		t.Errorf("threshold = %q, want %q", got, "medium")
	}
}

func TestAgentRuntimeSettings_ConversationAutoApprove_RejectsHigh(t *testing.T) {
	// The API must enforce the UI cap even when a direct HTTP client
	// tries to skip the dropdown. "high" and "critical" are above the
	// cap and must come back as 400 BAD REQUEST.
	env := newTestEnv(t)
	s := newSession(t, env)
	agentID := createTestAgent(t, s)

	for _, level := range []string{"high", "critical"} {
		t.Run(level, func(t *testing.T) {
			l := level
			resp := s.do("PUT", "/api/agents/"+agentID+"/runtime-settings", runtimeSettingsBody(&l))
			mustStatus(t, resp, http.StatusBadRequest)
		})
	}
}

func TestAgentRuntimeSettings_ConversationAutoApprove_RejectsGarbage(t *testing.T) {
	env := newTestEnv(t)
	s := newSession(t, env)
	agentID := createTestAgent(t, s)
	bad := "EXTREME"

	resp := s.do("PUT", "/api/agents/"+agentID+"/runtime-settings", runtimeSettingsBody(&bad))
	mustStatus(t, resp, http.StatusBadRequest)
}

func TestAgentRuntimeSettings_ConversationAutoApprove_OmittedFieldPreserved(t *testing.T) {
	// When the client doesn't include the threshold field in the PUT
	// payload, the stored value must not be reset to "off". This is
	// the optional-field semantics that protects existing
	// runtime-settings clients (which don't know about this field)
	// from accidentally clobbering it.
	env := newTestEnv(t)
	s := newSession(t, env)
	agentID := createTestAgent(t, s)

	// First set to medium.
	medium := "medium"
	resp := s.do("PUT", "/api/agents/"+agentID+"/runtime-settings", runtimeSettingsBody(&medium))
	mustStatus(t, resp, http.StatusOK)

	// Then PUT without the threshold field; existing value must persist.
	resp = s.do("PUT", "/api/agents/"+agentID+"/runtime-settings", runtimeSettingsBody(nil))
	body := mustStatus(t, resp, http.StatusOK)
	if got, _ := body["conversation_auto_approve_threshold"].(string); got != "medium" {
		t.Errorf("threshold after omitted-field PUT = %q, want %q (must not clobber)", got, "medium")
	}
}

func TestAgentRuntimeSettings_ConversationAutoApprove_EmptyCollapsesToOff(t *testing.T) {
	// Empty-string value collapses to "off" — that's how a client
	// "clears" the setting. Distinct from omitting the field, which
	// preserves the existing value.
	env := newTestEnv(t)
	s := newSession(t, env)
	agentID := createTestAgent(t, s)

	medium := "medium"
	s.do("PUT", "/api/agents/"+agentID+"/runtime-settings", runtimeSettingsBody(&medium))

	empty := ""
	resp := s.do("PUT", "/api/agents/"+agentID+"/runtime-settings", runtimeSettingsBody(&empty))
	body := mustStatus(t, resp, http.StatusOK)
	if got, _ := body["conversation_auto_approve_threshold"].(string); got != "off" {
		t.Errorf("threshold after empty-string PUT = %q, want %q", got, "off")
	}
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
