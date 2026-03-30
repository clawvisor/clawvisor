package adapters

import (
	"context"
	"sync"

	"golang.org/x/oauth2"
)

// ── Metadata types ──────────────────────────────────────────────────────────

// MetadataProvider is an optional interface adapters can implement to provide
// display, branding, and risk metadata. YAML adapters implement this automatically;
// Go-only adapters can opt in.
type MetadataProvider interface {
	ServiceMetadata() ServiceMetadata
}

// ServiceMetadata holds all display and risk metadata for a service.
type ServiceMetadata struct {
	DisplayName       string
	Description       string
	SetupURL          string
	VaultKey          string                // shared vault key (e.g. "google" for all google.* services); empty = use service ID
	OAuthEndpoint     string                // well-known OAuth endpoint name (e.g. "google"); empty = not OAuth or no known endpoint
	ActionMeta        map[string]ActionMeta // action_id → metadata
	VerificationHints string
}

// ActionMeta holds display and risk metadata for a single action.
type ActionMeta struct {
	DisplayName string // "List customers"
	Category    string // "read", "write", "delete", "search"
	Sensitivity string // "low", "medium", "high"
	Description string // "List Stripe customers" (for risk assessment)
}

// ActionInfo is returned by the service catalog with per-action metadata.
type ActionInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Category    string `json:"category"`
	Sensitivity string `json:"sensitivity"`
}

// OAuthCredentialProvider supplies OAuth app credentials (client_id, client_secret)
// on demand. This allows the server to read credentials from the vault lazily
// instead of requiring them at startup.
type OAuthCredentialProvider interface {
	// OAuthClientCredentials returns (clientID, clientSecret, redirectURL).
	// Returns empty strings if not yet configured.
	OAuthClientCredentials() (clientID, clientSecret, redirectURL string)
}

// NoopOAuthProvider is a provider that always returns empty credentials.
// Useful in tests where OAuth configuration is not needed.
type NoopOAuthProvider struct{}

func (NoopOAuthProvider) OAuthClientCredentials() (string, string, string) { return "", "", "" }

// SystemUserID is the well-known user ID for system-level vault entries
// (e.g. Google OAuth app credentials). Not a real user.
const SystemUserID = "__system__"

// SystemVaultKeyGoogleOAuth is the vault key for Google OAuth app credentials.
const SystemVaultKeyGoogleOAuth = "google.oauth"

// Request is passed to an adapter's Execute method.
// Credential is injected by the gateway; never logged or returned to the caller.
type Request struct {
	Action     string
	Params     map[string]any
	Credential []byte // decrypted from vault
}

// Result is the semantic output of an adapter action.
type Result struct {
	Summary string `json:"summary"`
	Data    any    `json:"data"`
}

// ContactsChecker is an optional interface implemented by the google.contacts adapter.
// The gateway handler uses it to pre-resolve the recipient_in_contacts policy condition
// before calling the policy evaluator (which must remain a pure function).
type ContactsChecker interface {
	// IsInContacts returns true if the given email address is found in the user's contacts.
	// cred is the raw vault credential bytes (vault key "google").
	IsInContacts(ctx context.Context, cred []byte, email string) (bool, error)
}

// VerificationHinter is an optional interface adapters can implement to provide
// service-specific guidance to the LLM intent verifier. The hints are included
// in the verification prompt only when that adapter is being verified.
type VerificationHinter interface {
	// VerificationHints returns natural-language guidance for the intent verifier
	// about how to interpret this service's parameters (e.g. "thread_ts is
	// within channel scope, not a scope escalation").
	VerificationHints() string
}

// Adapter is the interface every service adapter implements.
type Adapter interface {
	// ServiceID returns the adapter's canonical service identifier (e.g. "google.gmail").
	ServiceID() string
	// SupportedActions returns the list of action names this adapter handles.
	SupportedActions() []string
	// Execute runs the action with the given (credential-injected) request.
	Execute(ctx context.Context, req Request) (*Result, error)
	// OAuthConfig returns the OAuth2 config for authorization code flow.
	// Returns nil if the service uses API keys or a different auth mechanism.
	OAuthConfig() *oauth2.Config
	// CredentialFromToken serializes an OAuth2 token into storable bytes.
	CredentialFromToken(token *oauth2.Token) ([]byte, error)
	// ValidateCredential checks whether stored credential bytes are parseable/valid.
	ValidateCredential(credBytes []byte) error
	// RequiredScopes returns the OAuth scopes this adapter needs.
	// Returns nil for non-OAuth or non-Google adapters.
	RequiredScopes() []string
}

// ServiceInfo is returned by the service catalog endpoint.
type ServiceInfo struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	OAuth       bool     `json:"oauth"`
	Actions     []string `json:"actions"`
}

// Registry holds all registered adapters, keyed by service ID.
// It is safe for concurrent use.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

func NewRegistry() *Registry {
	return &Registry{adapters: make(map[string]Adapter)}
}

func (r *Registry) Register(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[a.ServiceID()] = a
}

func (r *Registry) Get(serviceID string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[serviceID]
	return a, ok
}

func (r *Registry) All() []Adapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Adapter, 0, len(r.adapters))
	for _, a := range r.adapters {
		out = append(out, a)
	}
	return out
}

// Replace swaps an adapter in the registry (used for hot-reload).
func (r *Registry) Replace(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[a.ServiceID()] = a
}

// VaultKey returns the vault key for a service ID. If the adapter implements
// MetadataProvider and declares a VaultKey, that is used; otherwise the
// service ID itself is the vault key.
func (r *Registry) VaultKey(serviceID string) string {
	r.mu.RLock()
	a, ok := r.adapters[serviceID]
	r.mu.RUnlock()
	if ok {
		if mp, ok := a.(MetadataProvider); ok {
			if vk := mp.ServiceMetadata().VaultKey; vk != "" {
				return vk
			}
		}
	}
	return serviceID
}

// VaultKeyWithAlias returns the vault key for a service ID + alias pair.
// "default" or empty alias maps to the plain vault key for backward compatibility.
func (r *Registry) VaultKeyWithAlias(serviceID, alias string) string {
	base := r.VaultKey(serviceID)
	if alias == "" || alias == "default" {
		return base
	}
	return base + ":" + alias
}

func (r *Registry) SupportedServices() []ServiceInfo {
	all := r.All()
	infos := make([]ServiceInfo, 0, len(all))
	for _, a := range all {
		infos = append(infos, ServiceInfo{
			ID:      a.ServiceID(),
			OAuth:   a.OAuthConfig() != nil,
			Actions: a.SupportedActions(),
		})
	}
	return infos
}
