package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamlruntime"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

type oauthAdversarialFixture struct {
	keyPath   string
	def       yamldef.ServiceDef
	store     *sqlite.Store
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
	keyPath := filepath.Join(t.TempDir(), "vault.key")
	v, err := vault.NewLocalVault(keyPath, db, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	f := &oauthAdversarialFixture{keyPath: keyPath, store: sqlite.NewStore(db), v: v, user: u.ID, key: "x.account:alice"}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			_ = r.ParseForm()
			cid := r.Form.Get("client_id")
			if cid == "" {
				cid, _, _ = r.BasicAuth()
			}
			generation := f.refreshes.Load()
			if r.Form.Get("grant_type") != "refresh_token" || cid != clientID || r.Form.Get("refresh_token") != fmt.Sprintf("refresh-%d", generation) || !f.refreshes.CompareAndSwap(generation, generation+1) {
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
	f.def = yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "x.account"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer ", PKCEFlow: &yamldef.PKCEFlowDef{ClientID: "builtin-client", TokenURL: provider.URL + "/token"}},
		API:     yamldef.APIDef{BaseURL: provider.URL, Type: "rest"},
		Actions: map[string]yamldef.Action{"get_me": {Method: "GET", Path: "/me", Response: yamldef.ResponseDef{DataPath: "data"}}},
	}
	f.reg = f.newRegistry(t)
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

func (f *oauthAdversarialFixture) newRegistry(t *testing.T) *adapters.Registry {
	t.Helper()
	a, err := yamlruntime.New(f.def, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg := adapters.NewRegistry()
	reg.Register(a)
	return reg
}

func (f *oauthAdversarialFixture) reopen(t *testing.T) {
	t.Helper()
	v, err := vault.NewLocalVault(f.keyPath, f.store.DB(), "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	f.v = v
	f.reg = f.newRegistry(t)
}
