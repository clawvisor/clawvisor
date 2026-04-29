package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type Manager struct {
	Store  store.Store
	Config *config.Config
	Logger *slog.Logger
	Proxy  *Server
}

type CreateSessionRequest struct {
	Mode            string
	ObservationMode *bool
	TTLSeconds      int
	Metadata        map[string]any
}

type CreateSessionResult struct {
	Session         *store.RuntimeSession `json:"session"`
	ProxyBearer     string                `json:"proxy_bearer_secret"`
	ProxyURL        string                `json:"proxy_url"`
	CACertPEM       string                `json:"ca_cert_pem,omitempty"`
	ObservationMode bool                  `json:"observation_mode"`
}

func (m *Manager) CreateRuntimeSession(ctx context.Context, agentID, userID string, req CreateSessionRequest) (*CreateSessionResult, error) {
	if m.Config == nil {
		return nil, fmt.Errorf("runtime config is unavailable")
	}
	if req.Mode == "" {
		req.Mode = "proxy"
	}
	if req.TTLSeconds <= 0 {
		req.TTLSeconds = m.Config.RuntimeProxy.SessionTTLSeconds
	}
	observation := m.Config.RuntimePolicy.ObservationModeDefault
	if req.ObservationMode != nil {
		observation = *req.ObservationMode
	}
	secret, err := mintRuntimeSecret()
	if err != nil {
		return nil, err
	}
	var metadataJSON json.RawMessage
	if len(req.Metadata) > 0 {
		b, err := json.Marshal(req.Metadata)
		if err != nil {
			return nil, fmt.Errorf("marshal runtime session metadata: %w", err)
		}
		metadataJSON = json.RawMessage(b)
	}
	session := &store.RuntimeSession{
		ID:                    uuid.NewString(),
		UserID:                userID,
		AgentID:               agentID,
		Mode:                  req.Mode,
		ProxyBearerSecretHash: HashProxyBearerSecret(secret),
		ObservationMode:       observation,
		MetadataJSON:          metadataJSON,
		ExpiresAt:             time.Now().UTC().Add(time.Duration(req.TTLSeconds) * time.Second),
	}
	if err := m.Store.CreateRuntimeSession(ctx, session); err != nil {
		return nil, err
	}
	return &CreateSessionResult{
		Session:         session,
		ProxyBearer:     secret,
		ProxyURL:        m.ProxyURL(),
		CACertPEM:       m.CACertPEM(),
		ObservationMode: observation,
	}, nil
}

func (m *Manager) ProxyURL() string {
	if m.Proxy == nil {
		return ""
	}
	scheme := "http"
	if m.Config != nil && m.Config.RuntimeProxy.TLS {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, m.Proxy.Addr())
}

func (m *Manager) CACertPEM() string {
	if m.Proxy == nil || m.Proxy.CA() == nil {
		return ""
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: m.Proxy.CA().Raw,
	}))
}

func (m *Manager) ListRuntimeSessionsForUser(ctx context.Context, userID string) ([]*store.RuntimeSession, error) {
	agents, err := m.Store.ListAgents(ctx, userID)
	if err != nil {
		return nil, err
	}
	var out []*store.RuntimeSession
	for _, agent := range agents {
		sessions, err := m.Store.ListRuntimeSessionsByAgent(ctx, agent.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, sessions...)
	}
	return out, nil
}

func (m *Manager) RevokeRuntimeSession(ctx context.Context, sessionID string) error {
	return m.Store.RevokeRuntimeSession(ctx, sessionID, time.Now().UTC())
}

func mintRuntimeSecret() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate runtime session secret: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
