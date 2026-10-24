package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamlruntime"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

func TestGatewayPersistsRotatingPKCECredentials(t *testing.T) {
	t.Setenv("CLAWVISOR_YAMLRUNTIME_ALLOW_PRIVATE_NETWORKS", "1")
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	user, err := sqlite.NewStore(db).CreateUser(ctx, "oauth@example.test", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "vault.key")
	v, err := vault.NewLocalVault(keyPath, db, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	var refreshes atomic.Int32
	var reads atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			_ = r.ParseForm()
			generation := refreshes.Load()
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != fmt.Sprintf("refresh-%d", generation) || !refreshes.CompareAndSwap(generation, generation+1) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token already redeemed"}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"access_token":"access-%d","refresh_token":"refresh-%d","token_type":"Bearer","expires_in":7200}`, generation+1, generation+1)
			return
		}
		reads.Add(1)
		if r.Header.Get("Authorization") != fmt.Sprintf("Bearer access-%d", refreshes.Load()) && r.Header.Get("Authorization") != "Bearer static-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"123"}}`))
	}))
	t.Cleanup(provider.Close)
	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "x.account"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer ", PKCEFlow: &yamldef.PKCEFlowDef{ClientID: "native-client", TokenURL: provider.URL + "/token"}},
		API:     yamldef.APIDef{BaseURL: provider.URL, Type: "rest"},
		Actions: map[string]yamldef.Action{"get_me": {Method: "GET", Path: "/me", Response: yamldef.ResponseDef{DataPath: "data"}}},
	}
	newRegistry := func() *adapters.Registry {
		a, err := yamlruntime.New(def, nil)
		if err != nil {
			t.Fatal(err)
		}
		r := adapters.NewRegistry()
		r.Register(a)
		return r
	}
	initial, _ := json.Marshal(map[string]any{"type": "oauth2", "access_token": "access-0", "refresh_token": "refresh-0", "expiry": time.Now().Add(-time.Hour), "scopes": []string{"offline.access", "users.read"}, "custom_metadata": "preserve-me"})
	const key = "x.account:alice"
	if err := v.Set(ctx, user.ID, key, initial); err != nil {
		t.Fatal(err)
	}
	for request := 0; request < 2; request++ {
		// A new registry discards all adapter-local token caches, as a restart does.
		v, err = vault.NewLocalVault(keyPath, db, "sqlite")
		if err != nil {
			t.Fatal(err)
		}
		_, err := executeAdapterRequest(ctx, v, newRegistry(), nil, user.ID, key, "get_me", nil, key)
		if err != nil {
			t.Fatalf("request %d failed: %v", request+1, err)
		}
	}
	raw, err := v.Get(ctx, user.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["access_token"] != "access-1" || stored["refresh_token"] != "refresh-1" {
		t.Fatal("rotated credential was not persisted")
	}
	if stored["custom_metadata"] != "preserve-me" {
		t.Fatal("credential metadata was lost")
	}
	if fmt.Sprint(stored["scopes"]) != "[offline.access users.read]" {
		t.Fatal("credential scopes were lost")
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refresh count = %d, want 1", refreshes.Load())
	}

	expire := func() {
		t.Helper()
		raw, err := v.Get(ctx, user.ID, key)
		if err != nil {
			t.Fatal(err)
		}
		var stored map[string]any
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatal(err)
		}
		stored["expiry"] = time.Now().Add(-time.Hour)
		raw, err = json.Marshal(stored)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.Set(ctx, user.ID, key, raw); err != nil {
			t.Fatal(err)
		}
	}
	expire()
	if _, err := executeAdapterRequest(ctx, v, newRegistry(), nil, user.ID, key, "get_me", nil, key); err != nil {
		t.Fatalf("second expiry cycle: %v", err)
	}
	if refreshes.Load() != 2 {
		t.Fatal("second cycle did not use the rotated refresh token")
	}

	expire()
	reg := newRegistry()
	var wg sync.WaitGroup
	failures := make(chan error, 12)
	for range 12 {
		wg.Go(func() {
			_, err := executeAdapterRequest(ctx, v, reg, nil, user.ID, key, "get_me", nil, key)
			failures <- err
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatalf("concurrent request: %v", err)
		}
	}
	if refreshes.Load() != 3 {
		t.Fatal("concurrent callers redeemed the same token more than once")
	}

	// Do not contact the upstream API if persisting a rotation fails, and do
	// not expose a storage/provider error body to the agent.
	expire()
	before := reads.Load()
	_, err = executeAdapterRequest(ctx, failOAuthWriteVault{v}, reg, nil, user.ID, key, "get_me", nil, key)
	if err == nil || strings.Contains(err.Error(), "secret-storage-detail") {
		t.Fatalf("unexpected persistence failure: %v", err)
	}
	if reads.Load() != before {
		t.Fatal("API called before refreshed tokens were persisted")
	}

	for _, credential := range []string{
		`{"access_token":"expired","refresh_token":"","expiry":"2000-01-01T00:00:00Z"}`,
		`{"access_token":"expired","refresh_token":"secret-refresh","expiry":"malformed"}`,
	} {
		if err := v.Set(ctx, user.ID, key, []byte(credential)); err != nil {
			t.Fatal(err)
		}
		_, err := executeAdapterRequest(ctx, v, reg, nil, user.ID, key, "get_me", nil, key)
		if err == nil || strings.Contains(err.Error(), "secret-refresh") {
			t.Fatalf("invalid credential was not rejected safely: %v", err)
		}
		if reads.Load() != before {
			t.Fatal("invalid credential reached API")
		}
	}
	if err := v.Set(ctx, user.ID, key, []byte(`{"type":"api_key","token":"static-key"}`)); err != nil {
		t.Fatal(err)
	}
	beforeRefresh := refreshes.Load()
	if _, err := executeAdapterRequest(ctx, v, reg, nil, user.ID, key, "get_me", nil, key); err != nil {
		t.Fatal(err)
	}
	if refreshes.Load() != beforeRefresh {
		t.Fatal("static API key was refreshed")
	}
}

type failOAuthWriteVault struct{ vault.Vault }

func (failOAuthWriteVault) Set(context.Context, string, string, []byte) error {
	return errors.New("secret-storage-detail")
}

func (failOAuthWriteVault) CompareAndSwap(context.Context, string, string, []byte, []byte) (bool, error) {
	return false, errors.New("secret-storage-detail")
}
