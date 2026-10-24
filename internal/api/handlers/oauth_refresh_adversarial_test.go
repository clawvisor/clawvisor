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

	"golang.org/x/oauth2"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamlruntime"
	"github.com/clawvisor/clawvisor/pkg/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

type oauthAdversarialFixture struct {
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
	v, err := vault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	f := &oauthAdversarialFixture{store: sqlite.NewStore(db), v: v, user: u.ID, key: "x.account:alice", reg: adapters.NewRegistry()}
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
	for i := range 2 {
		if err := f.execute(); err != nil {
			t.Fatalf("short-lived token request: %v", err)
		}
		if f.refreshes.Load() != int32(i+1) {
			t.Fatal("one execution redeemed more than one refresh token")
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

func TestOAuthRefreshPersistsThroughProductionVaultWrappers(t *testing.T) {
	f := newOAuthAdversarialFixture(t, 7200, "builtin-client", nil)
	local := f.v.(*vault.LocalVault)
	f.v = vault.NewInstanceAware(vault.NewReferenceVault(local, local, nil, nil))
	for range 2 {
		if err := f.execute(); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := local.Get(context.Background(), f.user, f.key)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["refresh_token"] != "refresh-1" {
		t.Fatal("production vault wrappers hid conditional persistence")
	}
}

type expireBeforeExecutionAdapter struct {
	adapters.Adapter
	f *oauthAdversarialFixture
}

func (a expireBeforeExecutionAdapter) OAuthRefreshConfig(ctx context.Context, v vault.Vault) (*oauth2.Config, error) {
	return a.Adapter.(adapters.OAuthRefreshConfigProvider).OAuthRefreshConfig(ctx, v)
}

func (a expireBeforeExecutionAdapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	// Let the actual preflight token expire before its first HTTP use.
	time.Sleep(1100 * time.Millisecond)
	return a.Adapter.Execute(ctx, req)
}

func TestOAuthRefreshPersistsAfterDelayedAdapterExecution(t *testing.T) {
	f := newOAuthAdversarialFixture(t, 1, "builtin-client", nil)
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

// Cancel only after the provider has redeemed the old token. The next request
// must succeed using the saved rotation despite the original caller leaving.
func TestOAuthRefreshSurvivesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newOAuthAdversarialFixture(t, 7200, "builtin-client", func(vault.Vault, string, string) { cancel() })
	_, _ = executeAdapterRequest(ctx, f.v, f.reg, nil, f.user, f.key, "get_me", nil, f.key)
	if err := f.execute(); err != nil {
		t.Fatalf("next request lost rotated credential: %v", err)
	}
	if f.refreshes.Load() != 1 || f.reads.Load() != 1 {
		t.Fatal("next request did not reuse the saved rotation")
	}
}

func TestOAuthRefreshUsesSharedCredentialWithoutCreatingPersonalCopy(t *testing.T) {
	f := newOAuthAdversarialFixture(t, 7200, "builtin-client", nil)
	ctx := context.Background()
	local := f.v.(*vault.LocalVault)
	raw, err := local.Get(ctx, f.user, f.key)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Set(ctx, vault.InstanceUserID, f.key, raw); err != nil {
		t.Fatal(err)
	}
	if err := local.Delete(ctx, f.user, f.key); err != nil {
		t.Fatal(err)
	}
	f.v = vault.NewInstanceAware(vault.NewReferenceVault(local, local, nil, nil))
	bob, err := f.store.CreateUser(ctx, "bob@example.test", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for i := range 8 {
		user := f.user
		if i%2 == 1 {
			user = bob.ID
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := executeAdapterRequest(ctx, f.v, f.reg, nil, user, f.key, "get_me", nil, f.key)
			failures <- err
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := local.Get(ctx, bob.ID, f.key); !errors.Is(err, vault.ErrNotFound) {
		t.Fatal("renewal created a second user's personal credential")
	}
	if f.refreshes.Load() != 1 {
		t.Fatal("shared rotation was not reused")
	}
	if _, err := local.Get(ctx, f.user, f.key); !errors.Is(err, vault.ErrNotFound) {
		t.Fatal("renewal created a personal credential")
	}
}

type failPKCEConfigVault struct{ vault.Vault }

func (v failPKCEConfigVault) Get(ctx context.Context, user, key string) ([]byte, error) {
	if user == adapters.SystemUserID {
		return nil, errors.New("sensitive-backend-detail")
	}
	return v.Vault.Get(ctx, user, key)
}

func TestOAuthRefreshConfigReadFailureFailsClosed(t *testing.T) {
	f := newOAuthAdversarialFixture(t, 7200, "builtin-client", nil)
	f.v = failPKCEConfigVault{f.v}
	err := f.execute()
	if err == nil || err.Error() != errOAuthConfiguration.Error() {
		t.Fatalf("expected safe config failure: %v", err)
	}
	if f.refreshes.Load() != 0 || f.reads.Load() != 0 {
		t.Fatal("configuration failure contacted provider")
	}
}

func TestOAuthStaticKeyIgnoresUnavailablePKCEConfig(t *testing.T) {
	f := newOAuthAdversarialFixture(t, 7200, "builtin-client", nil)
	// The provider accepts access-0 before any renewal; use it as a static key.
	if err := f.v.Set(context.Background(), f.user, f.key, []byte(`{"token":"access-0"}`)); err != nil {
		t.Fatal(err)
	}
	f.v = failPKCEConfigVault{f.v}
	if err := f.execute(); err != nil {
		t.Fatal(err)
	}
	if f.refreshes.Load() != 0 || f.reads.Load() != 1 {
		t.Fatal("static key did not reach API without OAuth")
	}
}

func TestOAuthRefreshRetainsOmittedRefreshTokenAndClientSecret(t *testing.T) {
	f := newOAuthAdversarialFixture(t, 7200, "builtin-client", nil)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/me" {
			if r.Header.Get("Authorization") != fmt.Sprintf("Bearer new-%d", calls.Load()) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"id":"123"}}`))
			return
		}
		_ = r.ParseForm()
		id, secret, _ := r.BasicAuth()
		if id != "confidential-client" || secret != "client-secret" || r.Form.Get("refresh_token") != "refresh-0" {
			t.Error("confidential refresh omitted client authentication or retained refresh token")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		n := calls.Add(1)
		_, _ = fmt.Fprintf(w, `{"access_token":"new-%d","token_type":"Bearer","expires_in":3600}`, n)
	}))
	t.Cleanup(provider.Close)
	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "x.account"},
		Auth:    yamldef.AuthDef{Type: "oauth2", OAuth: &yamldef.OAuthDef{ClientID: "confidential-client", ClientSecret: "client-secret", TokenURL: provider.URL + "/token"}},
		API:     yamldef.APIDef{BaseURL: provider.URL, Type: "rest"},
		Actions: map[string]yamldef.Action{"get_me": {Method: "GET", Path: "/me", Response: yamldef.ResponseDef{DataPath: "data"}}},
	}
	a, err := yamlruntime.New(def, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.reg.Register(a)
	for i := range 2 {
		if err := f.execute(); err != nil {
			t.Fatal(err)
		}
		raw, err := f.v.Get(context.Background(), f.user, f.key)
		if err != nil {
			t.Fatal(err)
		}
		var saved oauthCredentialForResolver
		if err := json.Unmarshal(raw, &saved); err != nil {
			t.Fatal(err)
		}
		if saved.AccessToken != fmt.Sprintf("new-%d", i+1) || saved.RefreshToken != "refresh-0" || !saved.Expiry.After(time.Now()) {
			t.Fatal("renewal failed to preserve reusable credential bundle")
		}
		saved.Expiry = time.Now().Add(-time.Hour)
		raw, _ = json.Marshal(saved)
		if err := f.v.Set(context.Background(), f.user, f.key, raw); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("expected two successful renewal cycles")
	}
}

func TestOAuthPKCEOnlyOAuthTypeCanRenew(t *testing.T) {
	f := newOAuthAdversarialFixture(t, 7200, "builtin-client", nil)
	original, _ := f.reg.Get("x.account")
	cfg, err := oauthConfigForRefresh(context.Background(), f.v, original)
	if err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSuffix(cfg.Endpoint.TokenURL, "/token")
	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "x.account"},
		Auth:    yamldef.AuthDef{Type: "oauth2", PKCEFlow: &yamldef.PKCEFlowDef{ClientID: "builtin-client", TokenURL: cfg.Endpoint.TokenURL}},
		API:     yamldef.APIDef{BaseURL: base, Type: "rest"},
		Actions: map[string]yamldef.Action{"get_me": {Method: "GET", Path: "/me", Response: yamldef.ResponseDef{DataPath: "data"}}},
	}
	a, err := yamlruntime.New(def, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.reg.Register(a)
	for range 2 {
		if err := f.execute(); err != nil {
			t.Fatal(err)
		}
	}
	if f.refreshes.Load() != 1 || f.reads.Load() != 2 {
		t.Fatal("PKCE-only OAuth adapter did not reuse renewed credential")
	}
}

// Hiding CAS simulates an external vault backend with legacy per-request
// refresh behavior. A refresh-only credential must remain usable in the proxy.
type nonAtomicOAuthVault struct{ vault.Vault }

type incompleteOAuthConfigAdapter struct {
	adapters.Adapter
	cfg *oauth2.Config
}

func (a incompleteOAuthConfigAdapter) OAuthRefreshConfig(context.Context, vault.Vault) (*oauth2.Config, error) {
	return a.cfg, nil
}

func TestOAuthIncompleteConfigNeverForwardsExpiredAccessToken(t *testing.T) {
	for _, missing := range []string{"client_id", "token_url"} {
		t.Run(missing, func(t *testing.T) {
			f := newOAuthAdversarialFixture(t, 7200, "builtin-client", nil)
			a, _ := f.reg.Get("x.account")
			cfg, err := oauthConfigForRefresh(context.Background(), f.v, a)
			if err != nil {
				t.Fatal(err)
			}
			if missing == "client_id" {
				cfg.ClientID = ""
			} else {
				cfg.Endpoint.TokenURL = ""
			}
			f.reg.Register(incompleteOAuthConfigAdapter{Adapter: a, cfg: cfg})
			if err := f.execute(); err == nil {
				t.Fatal("expired OAuth credential accepted without renewal configuration")
			}
			if f.refreshes.Load() != 0 || f.reads.Load() != 0 {
				t.Fatal("incomplete configuration contacted provider")
			}
		})
	}
}
