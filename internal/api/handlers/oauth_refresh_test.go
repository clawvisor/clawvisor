package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/pkg/vault"
)

func TestGatewayPersistsRotatingPKCECredentials(t *testing.T) {
	f := newOAuthAdversarialFixture(t, 7200, "builtin-client", nil)
	ctx := context.Background()
	initial, _ := json.Marshal(map[string]any{"type": "oauth2", "access_token": "access-0", "refresh_token": "refresh-0", "expiry": time.Now().Add(-time.Hour), "scopes": []string{"offline.access", "users.read"}, "custom_metadata": "preserve-me"})
	if err := f.v.Set(ctx, f.user, f.key, initial); err != nil {
		t.Fatal(err)
	}
	for request := 0; request < 2; request++ {
		// Reopen both vault and adapters so these requests cannot rely on caches.
		f.reopen(t)
		err := f.execute()
		if err != nil {
			t.Fatalf("request %d failed: %v", request+1, err)
		}
	}
	raw, err := f.v.Get(ctx, f.user, f.key)
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
	if f.refreshes.Load() != 1 {
		t.Fatalf("refresh count = %d, want 1", f.refreshes.Load())
	}

	expire := func() {
		t.Helper()
		raw, err := f.v.Get(ctx, f.user, f.key)
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
		if err := f.v.Set(ctx, f.user, f.key, raw); err != nil {
			t.Fatal(err)
		}
	}
	expire()
	if err := f.execute(); err != nil {
		t.Fatalf("second expiry cycle: %v", err)
	}
	if f.refreshes.Load() != 2 {
		t.Fatal("second cycle did not use the rotated refresh token")
	}

	expire()
	f.reg = f.newRegistry(t)
	var wg sync.WaitGroup
	failures := make(chan error, 12)
	for range 12 {
		wg.Go(func() {
			err := f.execute()
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
	if f.refreshes.Load() != 3 {
		t.Fatal("concurrent callers redeemed the same token more than once")
	}
}

func TestOAuthRefreshPersistenceFailureBlocksAPI(t *testing.T) {
	f := newOAuthAdversarialFixture(t, 7200, "builtin-client", nil)
	f.v = failOAuthWriteVault{f.v}
	err := f.execute()
	if err == nil || strings.Contains(err.Error(), "secret-storage-detail") {
		t.Fatalf("unexpected persistence failure: %v", err)
	}
	if f.refreshes.Load() != 1 || f.reads.Load() != 0 {
		t.Fatal("API called before refreshed tokens were persisted, or renewal was not attempted")
	}
}

func TestOAuthRefreshRejectsInvalidCredentials(t *testing.T) {
	f := newOAuthAdversarialFixture(t, 7200, "builtin-client", nil)
	ctx := context.Background()

	for _, credential := range []string{
		`{"access_token":"expired","refresh_token":"","expiry":"2000-01-01T00:00:00Z"}`,
		`{"access_token":"expired","refresh_token":"secret-refresh","expiry":"malformed"}`,
	} {
		if err := f.v.Set(ctx, f.user, f.key, []byte(credential)); err != nil {
			t.Fatal(err)
		}
		err := f.execute()
		if err == nil || strings.Contains(err.Error(), "secret-refresh") {
			t.Fatalf("invalid credential was not rejected safely: %v", err)
		}
		if f.reads.Load() != 0 || f.refreshes.Load() != 0 {
			t.Fatal("invalid credential reached API")
		}
	}
}

func TestOAuthStaticKeyDoesNotRefresh(t *testing.T) {
	f := newOAuthAdversarialFixture(t, 7200, "builtin-client", nil)
	if err := f.v.Set(context.Background(), f.user, f.key, []byte(`{"type":"api_key","token":"access-0"}`)); err != nil {
		t.Fatal(err)
	}
	if err := f.execute(); err != nil {
		t.Fatal(err)
	}
	if f.refreshes.Load() != 0 || f.reads.Load() != 1 {
		t.Fatal("static API key did not reach API without OAuth renewal")
	}
}

type failOAuthWriteVault struct{ vault.Vault }

func (failOAuthWriteVault) Set(context.Context, string, string, []byte) error {
	return errors.New("secret-storage-detail")
}

func (failOAuthWriteVault) CompareAndSwap(context.Context, string, string, []byte, []byte) (bool, error) {
	return false, errors.New("secret-storage-detail")
}
