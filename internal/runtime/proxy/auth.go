package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

var ErrProxyAuthorizationRequired = errors.New("proxy authorization required")
var ErrProxyAuthorizationRejected = errors.New("proxy authorization rejected")

func HashProxyBearerSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func ExtractBearerSecret(header http.Header) (string, error) {
	value := strings.TrimSpace(header.Get("Proxy-Authorization"))
	if value == "" {
		return "", ErrProxyAuthorizationRequired
	}
	scheme, token, ok := strings.Cut(value, " ")
	if !ok || strings.TrimSpace(token) == "" {
		return "", ErrProxyAuthorizationRejected
	}
	switch {
	case strings.EqualFold(strings.TrimSpace(scheme), "Bearer"):
		return strings.TrimSpace(token), nil
	case strings.EqualFold(strings.TrimSpace(scheme), "Basic"):
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(token))
		if err != nil {
			return "", ErrProxyAuthorizationRejected
		}
		_, password, ok := strings.Cut(string(decoded), ":")
		if !ok || strings.TrimSpace(password) == "" {
			return "", ErrProxyAuthorizationRejected
		}
		return strings.TrimSpace(password), nil
	default:
		return "", ErrProxyAuthorizationRejected
	}
}

type Authenticator struct {
	Store store.Store
}

func (a Authenticator) Authenticate(ctx context.Context, header http.Header) (*store.RuntimeSession, error) {
	secret, err := ExtractBearerSecret(header)
	if err != nil {
		return nil, err
	}
	session, err := a.Store.GetRuntimeSessionByProxyBearerSecretHash(ctx, HashProxyBearerSecret(secret))
	if err != nil {
		return nil, ErrProxyAuthorizationRejected
	}
	if session.RevokedAt != nil || !session.ExpiresAt.After(time.Now().UTC()) {
		return nil, ErrProxyAuthorizationRejected
	}
	return session, nil
}

func ProxyURLWithSecret(proxyURL, secret string) (string, error) {
	if strings.TrimSpace(proxyURL) == "" {
		return "", fmt.Errorf("proxy URL is required")
	}
	req, err := http.NewRequest(http.MethodGet, proxyURL, nil)
	if err != nil {
		return "", fmt.Errorf("parse proxy URL: %w", err)
	}
	u := req.URL
	u.User = nil
	if secret != "" {
		u.User = url.UserPassword("clawvisor", secret)
	}
	return u.String(), nil
}
