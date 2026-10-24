package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"net/http"
	"sync"
	"time"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/vault"
	"golang.org/x/oauth2"
)

var (
	errOAuthCredentialInvalid = errors.New("stored OAuth credential could not be parsed")
	errOAuthRefreshFailed     = errors.New("could not refresh OAuth credential")
	errOAuthPersistFailed     = errors.New("could not persist refreshed OAuth credential")
	errOAuthCredentialChanged = errors.New("OAuth credential changed during refresh; retry the request")
)

// Fixed stripes bound memory use while serializing credential reads, refreshes,
// and writes in a daemon. A provider may invalidate the previous refresh token
// as soon as it is redeemed, so reading the vault must also happen under lock.
var oauthCredentialLocks [64]sync.Mutex

func oauthCredentialLock(userID, vaultKey string) *sync.Mutex {
	h := fnv.New64a()
	_, _ = h.Write([]byte(userID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(vaultKey))
	return &oauthCredentialLocks[h.Sum64()%uint64(len(oauthCredentialLocks))]
}

func oauthConfigForRefresh(ctx context.Context, v vault.Vault, a adapters.Adapter) *oauth2.Config {
	cfg := a.OAuthConfig()
	if p, ok := a.(adapters.OAuthRefreshConfigProvider); ok {
		cfg = p.OAuthRefreshConfig(ctx, v)
	}
	if cfg == nil {
		return nil
	}
	copy := *cfg
	if copy.ClientID == "" {
		copy.ClientID = adapters.GetPKCEClientID(ctx, v, a.ServiceID())
	}
	return &copy
}

// loadRefreshedCredential atomically persists rotated tokens on supporting
// vaults before returning them. Other backends retain per-request refresh without
// an unsafe unconditional write. Static API-key credentials remain unchanged.
func loadRefreshedCredential(ctx context.Context, v vault.Vault, a adapters.Adapter, userID, vaultKey string) ([]byte, error) {
	cfg := oauthConfigForRefresh(ctx, v, a)
	if cfg == nil {
		return v.Get(ctx, userID, vaultKey)
	}
	lock := oauthCredentialLock(userID, vaultKey)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := v.Get(ctx, userID, vaultKey)
	if err != nil {
		return nil, err
	}
	var stored struct {
		Token        string    `json:"token"`
		AccessToken  string    `json:"access_token"`
		RefreshToken string    `json:"refresh_token"`
		Expiry       time.Time `json:"expiry"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, errOAuthCredentialInvalid
	}
	if stored.Token != "" && stored.AccessToken == "" && stored.RefreshToken == "" {
		return raw, nil
	}
	if stored.AccessToken == "" && stored.RefreshToken == "" {
		return nil, errOAuthCredentialInvalid
	}
	token := &oauth2.Token{AccessToken: stored.AccessToken, RefreshToken: stored.RefreshToken, TokenType: "Bearer", Expiry: stored.Expiry}
	// Renew slightly early so the adapter's own token source cannot redeem an
	// almost-expired token outside this persistence lock.
	if token.Valid() && (token.Expiry.IsZero() || time.Until(token.Expiry) > time.Minute || stored.RefreshToken == "") {
		return raw, nil
	}
	if stored.RefreshToken == "" || cfg.ClientID == "" || validateTokenEndpoint(cfg.Endpoint.TokenURL) != nil {
		return nil, errOAuthRefreshFailed
	}
	token.Expiry = time.Now().Add(-time.Minute)
	client := *ssrfSafeOAuthClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	refreshCtx := context.WithValue(ctx, oauth2.HTTPClient, &client)
	token, err = cfg.TokenSource(refreshCtx, token).Token()
	if err != nil || token.AccessToken == "" {
		// Provider errors can contain response bodies and tokens. Never propagate
		// them through gateway results or audit logs.
		return nil, errOAuthRefreshFailed
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, errOAuthCredentialInvalid
	}
	fields["access_token"], _ = json.Marshal(token.AccessToken)
	fields["refresh_token"], _ = json.Marshal(token.RefreshToken)
	fields["expiry"], _ = json.Marshal(token.Expiry)
	updated, err := json.Marshal(fields)
	if err != nil {
		return nil, errOAuthCredentialInvalid
	}
	// Once the provider has rotated a token, persist it even if the original
	// caller disconnected. Bound this detached write so storage cannot hang.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if updater, ok := v.(vault.ConditionalUpdater); ok {
		swapped, err := updater.CompareAndSwap(persistCtx, userID, vaultKey, raw, updated)
		if err != nil {
			return nil, errOAuthPersistFailed
		}
		if !swapped {
			return nil, errOAuthCredentialChanged
		}
	}
	// Backends without atomic replacement keep their existing per-request
	// refresh behavior. Never introduce an unconditional credential write there.
	return updated, nil
}

type vaultOAuthTokenSource struct {
	ctx      context.Context
	v        vault.Vault
	a        adapters.Adapter
	userID   string
	vaultKey string
}

func (s *vaultOAuthTokenSource) Token() (*oauth2.Token, error) {
	raw, err := loadRefreshedCredential(s.ctx, s.v, s.a, s.userID, s.vaultKey)
	if err != nil {
		return nil, err
	}
	var stored oauthCredentialForResolver
	if json.Unmarshal(raw, &stored) != nil || stored.AccessToken == "" {
		return nil, errOAuthCredentialInvalid
	}
	return &oauth2.Token{AccessToken: stored.AccessToken, RefreshToken: stored.RefreshToken, Expiry: stored.Expiry, TokenType: "Bearer"}, nil
}
