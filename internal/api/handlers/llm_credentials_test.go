package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// memVault implements vault.Vault in memory for handler-level tests.
type memVault struct {
	stored map[string][]byte
}

func newMemVault() *memVault { return &memVault{stored: map[string][]byte{}} }

func (v *memVault) Set(ctx context.Context, userID, serviceID string, credential []byte) error {
	v.stored[userID+"/"+serviceID] = append([]byte{}, credential...)
	return nil
}
func (v *memVault) Get(ctx context.Context, userID, serviceID string) ([]byte, error) {
	b, ok := v.stored[userID+"/"+serviceID]
	if !ok {
		return nil, vault.ErrNotFound
	}
	return append([]byte{}, b...), nil
}
func (v *memVault) Delete(ctx context.Context, userID, serviceID string) error {
	delete(v.stored, userID+"/"+serviceID)
	return nil
}
func (v *memVault) List(ctx context.Context, userID string) ([]string, error) { return nil, nil }

func newCredsTestEnv(t *testing.T) (*LLMCredentialsHandler, *memVault, *store.User, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "creds.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	user, err := st.CreateUser(ctx, "creds@example.com", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "creds-agent", "tok-hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	v := newMemVault()
	h := NewLLMCredentialsHandler(st, v, nil)
	return h, v, user, agent
}

func reqWithUser(r *http.Request, user *store.User) *http.Request {
	return r.WithContext(withUser(r.Context(), user))
}

func TestLLMCredentials_SetUserScoped(t *testing.T) {
	h, v, user, _ := newCredsTestEnv(t)
	body := strings.NewReader(`{"api_key":"sk-ant-real-key"}`)
	r := httptest.NewRequest(http.MethodPut, "/api/runtime/llm-credentials/anthropic", body)
	r.SetPathValue("provider", "anthropic")
	r = reqWithUser(r, user)

	w := httptest.NewRecorder()
	h.Set(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got, _ := v.Get(context.Background(), user.ID, "anthropic"); string(got) != "sk-ant-real-key" {
		t.Errorf("user-scoped vault got %q", got)
	}
}

func TestLLMCredentials_SetAgentScoped(t *testing.T) {
	h, v, user, agent := newCredsTestEnv(t)
	body := strings.NewReader(`{"api_key":"sk-ant-agent-key"}`)
	r := httptest.NewRequest(http.MethodPut, "/api/runtime/llm-credentials/anthropic?agent_id="+agent.ID, body)
	r.SetPathValue("provider", "anthropic")
	r = reqWithUser(r, user)

	w := httptest.NewRecorder()
	h.Set(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	wantSID := "agent:" + agent.ID + ":anthropic"
	if got, _ := v.Get(context.Background(), user.ID, wantSID); string(got) != "sk-ant-agent-key" {
		t.Errorf("agent-scoped vault got %q", got)
	}
	// User-scoped should NOT be touched.
	if _, err := v.Get(context.Background(), user.ID, "anthropic"); err == nil {
		t.Errorf("user-scoped key should not be set when agent_id is provided")
	}

	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["agent_id"] != agent.ID {
		t.Errorf("response missing agent_id: %v", resp)
	}
	if resp["service_id"] != wantSID {
		t.Errorf("response service_id=%q, want %q", resp["service_id"], wantSID)
	}
}

func TestLLMCredentials_AgentOwnershipEnforced(t *testing.T) {
	h, _, user, _ := newCredsTestEnv(t)
	// Use a random agent_id that does NOT belong to user.
	body := strings.NewReader(`{"api_key":"sk-ant-x"}`)
	r := httptest.NewRequest(http.MethodPut, "/api/runtime/llm-credentials/anthropic?agent_id=stranger-agent", body)
	r.SetPathValue("provider", "anthropic")
	r = reqWithUser(r, user)

	w := httptest.NewRecorder()
	h.Set(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for foreign agent_id, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestLLMCredentials_DeleteAgentScoped(t *testing.T) {
	h, v, user, agent := newCredsTestEnv(t)
	sid := "agent:" + agent.ID + ":openai"
	_ = v.Set(context.Background(), user.ID, sid, []byte("sk-existing"))

	r := httptest.NewRequest(http.MethodDelete, "/api/runtime/llm-credentials/openai?agent_id="+agent.ID, nil)
	r.SetPathValue("provider", "openai")
	r = reqWithUser(r, user)

	w := httptest.NewRecorder()
	h.Delete(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := v.Get(context.Background(), user.ID, sid); err == nil {
		t.Errorf("agent-scoped key should be deleted")
	}
}

func TestLLMCredentials_ListIncludesAgentStatus(t *testing.T) {
	h, v, user, agent := newCredsTestEnv(t)
	_ = v.Set(context.Background(), user.ID, "anthropic", []byte("sk-ant-user"))
	_ = v.Set(context.Background(), user.ID, "agent:"+agent.ID+":openai", []byte("sk-openai-agent"))

	r := httptest.NewRequest(http.MethodGet, "/api/runtime/llm-credentials?agent_id="+agent.ID, nil)
	r = reqWithUser(r, user)
	w := httptest.NewRecorder()
	h.List(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Credentials []struct {
			Provider    string `json:"provider"`
			Stored      bool   `json:"stored"`
			AgentStored bool   `json:"agent_stored"`
			AgentID     string `json:"agent_id"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	gotByProvider := map[string]struct {
		stored, agentStored bool
		agentID             string
	}{}
	for _, e := range resp.Credentials {
		gotByProvider[e.Provider] = struct {
			stored, agentStored bool
			agentID             string
		}{e.Stored, e.AgentStored, e.AgentID}
	}
	if !gotByProvider["anthropic"].stored {
		t.Errorf("anthropic user-scoped should be stored")
	}
	if gotByProvider["anthropic"].agentStored {
		t.Errorf("anthropic agent-scoped should NOT be stored")
	}
	if gotByProvider["openai"].stored {
		t.Errorf("openai user-scoped should NOT be stored")
	}
	if !gotByProvider["openai"].agentStored {
		t.Errorf("openai agent-scoped should be stored")
	}
	if gotByProvider["openai"].agentID != agent.ID {
		t.Errorf("openai agent_id=%q, want %q", gotByProvider["openai"].agentID, agent.ID)
	}
}
