package handlers

import (
	"encoding/json"
	"sync"
	"time"
)

// OAuthStateStore abstracts storage of pending OAuth flow state (standard
// OAuth, device flows, PKCE flows) so it works across multiple instances.
type OAuthStateStore interface {
	StoreOAuth(state string, entry oauthStateEntry)
	LoadAndDeleteOAuth(state string) (oauthStateEntry, bool)

	StoreDeviceFlow(flowID string, entry deviceFlowEntry)
	LoadDeviceFlow(flowID string) (deviceFlowEntry, bool)
	UpdateDeviceFlow(flowID string, entry deviceFlowEntry)
	DeleteDeviceFlow(flowID string)

	StorePKCE(state string, entry pkceFlowEntry)
	LoadAndDeletePKCE(state string) (pkceFlowEntry, bool)

	// Cleanup removes expired entries. No-op for Redis (TTL handles it).
	Cleanup()
}

// memoryOAuthStateStore is the default in-memory implementation.
type memoryOAuthStateStore struct {
	oauthStates sync.Map
	deviceFlows sync.Map
	pkceFlows   sync.Map
}

func newMemoryOAuthStateStore() *memoryOAuthStateStore {
	return &memoryOAuthStateStore{}
}

func (s *memoryOAuthStateStore) StoreOAuth(state string, entry oauthStateEntry) {
	s.oauthStates.Store(state, entry)
}

func (s *memoryOAuthStateStore) LoadAndDeleteOAuth(state string) (oauthStateEntry, bool) {
	val, ok := s.oauthStates.LoadAndDelete(state)
	if !ok {
		return oauthStateEntry{}, false
	}
	return val.(oauthStateEntry), true
}

func (s *memoryOAuthStateStore) StoreDeviceFlow(flowID string, entry deviceFlowEntry) {
	s.deviceFlows.Store(flowID, entry)
}

func (s *memoryOAuthStateStore) LoadDeviceFlow(flowID string) (deviceFlowEntry, bool) {
	val, ok := s.deviceFlows.Load(flowID)
	if !ok {
		return deviceFlowEntry{}, false
	}
	return val.(deviceFlowEntry), true
}

func (s *memoryOAuthStateStore) UpdateDeviceFlow(flowID string, entry deviceFlowEntry) {
	s.deviceFlows.Store(flowID, entry)
}

func (s *memoryOAuthStateStore) DeleteDeviceFlow(flowID string) {
	s.deviceFlows.Delete(flowID)
}

func (s *memoryOAuthStateStore) StorePKCE(state string, entry pkceFlowEntry) {
	s.pkceFlows.Store(state, entry)
}

func (s *memoryOAuthStateStore) LoadAndDeletePKCE(state string) (pkceFlowEntry, bool) {
	val, ok := s.pkceFlows.LoadAndDelete(state)
	if !ok {
		return pkceFlowEntry{}, false
	}
	return val.(pkceFlowEntry), true
}

func (s *memoryOAuthStateStore) Cleanup() {
	now := time.Now()
	s.oauthStates.Range(func(key, value any) bool {
		if value.(oauthStateEntry).ExpiresAt.Before(now) {
			s.oauthStates.Delete(key)
		}
		return true
	})
}

// oauthStateJSON / deviceFlowJSON / pkceFlowJSON are the JSON-serializable
// forms for Redis storage.
type oauthStateJSON struct {
	UserID       string            `json:"user_id"`
	ServiceID    string            `json:"service_id"`
	Alias        string            `json:"alias"`
	PendingReqID string            `json:"pending_req_id,omitempty"`
	CLICallback  string            `json:"cli_callback,omitempty"`
	Scopes       []string          `json:"scopes,omitempty"`
	Config       map[string]string `json:"config,omitempty"`
	ExpiresAt    time.Time         `json:"expires_at"`
}

func marshalOAuthState(e oauthStateEntry) ([]byte, error) {
	return json.Marshal(oauthStateJSON{
		UserID:       e.UserID,
		ServiceID:    e.ServiceID,
		Alias:        e.Alias,
		PendingReqID: e.PendingReqID,
		CLICallback:  e.CLICallback,
		Scopes:       e.Scopes,
		Config:       e.Config,
		ExpiresAt:    e.ExpiresAt,
	})
}

func unmarshalOAuthState(data []byte) (oauthStateEntry, error) {
	var j oauthStateJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return oauthStateEntry{}, err
	}
	return oauthStateEntry{
		UserID:       j.UserID,
		ServiceID:    j.ServiceID,
		Alias:        j.Alias,
		PendingReqID: j.PendingReqID,
		CLICallback:  j.CLICallback,
		Scopes:       j.Scopes,
		Config:       j.Config,
		ExpiresAt:    j.ExpiresAt,
	}, nil
}

var _ OAuthStateStore = (*memoryOAuthStateStore)(nil)
