package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	intauth "github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/pkg/config"
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
	Store  store.Store
	Config *config.Config

	mu sync.Mutex
}

const (
	agentTokenRuntimeSessionLauncher = "runtime-proxy-agent-token"
	agentTokenRuntimeSessionAuthMode = "agent_token"
)

func (a *Authenticator) Authenticate(ctx context.Context, header http.Header) (*store.RuntimeSession, error) {
	secret, err := ExtractBearerSecret(header)
	if err != nil {
		return nil, err
	}
	session, err := a.Store.GetRuntimeSessionByProxyBearerSecretHash(ctx, HashProxyBearerSecret(secret))
	if err == nil {
		if session.RevokedAt != nil || !session.ExpiresAt.After(time.Now().UTC()) {
			return nil, ErrProxyAuthorizationRejected
		}
		return session, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, ErrProxyAuthorizationRejected
	}

	agent, err := a.Store.GetAgentByToken(ctx, intauth.HashToken(secret))
	if err != nil {
		return nil, ErrProxyAuthorizationRejected
	}
	return a.getOrCreateAgentRuntimeSession(ctx, agent)
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

func (a *Authenticator) getOrCreateAgentRuntimeSession(ctx context.Context, agent *store.Agent) (*store.RuntimeSession, error) {
	if agent == nil {
		return nil, ErrProxyAuthorizationRejected
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now().UTC()
	wantObservation := false
	ttlSeconds := 1800
	if a.Config != nil {
		wantObservation = a.Config.RuntimePolicy.ObservationModeDefault
		if a.Config.RuntimeProxy.SessionTTLSeconds > 0 {
			ttlSeconds = a.Config.RuntimeProxy.SessionTTLSeconds
		}
	}

	sessions, err := a.Store.ListRuntimeSessionsByAgent(ctx, agent.ID)
	if err != nil {
		return nil, ErrProxyAuthorizationRejected
	}
	if existing := selectReusableAgentTokenRuntimeSession(sessions, now, wantObservation); existing != nil {
		return existing, nil
	}

	secret, err := mintRuntimeSecret()
	if err != nil {
		return nil, ErrProxyAuthorizationRejected
	}
	metadataJSON, err := json.Marshal(map[string]any{
		"launcher":        agentTokenRuntimeSessionLauncher,
		"proxy_auth_mode": agentTokenRuntimeSessionAuthMode,
	})
	if err != nil {
		return nil, ErrProxyAuthorizationRejected
	}
	session := &store.RuntimeSession{
		ID:                    uuid.NewString(),
		UserID:                agent.UserID,
		AgentID:               agent.ID,
		Mode:                  "proxy",
		ProxyBearerSecretHash: HashProxyBearerSecret(secret),
		ObservationMode:       wantObservation,
		MetadataJSON:          metadataJSON,
		ExpiresAt:             now.Add(time.Duration(ttlSeconds) * time.Second),
	}
	if err := a.Store.CreateRuntimeSession(ctx, session); err != nil {
		return nil, ErrProxyAuthorizationRejected
	}
	return session, nil
}

func selectReusableAgentTokenRuntimeSession(sessions []*store.RuntimeSession, now time.Time, observation bool) *store.RuntimeSession {
	var best *store.RuntimeSession
	for _, session := range sessions {
		if session == nil || session.RevokedAt != nil || !session.ExpiresAt.After(now) || session.ObservationMode != observation {
			continue
		}
		if !isAgentTokenRuntimeSession(session.MetadataJSON) {
			continue
		}
		if best == nil || session.CreatedAt.After(best.CreatedAt) {
			best = session
		}
	}
	return best
}

func isAgentTokenRuntimeSession(metadata json.RawMessage) bool {
	if len(metadata) == 0 {
		return false
	}
	var parsed struct {
		Launcher      string `json:"launcher"`
		ProxyAuthMode string `json:"proxy_auth_mode"`
	}
	if err := json.Unmarshal(metadata, &parsed); err != nil {
		return false
	}
	return parsed.Launcher == agentTokenRuntimeSessionLauncher && parsed.ProxyAuthMode == agentTokenRuntimeSessionAuthMode
}
