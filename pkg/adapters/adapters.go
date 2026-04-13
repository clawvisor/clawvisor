package adapters

import (
	"context"
	"sync"

	"golang.org/x/oauth2"

	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
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
	IconSVG           string                // inline SVG markup for the service icon
	VaultKey          string                // shared vault key (e.g. "google" for all google.* services); empty = use service ID
	OAuthEndpoint     string                // well-known OAuth endpoint name (e.g. "google"); empty = not OAuth or no known endpoint
	DeviceFlow          bool                  // whether device flow activation is available
	PKCEFlow            bool                  // whether PKCE authorization code flow is available (client ID resolved)
	PKCEFlowDefined     bool                  // whether PKCE flow is defined in the adapter (even without client ID)
	AutoIdentity        bool                  // whether the adapter can auto-detect account identity
	ActionMeta        map[string]ActionMeta // action_id → metadata
	VerificationHints string
	Variables         []VariableMeta // user-configurable variables declared by the adapter
}

// VariableMeta holds metadata for a single user-configurable variable.
type VariableMeta struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required"`
	Default     string `json:"default,omitempty"`
}

// ActionMeta holds display and risk metadata for a single action.
type ActionMeta struct {
	DisplayName string      // "List customers"
	Category    string      // "read", "write", "delete", "search"
	Sensitivity string      // "low", "medium", "high"
	Description string      // "List Stripe customers" (for risk assessment)
	Params      []ParamMeta // ordered parameter documentation
}

// ParamMeta holds documentation metadata for a single action parameter.
type ParamMeta struct {
	Name     string // parameter name as passed in the request
	Type     string // "string", "int", "bool", "object", "array"
	Required bool
	Default  any    // default value, nil if none
	Min      *int   // minimum value (for int params)
	Max      *int   // maximum value (for int params)
}

// ActionInfo is returned by the service catalog with per-action metadata.
type ActionInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Category    string `json:"category"`
	Sensitivity string `json:"sensitivity"`
}

// DeviceFlowProvider is an optional interface for adapters that support
// OAuth2 device authorization grant (RFC 8628).
type DeviceFlowProvider interface {
	DeviceFlowConfig() *yamldef.DeviceFlowDef
}

// PKCEFlowProvider is an optional interface for adapters that support
// OAuth2 authorization code flow with PKCE (RFC 7636).
type PKCEFlowProvider interface {
	PKCEFlowConfig() *yamldef.PKCEFlowDef
}

// OAuthCredentialProvider supplies OAuth app credentials (client_id, client_secret)
// on demand. This allows the server to read credentials from the vault lazily
// instead of requiring them at startup.
type OAuthCredentialProvider interface {
	// OAuthClientCredentials returns (clientID, clientSecret).
	// Returns empty strings if not yet configured.
	OAuthClientCredentials() (clientID, clientSecret string)
}

// NoopOAuthProvider is a provider that always returns empty credentials.
// Useful in tests where OAuth configuration is not needed.
type NoopOAuthProvider struct{}

func (NoopOAuthProvider) OAuthClientCredentials() (string, string) { return "", "" }

// SystemUserID is the well-known user ID for system-level vault entries
// (e.g. Google OAuth app credentials). Not a real user.
const SystemUserID = "__system__"

// SystemVaultKeyGoogleOAuth is the vault key for Google OAuth app credentials.
const SystemVaultKeyGoogleOAuth = "google.oauth"

// SystemVaultKeyPKCEPrefix is the vault key prefix for per-service PKCE client IDs.
// Stored as "__system__" / "pkce.{serviceID}" → {"client_id": "..."}.
const SystemVaultKeyPKCEPrefix = "pkce."

