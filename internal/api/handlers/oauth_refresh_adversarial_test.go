package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamlruntime"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

type oauthAdversarialFixture struct {
	v         vault.Vault
	user      string
	key       string
	reg       *adapters.Registry
	refreshes atomic.Int32
	reads     atomic.Int32
}

func newOAuthAdversarialFixture(t *testing.T, lifetime int, clientID string, duringRefresh func(vault.Vault, string, string)) *oauthAdversarialFixture {
	t.Helper()
	t.Setenv("CLAWVISOR_YAMLRUNTIME_ALLOW_PRIVATE_NETWORKS", "1")
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	u, err := sqlite.NewStore(db).CreateUser(ctx, "race@example.test", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	f := &oauthAdversarialFixture{v: v, user: u.ID, key: "x.account:alice", reg: adapters.NewRegistry()}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			_ = r.ParseForm()
			cid := r.Form.Get("client_id")
			if cid == "" {
				cid, _, _ = r.BasicAuth()
			}
			generation := f.refreshes.Load()
			if cid != clientID || r.Form.Get("refresh_token") != fmt.Sprintf("refresh-%d", generation) || !f.refreshes.CompareAndSwap(generation, generation+1) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			if duringRefresh != nil {
				duringRefresh(v, u.ID, f.key)
			}
			_, _ = fmt.Fprintf(w, `{"access_token":"access-%d","refresh_token":"refresh-%d","token_type":"Bearer","expires_in":%d}`, generation+1, generation+1, lifetime)
			return
		}
		f.reads.Add(1)
		if r.Header.Get("Authorization") != fmt.Sprintf("Bearer access-%d", f.refreshes.Load()) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"123"}}`))
	}))
	t.Cleanup(provider.Close)
	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "x.account"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer ", PKCEFlow: &yamldef.PKCEFlowDef{ClientID: "builtin-client", TokenURL: provider.URL + "/token"}},
		API:     yamldef.APIDef{BaseURL: provider.URL, Type: "rest"},
		Actions: map[string]yamldef.Action{"get_me": {Method: "GET", Path: "/me", Response: yamldef.ResponseDef{DataPath: "data"}}},
	}
	a, err := yamlruntime.New(def, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.reg.Register(a)
	initial, _ := json.Marshal(map[string]any{"type": "oauth2", "access_token": "access-0", "refresh_token": "refresh-0", "expiry": time.Now().Add(-time.Hour)})
	if err := v.Set(ctx, u.ID, f.key, initial); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *oauthAdversarialFixture) execute() error {
	_, err := executeAdapterRequest(context.Background(), f.v, f.reg, nil, f.user, f.key, "get_me", nil, f.key)
	return err
}

func TestOAuthRefreshDoesNotOverwriteConcurrentCredentialChange(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(fmt.Sprint("delete=", remove), func(t *testing.T) {
			const reauthorized = `{"access_token":"reauthorized","refresh_token":"reauthorized-refresh","expiry":"2030-01-01T00:00:00Z"}`
			f := newOAuthAdversarialFixture(t, 7200, "builtin-client", func(v vault.Vault, u, key string) {
				var err error
				if remove {
					err = v.Delete(context.Background(), u, key)
				} else {
					err = v.Set(context.Background(), u, key, []byte(reauthorized))
				}
				if err != nil {
					t.Error(err)
				}
			})
			if err := f.execute(); err == nil {
				t.Error("refresh should reject a concurrently changed credential")
			}
			raw, err := f.v.Get(context.Background(), f.user, f.key)
			if remove {
				if !errors.Is(err, vault.ErrNotFound) {
					t.Fatal("refresh recreated a deleted credential")
				}
			} else if err != nil || string(raw) != reauthorized {
				t.Fatal("refresh overwrote reauthorization")
			}
			if f.reads.Load() != 0 {
				t.Fatal("stale refresh result reached upstream API")
			}
		})
	}
}

func TestOAuthRefreshPersistsShortLivedRotations(t *testing.T) {
	f := newOAuthAdversarialFixture(t, 5, "builtin-client", nil)
	for range 2 {
		if err := f.execute(); err != nil {
			t.Fatalf("short-lived token request: %v", err)
		}
		raw, err := f.v.Get(context.Background(), f.user, f.key)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["refresh_token"] != fmt.Sprintf("refresh-%d", f.refreshes.Load()) {
			t.Fatal("adapter refreshed outside the persistence path")
		}
	}
}

func TestOAuthRefreshUsesConfiguredPKCEClientID(t *testing.T) {
	f := newOAuthAdversarialFixture(t, 7200, "user-client", nil)
	if err := adapters.SetPKCEClientID(context.Background(), f.v, "x.account", "user-client"); err != nil {
		t.Fatal(err)
	}
	if err := f.execute(); err != nil {
		t.Fatalf("vault client ID should override built-in client ID: %v", err)
	}
}

type expireBeforeExecutionAdapter struct {
	adapters.Adapter
	f *oauthAdversarialFixture
}

func (a expireBeforeExecutionAdapter) OAuthRefreshConfig(ctx context.Context, v vault.Vault) *oauth2.Config {
	return a.Adapter.(adapters.OAuthRefreshConfigProvider).OAuthRefreshConfig(ctx, v)
}

func (a expireBeforeExecutionAdapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	// Simulate expiry after the gateway's initial refresh but before the
	// adapter issues its HTTP request, without a sleep or real credentials.
	raw, err := a.f.v.Get(ctx, a.f.user, a.f.key)
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	fields["expiry"] = time.Now().Add(-time.Hour)
	raw, err = json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	if err := a.f.v.Set(ctx, a.f.user, a.f.key, raw); err != nil {
		return nil, err
	}
	req.Credential = raw
	return a.Adapter.Execute(ctx, req)
}

func TestOAuthRefreshPersistsAfterDelayedAdapterExecution(t *testing.T) {
	f := newOAuthAdversarialFixture(t, 7200, "builtin-client", nil)
	a, _ := f.reg.Get("x.account")
	f.reg.Register(expireBeforeExecutionAdapter{Adapter: a, f: f})
	if err := f.execute(); err != nil {
		t.Fatal(err)
	}
	raw, err := f.v.Get(context.Background(), f.user, f.key)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if f.refreshes.Load() != 2 || fields["refresh_token"] != "refresh-2" {
		t.Fatal("late adapter rotation was not persisted")
	}
}

func TestOAuthRefreshDoesNotFollowTokenEndpointRedirect(t *testing.T) {
	f := newOAuthAdversarialFixture(t, 7200, "builtin-client", nil)
	var leaked atomic.Bool
	recipient := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	t.Cleanup(recipient.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, recipient.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)
	f.reg.Register(resolverOAuthTestAdapter{serviceID: "x.account", tokenURL: redirect.URL})
	if err := f.execute(); err == nil {
		t.Fatal("redirected token exchange should fail")
	}
	if leaked.Load() {
		t.Fatal("refresh credential was forwarded to a redirect destination")
	}
}
