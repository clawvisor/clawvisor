// Package yamlruntime implements the adapters.Adapter interface for YAML-defined services.
package yamlruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
)

// ActionFunc is a Go function that handles a single action, used for overrides.
type ActionFunc func(ctx context.Context, req adapters.Request) (*adapters.Result, error)

// YAMLAdapter implements adapters.Adapter from a YAML service definition.
type YAMLAdapter struct {
	def           yamldef.ServiceDef
	overrides     map[string]ActionFunc              // action_name → Go override
	oauthProvider adapters.OAuthCredentialProvider    // lazy OAuth credential source
}

// New creates a YAMLAdapter from a parsed service definition.
// overrides maps action names to Go functions for actions too complex for YAML.
func New(def yamldef.ServiceDef, overrides map[string]ActionFunc) *YAMLAdapter {
	if overrides == nil {
		overrides = map[string]ActionFunc{}
	}
	return &YAMLAdapter{def: def, overrides: overrides}
}

func (a *YAMLAdapter) ServiceID() string { return a.def.Service.ID }

func (a *YAMLAdapter) SupportedActions() []string {
	actions := make([]string, 0, len(a.def.Actions))
	for name := range a.def.Actions {
		actions = append(actions, name)
	}
	sort.Strings(actions)
	return actions
}

func (a *YAMLAdapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	action, ok := a.def.Actions[req.Action]
	if !ok {
		return nil, fmt.Errorf("%s: unsupported action %q", a.def.Service.ID, req.Action)
	}

	// Check for Go override.
	if action.Override == "go" {
		if fn, ok := a.overrides[req.Action]; ok {
			return fn(ctx, req)
		}
		return nil, fmt.Errorf("%s: action %q requires Go override but none registered", a.def.Service.ID, req.Action)
	}
	if fn, ok := a.overrides[req.Action]; ok {
		return fn(ctx, req)
	}

	// Validate required params.
	if err := validateRequiredParams(req.Params, action.Params, a.def.Service.ID, req.Action); err != nil {
		return nil, err
	}

	// Build the authenticated HTTP client.
	client, err := buildHTTPClient(a.def.Auth, req.Credential)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", a.def.Service.ID, err)
	}

	// Get credential fields for path interpolation.
	credFields, _ := credentialFields(a.def.Auth, req.Credential)

	switch a.def.API.Type {
	case "rest":
		return executeREST(ctx, client, a.def.API.BaseURL, action, req.Params, credFields)
	case "graphql":
		return executeGraphQL(ctx, client, a.def.API.BaseURL, action, req.Params, a.def.Service.ID)
	default:
		return nil, fmt.Errorf("%s: unsupported API type %q", a.def.Service.ID, a.def.API.Type)
	}
}

func (a *YAMLAdapter) OAuthConfig() *oauth2.Config {
	if a.def.Auth.Type != "oauth2" || a.def.Auth.OAuth == nil {
		return nil
	}

	// Read OAuth app credentials lazily from the provider.
	var clientID, clientSecret, redirectURL string
	if a.oauthProvider != nil {
		clientID, clientSecret, redirectURL = a.oauthProvider.OAuthClientCredentials()
	}
	if clientID == "" {
		return nil // OAuth not yet configured
	}

	oauthDef := a.def.Auth.OAuth
	scopes := make([]string, len(oauthDef.Scopes))
	copy(scopes, oauthDef.Scopes)

	for _, cs := range oauthDef.ConditionalScopes {
		envVal := os.Getenv(cs.EnvGate)
		include := cs.Default
		if envVal != "" {
			include = !strings.EqualFold(envVal, "false")
		}
		if include {
			scopes = append(scopes, cs.Scope)
		}
	}

	var endpoint oauth2.Endpoint
	switch oauthDef.Endpoint {
	case "google":
		endpoint = google.Endpoint
	default:
		// Future: support custom endpoint URLs.
	}

	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Scopes:       scopes,
		Endpoint:     endpoint,
	}
}

func (a *YAMLAdapter) CredentialFromToken(token *oauth2.Token) ([]byte, error) {
	if a.def.Auth.Type != "oauth2" {
		return nil, fmt.Errorf("%s: OAuth token exchange not supported — use API key activation", a.def.Service.ID)
	}
	// For OAuth adapters, delegate to the credential package (set up by the loader).
	return nil, fmt.Errorf("%s: CredentialFromToken must be handled by OAuth credential manager", a.def.Service.ID)
}

func (a *YAMLAdapter) ValidateCredential(credBytes []byte) error {
	if a.def.Auth.Type == "none" {
		return nil
	}
	if credBytes == nil {
		return fmt.Errorf("%s: credential required", a.def.Service.ID)
	}
	var cred credential
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return fmt.Errorf("%s: invalid credential: %w", a.def.Service.ID, err)
	}
	if cred.Token == "" {
		return fmt.Errorf("%s: credential missing token", a.def.Service.ID)
	}

	// Additional validation for basic auth credentials.
	if a.def.Auth.Type == "basic" {
		parts := strings.SplitN(cred.Token, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("%s: credential must be in format 'user:pass'", a.def.Service.ID)
		}
	}

	return nil
}

func (a *YAMLAdapter) RequiredScopes() []string {
	if a.def.Auth.Type != "oauth2" || a.def.Auth.OAuth == nil {
		return nil
	}
	// Return scopes from the definition directly — these are known regardless
	// of whether OAuth app credentials are configured in the vault yet.
	return a.def.Auth.OAuth.Scopes
}

// ── MetadataProvider implementation ─────────────────────────────────────────

// ServiceMetadata returns the full display and risk metadata from the YAML definition.
func (a *YAMLAdapter) ServiceMetadata() adapters.ServiceMetadata {
	actionMeta := make(map[string]adapters.ActionMeta, len(a.def.Actions))
	for name, action := range a.def.Actions {
		actionMeta[name] = adapters.ActionMeta{
			DisplayName: action.DisplayName,
			Category:    action.Risk.Category,
			Sensitivity: action.Risk.Sensitivity,
			Description: action.Risk.Description,
		}
	}

	return adapters.ServiceMetadata{
		DisplayName:       a.def.Service.DisplayName,
		Description:       a.def.Service.Description,
		SetupURL:          a.def.Service.SetupURL,
		ActionMeta:        actionMeta,
		VerificationHints: a.def.VerificationHints,
	}
}

// ── VerificationHinter implementation ───────────────────────────────────────

// VerificationHints returns the verification hints if defined.
func (a *YAMLAdapter) VerificationHints() string {
	return a.def.VerificationHints
}

// SetOAuthProvider sets the provider that supplies OAuth app credentials lazily.
func (a *YAMLAdapter) SetOAuthProvider(p adapters.OAuthCredentialProvider) {
	a.oauthProvider = p
}

// Def returns the underlying YAML definition (for the loader/tests).
func (a *YAMLAdapter) Def() yamldef.ServiceDef {
	return a.def
}
