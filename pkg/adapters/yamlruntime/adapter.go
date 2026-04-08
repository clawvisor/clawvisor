// Package yamlruntime implements the adapters.Adapter interface for YAML-defined services.
package yamlruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

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
	compiled      map[string]*compiledAction          // action_name → compiled exprs (nil if none)
}

// New creates a YAMLAdapter from a parsed service definition.
// overrides maps action names to Go functions for actions too complex for YAML.
// Returns an error if any expr expression fails to compile.
func New(def yamldef.ServiceDef, overrides map[string]ActionFunc) (*YAMLAdapter, error) {
	if overrides == nil {
		overrides = map[string]ActionFunc{}
	}
	compiled := make(map[string]*compiledAction, len(def.Actions))
	for name, action := range def.Actions {
		if action.Override == "go" {
			continue
		}
		ca, err := compileAction(action)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", def.Service.ID, name, err)
		}
		if ca != nil {
			compiled[name] = ca
		}
	}
	return &YAMLAdapter{def: def, overrides: overrides, compiled: compiled}, nil
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
	client, err := a.buildAuthClient(ctx, req.Credential)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", a.def.Service.ID, err)
	}

	// Get credential fields for path interpolation.
	credFields, _ := credentialFields(a.def.Auth, req.Credential)

	switch a.def.API.Type {
	case "rest":
		return executeREST(ctx, client, a.def.API.BaseURL, action, req.Params, credFields, a.compiled[req.Action])
	case "graphql":
		return executeGraphQL(ctx, client, a.def.API.BaseURL, action, req.Params, a.def.Service.ID)
	default:
		return nil, fmt.Errorf("%s: unsupported API type %q", a.def.Service.ID, a.def.API.Type)
	}
}

// FetchIdentity makes a request to the configured identity endpoint and
// extracts the account identifier from the JSON response.
func (a *YAMLAdapter) FetchIdentity(ctx context.Context, credBytes []byte) (string, error) {
	idDef := a.def.Service.Identity
	if idDef == nil {
		return "", nil
	}

	client, err := a.buildAuthClient(ctx, credBytes)
	if err != nil {
		return "", fmt.Errorf("%s: identity fetch: %w", a.def.Service.ID, err)
	}

	endpoint := idDef.Endpoint
	if !strings.HasPrefix(endpoint, "http") {
		endpoint = strings.TrimRight(a.def.API.BaseURL, "/") + "/" + strings.TrimLeft(endpoint, "/")
	}

	method := idDef.Method
	if method == "" {
		method = http.MethodGet
	}
	var bodyReader io.Reader
	if idDef.Body != "" {
		bodyReader = strings.NewReader(idDef.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
	if err != nil {
		return "", fmt.Errorf("%s: identity request: %w", a.def.Service.ID, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s: identity request: %w", a.def.Service.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: identity endpoint returned %d", a.def.Service.ID, resp.StatusCode)
	}

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return "", fmt.Errorf("%s: identity parse: %w", a.def.Service.ID, err)
	}

	// Walk the dot-delimited field path.
	parts := strings.Split(idDef.Field, ".")
	var current any = raw
	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return "", fmt.Errorf("%s: identity field %q not found", a.def.Service.ID, idDef.Field)
		}
		current, ok = m[part]
		if !ok {
			return "", fmt.Errorf("%s: identity field %q not found", a.def.Service.ID, idDef.Field)
		}
	}

	identity, ok := current.(string)
	if !ok {
		return "", fmt.Errorf("%s: identity field %q is not a string", a.def.Service.ID, idDef.Field)
	}
	return identity, nil
}

func (a *YAMLAdapter) OAuthConfig() *oauth2.Config {
	if a.def.Auth.Type != "oauth2" || a.def.Auth.OAuth == nil {
		return nil
	}

	oauthDef := a.def.Auth.OAuth

	// Try inline credentials first (custom OAuth endpoints), then provider (Google).
	clientID := oauthDef.ClientID
	clientSecret := oauthDef.ClientSecret
	if oauthDef.ClientIDEnv != "" {
		if v := os.Getenv(oauthDef.ClientIDEnv); v != "" {
			clientID = v
		}
	}
	if oauthDef.ClientSecretEnv != "" {
		if v := os.Getenv(oauthDef.ClientSecretEnv); v != "" {
			clientSecret = v
		}
	}
	if clientID == "" && a.oauthProvider != nil {
		clientID, clientSecret = a.oauthProvider.OAuthClientCredentials()
	}
	if clientID == "" {
		return nil // OAuth not yet configured
	}

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
		endpoint = oauth2.Endpoint{
			AuthURL:  oauthDef.AuthorizeURL,
			TokenURL: oauthDef.TokenURL,
		}
	}

	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
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

