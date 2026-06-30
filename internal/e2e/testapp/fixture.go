package testapp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"
)

// LocalUser holds tokens for a magic-link-authenticated user. The
// local magic-link flow is single-user on clawvisor — repeated calls
// to LoginAsLocalUser return the same identity.
type LocalUser struct {
	AccessToken  string
	RefreshToken string
	UserID       string
	Email        string
}

// LoginAsLocalUser drives the magic-link flow exposed for local
// installs (POST /api/auth/magic/local → POST /api/auth/magic). Returns
// the resulting tokens. Tests that need a fresh user shouldn't reuse
// this — clawvisor's local-only auth always mints the same identity.
func (s *Server) LoginAsLocalUser(t *testing.T) *LocalUser {
	t.Helper()
	resp, err := s.Client.Post(s.URL+"/api/auth/magic/local", "application/json", nil)
	if err != nil {
		t.Fatalf("magic/local: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("magic/local: status=%d", resp.StatusCode)
	}
	var magic struct {
		Token string `json:"token"`
	}
	if err := jsonDecode(resp.Body, &magic); err != nil {
		t.Fatalf("decode magic: %v", err)
	}
	body := []byte(`{"token":"` + magic.Token + `"}`)
	resp2, err := s.Client.Post(s.URL+"/api/auth/magic", "application/json", bytesReader(body))
	if err != nil {
		t.Fatalf("magic exchange: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("magic exchange: status=%d", resp2.StatusCode)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		User         struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := jsonDecode(resp2.Body, &out); err != nil {
		t.Fatalf("decode auth: %v", err)
	}
	return &LocalUser{
		AccessToken:  out.AccessToken,
		RefreshToken: out.RefreshToken,
		UserID:       out.User.ID,
		Email:        out.User.Email,
	}
}

// Agent is an agent record + its raw bearer token.
type Agent struct {
	ID    string
	Token string
	Name  string
}

// CreateAgent registers a new agent under the given user and returns
// the agent + its bearer token. Most scenarios call this once per test
// — there's no policy reason to share agents across tests.
func (s *Server) CreateAgent(t *testing.T, user *LocalUser, name string) *Agent {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": name})
	req, _ := http.NewRequest("POST", s.URL+"/api/agents", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+user.AccessToken)
	resp, err := s.Client.Do(req)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("create agent: status=%d body=%s", resp.StatusCode, raw)
	}
	var out struct {
		ID    string `json:"id"`
		Token string `json:"token"`
		Name  string `json:"name"`
	}
	if err := jsonDecode(resp.Body, &out); err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	return &Agent{ID: out.ID, Token: out.Token, Name: out.Name}
}

// SetLLMCredential stores an upstream LLM API key in the user's vault
// via the dedicated /api/runtime/llm-credentials endpoint. The generic
// /api/vault/items endpoint rejects reserved provider IDs ("anthropic",
// "openai", "google") so this is the only path that works for
// provider keys.
//
// agentID="" sets the user-scoped key; non-empty sets agent-scoped
// (preferred over user-scoped by the forwarder's lookup).
func (s *Server) SetLLMCredential(t *testing.T, user *LocalUser, provider, agentID, apiKey string) {
	t.Helper()
	path := "/api/runtime/llm-credentials/" + provider
	if agentID != "" {
		path += "?agent_id=" + url.QueryEscape(agentID)
	}
	body, _ := json.Marshal(map[string]any{"api_key": apiKey})
	req, _ := http.NewRequest("PUT", s.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+user.AccessToken)
	resp, err := s.Client.Do(req)
	if err != nil {
		t.Fatalf("set llm credential: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("set llm credential: status=%d body=%s", resp.StatusCode, raw)
	}
}
