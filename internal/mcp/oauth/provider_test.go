package oauth

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/relay"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// mockStore embeds the store.Store interface; only the methods needed for
// relay_pairing are implemented. Calling anything else panics.
type mockStore struct {
	store.Store
	user  *store.User
	agent *store.Agent
}

func (m *mockStore) GetUserByEmail(_ context.Context, email string) (*store.User, error) {
	if m.user != nil && m.user.Email == email {
		return m.user, nil
	}
	return nil, store.ErrNotFound
}

func (m *mockStore) CreateAgent(_ context.Context, userID, name, tokenHash string) (*store.Agent, error) {
	m.agent = &store.Agent{ID: "agent-1", UserID: userID, Name: name, TokenHash: tokenHash}
	return m.agent, nil
}

func newTestProvider(verifier func(string) bool) *Provider {
	st := &mockStore{
		user: &store.User{ID: "user-1", Email: "admin@local"},
	}
	opts := []ProviderOption{WithDaemonID("test-daemon")}
	if verifier != nil {
		opts = append(opts, WithPairingVerifier(verifier))
	}
	return NewProvider(st, nil, "http://localhost", slog.Default(), opts...)
}

func postToken(p *Provider, viaRelay bool, params url.Values) *httptest.ResponseRecorder {
	body := params.Encode()
	req := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if viaRelay {
		req = req.WithContext(relay.WithViaRelay(req.Context()))
	}
	w := httptest.NewRecorder()
	p.Token(w, req)
	return w
}

func TestRelayPairingSuccess(t *testing.T) {
	verifier := func(code string) bool { return code == "123456" }
	p := newTestProvider(verifier)

	w := postToken(p, true, url.Values{
		"grant_type":   {"relay_pairing"},
		"pairing_code": {"123456"},
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "access_token") {
		t.Error("response should contain access_token")
	}
	if !strings.Contains(body, "test-daemon") {
		t.Error("response should contain daemon_id")
	}
}

func TestRelayPairingMissingCode(t *testing.T) {
	p := newTestProvider(func(string) bool { return true })

	w := postToken(p, true, url.Values{
		"grant_type": {"relay_pairing"},
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pairing_code is required") {
		t.Error("expected pairing_code required error")
	}
}

func TestRelayPairingWrongCode(t *testing.T) {
	verifier := func(code string) bool { return code == "123456" }
	p := newTestProvider(verifier)

	w := postToken(p, true, url.Values{
		"grant_type":   {"relay_pairing"},
		"pairing_code": {"000000"},
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid_grant") {
		t.Error("expected invalid_grant error")
	}
}

func TestRelayPairingNotViaRelay(t *testing.T) {
	p := newTestProvider(func(string) bool { return true })

	w := postToken(p, false, url.Values{
		"grant_type":   {"relay_pairing"},
		"pairing_code": {"123456"},
	})

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRelayPairingNoVerifierConfigured(t *testing.T) {
	p := newTestProvider(nil) // no verifier

	w := postToken(p, true, url.Values{
		"grant_type":   {"relay_pairing"},
		"pairing_code": {"123456"},
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid_grant") {
		t.Error("expected invalid_grant error when no verifier configured")
	}
}