// buildAuthClient creates an *http.Client with proper authentication.
// For OAuth2 services, this uses the OAuthConfig token source which handles
// automatic token refresh. For other auth types, delegates to buildHTTPClient.
func (a *YAMLAdapter) buildAuthClient(ctx context.Context, credBytes []byte) (*http.Client, error) {
	if a.def.Auth.Type == "oauth2" {
		oauthCfg := a.OAuthConfig()
		if oauthCfg == nil {
			return nil, fmt.Errorf("OAuth not configured — missing app credentials")
		}
		var stored struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			Expiry       string `json:"expiry"`
		}
		if err := json.Unmarshal(credBytes, &stored); err != nil {
			return nil, fmt.Errorf("parsing oauth2 credential: %w", err)
		}
		token := &oauth2.Token{
			AccessToken:  stored.AccessToken,
			RefreshToken: stored.RefreshToken,
			TokenType:    "Bearer",
		}
		if stored.Expiry != "" {
			if t, err := time.Parse(time.RFC3339, stored.Expiry); err == nil {
				token.Expiry = t
			}
		}
		ts := oauthCfg.TokenSource(ctx, token)
		return oauth2.NewClient(ctx, ts), nil
	}
	return buildHTTPClient(a.def.Auth, credBytes)
}

// ── MetadataProvider implementation ─────────────────────────────────────────

// ServiceMetadata returns the full display and risk metadata from the YAML definition.
func (a *YAMLAdapter) ServiceMetadata() adapters.ServiceMetadata {
	actionMeta := make(map[string]adapters.ActionMeta, len(a.def.Actions))
	for name, action := range a.def.Actions {
		am := adapters.ActionMeta{
			DisplayName: action.DisplayName,
			Category:    action.Risk.Category,
			Sensitivity: action.Risk.Sensitivity,
			Description: action.Risk.Description,
		}
		// Build ordered parameter metadata from YAML params.
		if len(action.Params) > 0 {
			names := make([]string, 0, len(action.Params))
			for pn := range action.Params {
				names = append(names, pn)
			}
			sort.Strings(names)
			// Put required params first, then optional, preserving alpha within each group.
			sort.SliceStable(names, func(i, j int) bool {
				ri := action.Params[names[i]].Required
				rj := action.Params[names[j]].Required
				if ri != rj {
					return ri
				}
				return false
			})
			am.Params = make([]adapters.ParamMeta, 0, len(names))
			for _, pn := range names {
				p := action.Params[pn]
				am.Params = append(am.Params, adapters.ParamMeta{
					Name:     pn,
					Type:     p.Type,
					Required: p.Required,
					Default:  p.Default,
					Min:      p.Min,
					Max:      p.Max,
				})
			}
		}
		actionMeta[name] = am
	}

	var vaultKey, oauthEndpoint string
	if a.def.Auth.OAuth != nil {
		vaultKey = a.def.Auth.OAuth.VaultKey
		oauthEndpoint = a.def.Auth.OAuth.Endpoint
	}

	return adapters.ServiceMetadata{
		DisplayName:       a.def.Service.DisplayName,
		Description:       a.def.Service.Description,
		SetupURL:          a.def.Service.SetupURL,
		IconSVG:           a.def.Service.IconSVG,
		VaultKey:          vaultKey,
		OAuthEndpoint:     oauthEndpoint,
		DeviceFlow:        a.def.Auth.DeviceFlow != nil && a.resolveDeviceFlowClientID() != "",
		PKCEFlow:          a.def.Auth.PKCEFlow != nil && a.resolvePKCEFlowClientID() != "",
		PKCEFlowDefined:   a.def.Auth.PKCEFlow != nil,
		AutoIdentity:      a.def.Service.Identity != nil,
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

// DeviceFlowConfig returns the device flow configuration if available and
// a client_id can be resolved. Implements adapters.DeviceFlowProvider.
func (a *YAMLAdapter) DeviceFlowConfig() *yamldef.DeviceFlowDef {
	if a.def.Auth.DeviceFlow == nil {
		return nil
	}
	if a.resolveDeviceFlowClientID() == "" {
		return nil
	}
	return a.def.Auth.DeviceFlow
}

// resolveDeviceFlowClientID returns the client_id for device flow,
// checking env var first then the hardcoded value.
func (a *YAMLAdapter) resolveDeviceFlowClientID() string {
	df := a.def.Auth.DeviceFlow
	if df == nil {
		return ""
	}
	if df.ClientIDEnv != "" {
		if v := os.Getenv(df.ClientIDEnv); v != "" {
			return v
		}
	}
	return df.ClientID
}

// PKCEFlowConfig returns the PKCE flow configuration if available and
// a client_id can be resolved. Implements adapters.PKCEFlowProvider.
func (a *YAMLAdapter) PKCEFlowConfig() *yamldef.PKCEFlowDef {
	if a.def.Auth.PKCEFlow == nil {
		return nil
	}
	if a.resolvePKCEFlowClientID() == "" {
		return nil
	}
	return a.def.Auth.PKCEFlow
}

// resolvePKCEFlowClientID returns the client_id for PKCE flow,
// checking env var first then the hardcoded value.
func (a *YAMLAdapter) resolvePKCEFlowClientID() string {
	pf := a.def.Auth.PKCEFlow
	if pf == nil {
		return ""
	}
	if pf.ClientIDEnv != "" {
		if v := os.Getenv(pf.ClientIDEnv); v != "" {
			return v
		}
	}
	return pf.ClientID
}

// Def returns the underlying YAML definition (for the loader/tests).
func (a *YAMLAdapter) Def() yamldef.ServiceDef {
	return a.def
}
