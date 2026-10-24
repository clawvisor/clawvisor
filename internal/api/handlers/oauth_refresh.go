package handlers

import (
	"bytes"
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
	errOAuthConfiguration     = errors.New("could not resolve OAuth refresh configuration")
	errOAuthCredentialLoad    = errors.New("could not load OAuth credential")
)

// Resolve once per adapter execution; retain the complete confidential-client
// configuration and use the same PKCE precedence as activation.
func oauthConfigForRefresh(ctx context.Context, v vault.Vault, a adapters.Adapter) (*oauth2.Config, error) {
	cfg := a.OAuthConfig()
	if p, ok := a.(adapters.OAuthRefreshConfigProvider); ok {
		var err error
		cfg, err = p.OAuthRefreshConfig(ctx, v)
		if err != nil {
			return nil, errOAuthConfiguration
		}
	}
	if cfg == nil {
		return nil, nil
	}
	copy := *cfg
	return &copy, nil
}

// A PKCE-capable adapter can also use a static key. An unrelated client-ID
// lookup failure must not disable that activation mode, but OAuth credentials
// must still fail closed rather than falling back to a stale access token.
func oauthConfigForCredential(ctx context.Context, v vault.Vault, a adapters.Adapter, userID, key string) (*oauth2.Config, error) {
	cfg, err := oauthConfigForRefresh(ctx, v, a)
	if err == nil || a.OAuthConfig() != nil {
		return cfg, err
	}
	raw, readErr := v.Get(ctx, userID, key)
	var stored struct {
		Token        string `json:"token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if readErr == nil && json.Unmarshal(raw, &stored) == nil && stored.Token != "" && stored.AccessToken == "" && stored.RefreshToken == "" {
		return nil, nil
	}
	return nil, err
}

// Stripes bound memory use. Shared credentials use their actual owner's key,
// so different users do not redeem the same shared refresh token concurrently.
var oauthCredentialLocks [64]sync.Mutex

func oauthCredentialLock(userID, vaultKey string) *sync.Mutex {
	h := fnv.New64a()
	_, _ = h.Write([]byte(userID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(vaultKey))
	return &oauthCredentialLocks[h.Sum64()%uint64(len(oauthCredentialLocks))]
}

func safeOAuthLoadError(err error) error {
	for _, known := range []error{vault.ErrNotFound, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	return errOAuthCredentialLoad
}

func lockedOAuthCredential(ctx context.Context, v vault.Vault, userID, key string) (*vault.ConditionalCredential, func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		target, err := vault.ReadForConditionalUpdate(ctx, v, userID, key)
		if err != nil {
			return nil, nil, safeOAuthLoadError(err)
		}
		if target.Updater == nil {
			return target, func() {}, nil
		}
		lock := oauthCredentialLock(target.UserID, target.ServiceID)
		lock.Lock()
		if err := ctx.Err(); err != nil {
			lock.Unlock()
			return nil, nil, err
		}
		current, err := vault.ReadForConditionalUpdate(ctx, v, userID, key)
		if err != nil {
			lock.Unlock()
			return nil, nil, safeOAuthLoadError(err)
		}
		if current.UserID != target.UserID || current.ServiceID != target.ServiceID {
			lock.Unlock()
			continue
		}
		return current, lock.Unlock, nil
	}
}

type loadedOAuthCredential struct {
	raw     []byte
	managed bool
}

func oauthTokenFromCredential(raw []byte) (*oauth2.Token, error) {
	var stored oauthCredentialForResolver
	if json.Unmarshal(raw, &stored) != nil || (stored.AccessToken == "" && stored.RefreshToken == "") {
		return nil, errOAuthCredentialInvalid
	}
	return &oauth2.Token{AccessToken: stored.AccessToken, RefreshToken: stored.RefreshToken, Expiry: stored.Expiry, TokenType: "Bearer"}, nil
}

// Once a renewal starts, finish receiving its rotated token even if the caller
// disconnects. A total deadline also bounds oauth2's authentication-style retry.
func exchangeOAuthToken(ctx context.Context, cfg *oauth2.Config, token *oauth2.Token) (*oauth2.Token, error) {
	if token.RefreshToken == "" || cfg.ClientID == "" || validateTokenEndpoint(cfg.Endpoint.TokenURL) != nil {
		return nil, errOAuthRefreshFailed
	}
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	client := *ssrfSafeOAuthClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	refreshCtx = context.WithValue(refreshCtx, oauth2.HTTPClient, &client)
	token, err := cfg.TokenSource(refreshCtx, token).Token()
	if err != nil || token == nil || token.AccessToken == "" {
		return nil, errOAuthRefreshFailed
	}
	return token, nil
}

// The configured routing wrappers return the effective owner and conditional
// updater together with the credential. Read-only references and unsupported
// backends never gain an unconditional write through this path.
func loadRefreshedCredential(ctx context.Context, v vault.Vault, userID, key string, cfg *oauth2.Config) (*loadedOAuthCredential, error) {
	if cfg == nil {
		raw, err := v.Get(ctx, userID, key)
		return &loadedOAuthCredential{raw: raw}, err
	}
	target, unlock, err := lockedOAuthCredential(ctx, v, userID, key)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var static struct {
		Token        string `json:"token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal(target.Value, &static) != nil {
		return nil, errOAuthCredentialInvalid
	}
	if static.Token != "" && static.AccessToken == "" && static.RefreshToken == "" {
		return &loadedOAuthCredential{raw: target.Value}, nil
	}
	token, err := oauthTokenFromCredential(target.Value)
	if err != nil {
		return nil, err
	}
	if target.Updater == nil {
		return &loadedOAuthCredential{raw: target.Value}, nil
	}
	if token.Valid() {
		return &loadedOAuthCredential{raw: target.Value, managed: true}, nil
	}
	token, err = exchangeOAuthToken(ctx, cfg, token)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(target.Value, &fields) != nil || fields == nil {
		return nil, errOAuthCredentialInvalid
	}
	fields["access_token"], _ = json.Marshal(token.AccessToken)
	fields["refresh_token"], _ = json.Marshal(token.RefreshToken)
	fields["expiry"], _ = json.Marshal(token.Expiry)
	updated, err := json.Marshal(fields)
	if err != nil {
		return nil, errOAuthCredentialInvalid
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	swapped, err := target.Updater.CompareAndSwap(persistCtx, target.UserID, target.ServiceID, target.Value, updated)
	if err != nil {
		return nil, errOAuthPersistFailed
	}
	if !swapped {
		return nil, errOAuthCredentialChanged
	}
	// Preserve the shared rotation, but do not execute using it if the caller
	// switched to a personal credential while the shared token was being renewed.
	current, err := vault.ReadForConditionalUpdate(persistCtx, v, userID, key)
	if err != nil || current.UserID != target.UserID || current.ServiceID != target.ServiceID || !bytes.Equal(current.Value, updated) {
		return nil, errOAuthCredentialChanged
	}
	return &loadedOAuthCredential{raw: updated, managed: true}, nil
}

type vaultOAuthTokenSource struct {
	ctx      context.Context
	v        vault.Vault
	cfg      *oauth2.Config
	userID   string
	vaultKey string
	mu       sync.Mutex
	initial  *oauth2.Token
}

func (s *vaultOAuthTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if initial := s.initial; initial != nil {
		s.initial = nil
		// A freshly issued short-lived token is usable until its actual expiry;
		// don't immediately redeem it again because of oauth2's early-expiry margin.
		if initial.Expiry.IsZero() || time.Now().Before(initial.Expiry) {
			return initial, nil
		}
	}
	loaded, err := loadRefreshedCredential(s.ctx, s.v, s.userID, s.vaultKey, s.cfg)
	if err != nil {
		return nil, err
	}
	if !loaded.managed {
		return nil, errOAuthCredentialChanged
	}
	return oauthTokenFromCredential(loaded.raw)
}

func oauthFailureCategory(err error) string {
	switch {
	case errors.Is(err, errOAuthCredentialInvalid):
		return "invalid"
	case errors.Is(err, errOAuthConfiguration):
		return "configuration"
	case errors.Is(err, errOAuthRefreshFailed):
		return "refresh"
	case errors.Is(err, errOAuthPersistFailed):
		return "persist"
	case errors.Is(err, errOAuthCredentialChanged):
		return "changed"
	case errors.Is(err, vault.ErrNotFound):
		return "not_found"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "load"
	}
}
