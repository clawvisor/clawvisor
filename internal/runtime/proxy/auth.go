package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
var ErrProxyAuthorizationUnavailable = errors.New("proxy authorization unavailable")

func HashProxyBearerSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func ExtractBearerSecret(header http.Header) (string, error) {
	_, secret, err := ExtractBearerCredentials(header)
	return secret, err
}

// ExtractBearerCredentials returns both the username and the secret from the
// Proxy-Authorization header. Bearer schemes have no username; for those the
// returned username is empty. Basic schemes return the decoded user component
// alongside the password. Empty username/password components are rejected the
// same way they were before this helper existed.
func ExtractBearerCredentials(header http.Header) (string, string, error) {
	value := strings.TrimSpace(header.Get("Proxy-Authorization"))
	if value == "" {
		return "", "", ErrProxyAuthorizationRequired
	}
	scheme, token, ok := strings.Cut(value, " ")
	if !ok || strings.TrimSpace(token) == "" {
		return "", "", ErrProxyAuthorizationRejected
	}
	switch {
	case strings.EqualFold(strings.TrimSpace(scheme), "Bearer"):
		return "", strings.TrimSpace(token), nil
	case strings.EqualFold(strings.TrimSpace(scheme), "Basic"):
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(token))
		if err != nil {
			return "", "", ErrProxyAuthorizationRejected
		}
		username, password, ok := strings.Cut(string(decoded), ":")
		if !ok || strings.TrimSpace(password) == "" {
			return "", "", ErrProxyAuthorizationRejected
		}
		return strings.TrimSpace(username), strings.TrimSpace(password), nil
	default:
		return "", "", ErrProxyAuthorizationRejected
	}
}

// ParseLaunchID returns the launch UUID encoded in a proxy username slot. The
// expected form is "launch-<uuid>"; any other username (including the legacy
// "clawvisor" or an empty Bearer username) yields an empty string so callers
// fall back to the agent-scoped reuse key.
func ParseLaunchID(username string) string {
	username = strings.TrimSpace(username)
	if username == "" {
		return ""
	}
	const prefix = "launch-"
	if !strings.HasPrefix(username, prefix) {
		return ""
	}
	rest := strings.TrimSpace(username[len(prefix):])
	if rest == "" {
		return ""
	}
	return rest
}

type Authenticator struct {
	Store  store.Store
	Config *config.Config
	Logger *slog.Logger

	mu sync.Mutex
}

const (
	agentTokenRuntimeSessionLauncher = "runtime-proxy-agent-token"
	agentTokenRuntimeSessionAuthMode = "agent_token"
)

func (a *Authenticator) Authenticate(ctx context.Context, header http.Header) (*store.RuntimeSession, error) {
	username, secret, err := ExtractBearerCredentials(header)
	if err != nil {
		return nil, err
	}
	launchID := ParseLaunchID(username)
	session, err := a.Store.GetRuntimeSessionByProxyBearerSecretHash(ctx, HashProxyBearerSecret(secret))
	if err == nil {
		if session.RevokedAt != nil || !session.ExpiresAt.After(time.Now().UTC()) {
			return nil, ErrProxyAuthorizationRejected
		}
		return session, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%w: load runtime session: %v", ErrProxyAuthorizationUnavailable, err)
	}

	agent, err := a.Store.GetAgentByToken(ctx, intauth.HashToken(secret))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrProxyAuthorizationRejected
		}
		return nil, fmt.Errorf("%w: load agent by token: %v", ErrProxyAuthorizationUnavailable, err)
	}
	return a.getOrCreateAgentRuntimeSession(ctx, agent, launchID)
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