// Request is passed to an adapter's Execute method.
// Credential is injected by the gateway; never logged or returned to the caller.
type Request struct {
	Action     string
	Params     map[string]any
	Credential []byte            // decrypted from vault
	Config     map[string]string // resolved variable values from service_configs
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

// AvailabilityChecker is an optional interface adapters can implement to
// indicate whether they should be shown on the current platform. If Available
// returns false the adapter is hidden from the service catalog, skill catalog,
// and dashboard. The iMessage adapter uses this to hide itself on non-macOS hosts.
type AvailabilityChecker interface {
	Available() bool
}

// ActivationChecker is an optional interface adapters can implement to
// validate that the adapter can actually function before activation.
// For example, the iMessage adapter uses this to attempt opening chat.db,
// which triggers macOS to register the app in Full Disk Access settings.
// If CheckPermissions returns an error, the activation is rejected with that message.
type ActivationChecker interface {
	CheckPermissions() error
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

// IdentityFetcher is an optional interface adapters can implement to
// auto-discover the account identity after activation (e.g. the email
// address for a Google account, the username for GitHub). When implemented,
// the returned identity is used as the service alias instead of "default".
type IdentityFetcher interface {
	// FetchIdentity uses the stored credential to query the service for a
	// human-readable account identifier (e.g. "levine.eric.j@gmail.com",
	// "octocat"). Returns an empty string if identity cannot be determined.
	FetchIdentity(ctx context.Context, credential []byte, config map[string]string) (string, error)
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

// ParamInfo describes a single action parameter for validation and error reporting.
type ParamInfo struct {
	Name     string `json:"name"`
	Type     string `json:"type"`               // "string", "int", "bool", "object", "array"
	Required bool   `json:"required"`
}

// ActionParamDescriber is an optional interface that adapters can implement
// to expose parameter metadata for pre-execution validation.
type ActionParamDescriber interface {
	// ActionParams returns the parameter definitions for the given action.
	// Returns nil if no param info is available.
	ActionParams(action string) []ParamInfo
}

// ServiceInfo is returned by the service catalog endpoint.
type ServiceInfo struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	OAuth       bool     `json:"oauth"`
	Actions     []string `json:"actions"`
}

// AdapterResolver loads an adapter on demand for a specific user.
// Used in cloud mode to lazily resolve user-generated adapters from the database.
type AdapterResolver func(ctx context.Context, serviceID, userID string) (Adapter, bool)

// Registry holds all registered adapters, keyed by service ID.
// It is safe for concurrent use.
type Registry struct {
	mu           sync.RWMutex
	adapters     map[string]Adapter                // built-in (shared) adapters
	userAdapters map[string]map[string]Adapter     // userID → serviceID → adapter (per-user generated)
	resolver     AdapterResolver                   // optional; called on cache miss by GetForUser
	fallback     func(ctx context.Context, serviceID string) (Adapter, bool) // cloud-injected resolver for custom adapters
}

func NewRegistry() *Registry {
	return &Registry{
		adapters:     make(map[string]Adapter),
		userAdapters: make(map[string]map[string]Adapter),
	}
}

// SetResolver sets an optional fallback resolver for user-scoped adapters.
// Called by GetForUser when the service ID is not found in the built-in registry.
func (r *Registry) SetResolver(fn AdapterResolver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolver = fn
}

func (r *Registry) Register(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[a.ServiceID()] = a
}

// Get returns an adapter by service ID from the built-in registry only.
func (r *Registry) Get(serviceID string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[serviceID]
	return a, ok
}

// GetForUser returns an adapter by service ID, checking the shared registry
// first, then the per-user cache, then falling back to the resolver.
// Resolved adapters are cached per-user to avoid cross-tenant leaks.
func (r *Registry) GetForUser(ctx context.Context, serviceID, userID string) (Adapter, bool) {
	r.mu.RLock()
	// Check shared (built-in) adapters first.
	a, ok := r.adapters[serviceID]
	if ok {
		r.mu.RUnlock()
		return a, true
	}
	// Check per-user cache.
	if userID != "" {
		if userMap, exists := r.userAdapters[userID]; exists {
			if a, ok = userMap[serviceID]; ok {
				r.mu.RUnlock()
				return a, true
			}
		}
	}
	resolver := r.resolver
	r.mu.RUnlock()

	if resolver == nil || userID == "" {
		return nil, false
	}

	a, ok = resolver(ctx, serviceID, userID)
	if !ok {
		return nil, false
	}

	// Cache per-user so subsequent requests don't hit the DB.
	r.mu.Lock()
	if r.userAdapters[userID] == nil {
		r.userAdapters[userID] = make(map[string]Adapter)
	}
	r.userAdapters[userID][serviceID] = a
	r.mu.Unlock()
	return a, true
}

// SetFallback registers a resolver for service IDs not in the built-in map.
// Used by cloud to resolve custom adapters and MCP servers per-org.
func (r *Registry) SetFallback(fn func(ctx context.Context, serviceID string) (Adapter, bool)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fallback = fn
}

// GetWithContext checks the built-in map, then the fallback resolver.
// Use this instead of Get when context-dependent resolution (e.g. org-scoped
// custom adapters) is needed.
func (r *Registry) GetWithContext(ctx context.Context, serviceID string) (Adapter, bool) {
	if a, ok := r.Get(serviceID); ok {
		return a, true
	}
	r.mu.RLock()
	fb := r.fallback
	r.mu.RUnlock()
	if fb != nil {
		return fb(ctx, serviceID)
	}
	return nil, false
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

// Remove deletes an adapter from the shared registry by service ID.
func (r *Registry) Remove(serviceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.adapters, serviceID)
}

// RemoveForUser deletes a user-generated adapter from the per-user cache.
func (r *Registry) RemoveForUser(serviceID, userID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if userMap, ok := r.userAdapters[userID]; ok {
		delete(userMap, serviceID)
		if len(userMap) == 0 {
			delete(r.userAdapters, userID)
		}
	}
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

// VaultKeyForUser returns the vault key for a service ID, checking both the
// shared registry and the per-user cache.
func (r *Registry) VaultKeyForUser(serviceID, userID string) string {
	r.mu.RLock()
	a, ok := r.adapters[serviceID]
	if !ok && userID != "" {
		if userMap, exists := r.userAdapters[userID]; exists {
			a, ok = userMap[serviceID]
		}
	}
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

// VaultKeyWithAliasForUser returns the vault key for a service ID + alias pair,
// checking both the shared registry and the per-user cache.
func (r *Registry) VaultKeyWithAliasForUser(serviceID, alias, userID string) string {
	base := r.VaultKeyForUser(serviceID, userID)
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
