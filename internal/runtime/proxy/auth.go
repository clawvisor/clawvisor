package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
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
	if !ok || !strings.EqualFold(strings.TrimSpace(scheme), "Bearer") || strings.TrimSpace(token) == "" {
		return "", ErrProxyAuthorizationRejected
	}
	return strings.TrimSpace(token), nil
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