func (a *Authenticator) getOrCreateAgentRuntimeSession(ctx context.Context, agent *store.Agent, launchID string) (*store.RuntimeSession, error) {
	if agent == nil {
		return nil, ErrProxyAuthorizationRejected
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now().UTC()
	ttlSeconds := 1800
	settings := mergedAgentRuntimeSettings(agent, a.Config)
	wantObservation := strings.EqualFold(settings.RuntimeMode, "observe")
	if a.Config != nil && a.Config.RuntimeProxy.SessionTTLSeconds > 0 {
		ttlSeconds = a.Config.RuntimeProxy.SessionTTLSeconds
	}
	ttl := time.Duration(ttlSeconds) * time.Second

	sessions, err := a.Store.ListRuntimeSessionsByAgent(ctx, agent.ID)
	if err != nil {
		return nil, fmt.Errorf("%w: list runtime sessions: %v", ErrProxyAuthorizationUnavailable, err)
	}
	if existing := selectReusableAgentTokenRuntimeSession(sessions, now, wantObservation, settings, launchID); existing != nil {
		a.maybeExtendSessionExpiry(ctx, existing, now, ttl)
		return existing, nil
	}

	secret, err := mintRuntimeSecret()
	if err != nil {
		return nil, fmt.Errorf("%w: mint runtime secret: %v", ErrProxyAuthorizationUnavailable, err)
	}
	metadata := map[string]any{
		"launcher":                 agentTokenRuntimeSessionLauncher,
		"proxy_auth_mode":          agentTokenRuntimeSessionAuthMode,
		"runtime_enabled":          settings.RuntimeEnabled,
		"runtime_mode":             settings.RuntimeMode,
		"starter_profile":          settings.StarterProfile,
		"outbound_credential_mode": settings.OutboundCredentialMode,
		"inject_stored_bearer":     settings.InjectStoredBearer,
	}
	if launchID != "" {
		metadata["launch_id"] = launchID
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal runtime session metadata: %v", ErrProxyAuthorizationUnavailable, err)
	}
	session := &store.RuntimeSession{
		ID:                    uuid.NewString(),
		UserID:                agent.UserID,
		AgentID:               agent.ID,
		Mode:                  "proxy",
		ProxyBearerSecretHash: HashProxyBearerSecret(secret),
		ObservationMode:       wantObservation,
		MetadataJSON:          metadataJSON,
		ExpiresAt:             now.Add(ttl),
	}
	if err := a.Store.CreateRuntimeSession(ctx, session); err != nil {
		return nil, fmt.Errorf("%w: create runtime session: %v", ErrProxyAuthorizationUnavailable, err)
	}
	return session, nil
}

// maybeExtendSessionExpiry pushes the session's expires_at forward when it is
// being actively used and less than half of the configured TTL remains. This
// keeps a single agent-token session alive across long-running container
// workloads without minting a fresh row every TTL/2 window. Failures are
// swallowed because authentication should not be denied if the bookkeeping
// UPDATE fails.
func (a *Authenticator) maybeExtendSessionExpiry(ctx context.Context, session *store.RuntimeSession, now time.Time, ttl time.Duration) {
	if session == nil || ttl <= 0 {
		return
	}
	remaining := session.ExpiresAt.Sub(now)
	if remaining >= ttl/2 {
		return
	}
	newExpiry := now.Add(ttl)
	if !newExpiry.After(session.ExpiresAt) {
		return
	}
	if err := a.Store.UpdateRuntimeSessionExpiry(ctx, session.ID, newExpiry); err != nil {
		if a.Logger != nil {
			a.Logger.Warn("failed to extend runtime session expiry", "session_id", session.ID, "err", err)
		}
		return
	}
	session.ExpiresAt = newExpiry
}

func selectReusableAgentTokenRuntimeSession(sessions []*store.RuntimeSession, now time.Time, observation bool, want sessionRuntimeSettings, launchID string) *store.RuntimeSession {
	var best *store.RuntimeSession
	for _, session := range sessions {
		if session == nil || session.RevokedAt != nil || !session.ExpiresAt.After(now) || session.ObservationMode != observation {
			continue
		}
		if !isAgentTokenRuntimeSession(session.MetadataJSON) {
			continue
		}
		if sessionLaunchID(session.MetadataJSON) != launchID {
			continue
		}
		got := sessionRuntimeSettingsFromMetadata(session, nil)
		if got.RuntimeEnabled != want.RuntimeEnabled || got.RuntimeMode != want.RuntimeMode || got.StarterProfile != want.StarterProfile || got.OutboundCredentialMode != want.OutboundCredentialMode || got.InjectStoredBearer != want.InjectStoredBearer {
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

func sessionLaunchID(metadata json.RawMessage) string {
	if len(metadata) == 0 {
		return ""
	}
	var parsed struct {
		LaunchID string `json:"launch_id"`
	}
	if err := json.Unmarshal(metadata, &parsed); err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.LaunchID)
}
