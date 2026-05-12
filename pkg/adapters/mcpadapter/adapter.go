// Package mcpadapter implements adapters.Adapter by proxying to a downstream
// MCP server. One generic adapter handles every spec-declared MCP service —
// vendor-specific behavior lives in YAML, not Go.
//
// Architectural notes:
//   - Tool discovery is dynamic via tools/list, populating SupportedActions().
//   - Risk classification (read-only / destructive / idempotent) comes from
//     the MCP tool's standard `annotations` field — no per-adapter YAML risk.
//   - Response sanitization, identity unification, and approval rendering all
//     live ABOVE this adapter, in gateway middleware. This adapter is a
//     dumb proxy by design.
package mcpadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/mcpclient"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// Config configures an MCPAdapter.
type Config struct {
	ServiceID string    // e.g. "notion"
	Transport Transport // how to open a connection to the downstream MCP server
	// EnvForCredential maps the credential token into env vars passed to the
	// MCP server subprocess. Most published MCP servers read auth from env
	// (NOTION_TOKEN, GITHUB_TOKEN, etc.).
	EnvForCredential func(cred []byte) (map[string]string, error)
	// Spec is the parsed config-driven definition for this adapter, when one
	// exists. Used for service metadata (display name, icon, setup URL) and
	// the whoami identity hook. May be nil for adapters built directly with New.
	Spec *Spec
}

// MCPAdapter implements adapters.Adapter via an MCP client.
//
// Tool discovery happens at service-activation time (the user has just
// supplied a credential) — not lazily, not at catalog-fetch time. The
// activation handler calls DiscoverTools and then registers a per-user
// clone (via Registry.RegisterForUser) carrying the discovered tool set.
// Catalog reads that per-user adapter and shows the user's specific tools.
type MCPAdapter struct {
	cfg                 Config
	tools               []mcpclient.Tool // populated for per-user clones; empty on the global instance
	oauthVault          vault.Vault      // optional; used to read system OAuth client credentials
	discoveryHTTPClient *http.Client     // optional; tests override to point at httptest.Server
	registerMu          *sync.Mutex      // serializes EnsureOAuthReady so concurrent first-time activations don't double-register; pointer so per-user clones share the lock with the global instance
}

func New(cfg Config) *MCPAdapter {
	return &MCPAdapter{cfg: cfg, registerMu: &sync.Mutex{}}
}

func (a *MCPAdapter) ServiceID() string { return a.cfg.ServiceID }

// SupportedActions returns the tool names this adapter exposes. For the
// global registry instance this is empty (no user, no credential, no
// discovery). Per-user clones populated at activation return their
// discovered tool set.
func (a *MCPAdapter) SupportedActions() []string {
	out := make([]string, 0, len(a.tools))
	for _, t := range a.tools {
		out = append(out, t.Name)
	}
	return out
}

// Tools returns the cached tool list (populated at activation time).
func (a *MCPAdapter) Tools() []mcpclient.Tool { return a.tools }

// openCaller centralizes the per-request transport setup so OAuth-MCP and
// bearer-MCP share one code path. For OAuth, it overlays a per-request
// HTTPTransport whose http.Client has an oauth2.TokenSource baked in — so
// token refresh happens transparently for every call. For bearer/stdio,
// it goes through the configured transport with the env map populated as
// usual.
func (a *MCPAdapter) openCaller(ctx context.Context, cred []byte) (mcpclient.Caller, error) {
	if a.oauthSpec() != nil {
		base, ok := a.cfg.Transport.(*HTTPTransport)
		if !ok {
			return nil, fmt.Errorf("%s: oauth requires http transport", a.cfg.ServiceID)
		}
		client, err := a.httpClientFor(ctx, cred)
		if err != nil {
			return nil, err
		}
		// Shallow copy so the per-request http.Client doesn't leak across users.
		tr := *base
		tr.HTTPClient = client
		// Skip the bearer-header path; the oauth2 client injects Authorization itself.
		return tr.Open(ctx, map[string]string{})
	}
	env, err := a.cfg.EnvForCredential(cred)
	if err != nil {
		return nil, fmt.Errorf("%s: credential: %w", a.cfg.ServiceID, err)
	}
	return a.cfg.Transport.Open(ctx, env)
}

// DiscoverTools opens a fresh transport with the user's credential, calls
// MCP tools/list, and returns the tool set. Called by the activation
// handler after the credential has been validated and stored.
func (a *MCPAdapter) DiscoverTools(ctx context.Context, cred []byte) ([]mcpclient.Tool, error) {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, err := a.openCaller(dctx, cred)
	if err != nil {
		return nil, fmt.Errorf("%s: open transport: %w", a.cfg.ServiceID, err)
	}
	defer client.Close()
	if err := client.Initialize(dctx); err != nil {
		return nil, fmt.Errorf("%s: initialize: %w", a.cfg.ServiceID, err)
	}
	return client.ListTools(dctx)
}

// ForUser returns a shallow clone of this adapter with a fixed tool set.
// Activation hands this clone to Registry.RegisterForUser so subsequent
// per-user lookups see the user's discovered tools.
func (a *MCPAdapter) ForUser(tools []mcpclient.Tool) *MCPAdapter {
	clone := *a
	clone.tools = tools
	return &clone
}

func (a *MCPAdapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	client, err := a.openCaller(ctx, req.Credential)
	if err != nil {
		return nil, fmt.Errorf("%s: open transport: %w", a.cfg.ServiceID, err)
	}
	defer client.Close()

	if err := client.Initialize(ctx); err != nil {
		return nil, fmt.Errorf("%s: initialize: %w", a.cfg.ServiceID, err)
	}

	tr, err := client.CallTool(ctx, req.Action, req.Params)
	if err != nil {
		return nil, fmt.Errorf("%s: call %s: %w", a.cfg.ServiceID, req.Action, err)
	}
	if tr.IsError {
		msg := "tool returned error"
		if len(tr.Content) > 0 {
			msg = tr.Content[0].Text
		}
		return nil, fmt.Errorf("%s: %s: %s", a.cfg.ServiceID, req.Action, msg)
	}

	// MCP tool responses are content-typed (text/image/resource). We expect
	// text content holding JSON. Parse it; if parse fails, surface the raw
	// text. The gateway middleware handles sanitization above us.
	var data any
	var summary string
	if len(tr.Content) > 0 && tr.Content[0].Type == "text" {
		text := tr.Content[0].Text
		if err := json.Unmarshal([]byte(text), &data); err != nil {
			data = text
		}
		summary = a.summaryFor(req.Action, data)
	}

	return &adapters.Result{
		Summary: summary,
		Data:    data,
		Meta: map[string]any{
			"source": "mcp",
			"server": a.cfg.ServiceID,
		},
	}, nil
}

// summaryFor synthesizes a one-line summary from the tool result. A future
// pass moves this into gateway middleware (and uses an LLM or the tool
// description); inlined here for now.
func (a *MCPAdapter) summaryFor(action string, data any) string {
	switch v := data.(type) {
	case []any:
		return fmt.Sprintf("%s: %d result(s)", action, len(v))
	case map[string]any:
		if results, ok := v["results"].([]any); ok {
			return fmt.Sprintf("%s: %d result(s)", action, len(results))
		}
		if id, ok := v["id"].(string); ok {
			return fmt.Sprintf("%s: %s", action, id)
		}
	}
	return action
}

// ── Adapter interface ────────────────────────────────────────────────────────
// MCPAdapter supports both API-key (Bearer header) and OAuth credential models,
// selected by the spec. The existing activation handler in services.go drives
// the OAuth dance when OAuthConfig() returns non-nil — the same path Gmail,
// Drive, Slack, and Linear use.

func (a *MCPAdapter) OAuthConfig() *oauth2.Config { return a.oauthConfig() }

func (a *MCPAdapter) CredentialFromToken(t *oauth2.Token) ([]byte, error) {
	if a.oauthSpec() == nil {
		return nil, fmt.Errorf("%s: not an OAuth adapter", a.cfg.ServiceID)
	}
	if t == nil {
		return nil, fmt.Errorf("%s: nil token", a.cfg.ServiceID)
	}
	payload := map[string]any{
		"access_token":  t.AccessToken,
		"refresh_token": t.RefreshToken,
	}
	if !t.Expiry.IsZero() {
		payload["expiry"] = t.Expiry.Format(time.RFC3339)
	}
	return json.Marshal(payload)
}

func (a *MCPAdapter) ValidateCredential(credBytes []byte) error {
	if len(credBytes) == 0 {
		return fmt.Errorf("%s: credential required", a.cfg.ServiceID)
	}
	if a.oauthSpec() != nil {
		var oc struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.Unmarshal(credBytes, &oc); err != nil {
			return fmt.Errorf("%s: invalid oauth credential: %w", a.cfg.ServiceID, err)
		}
		if oc.AccessToken == "" && oc.RefreshToken == "" {
			return fmt.Errorf("%s: oauth credential missing both access_token and refresh_token", a.cfg.ServiceID)
		}
		return nil
	}
	var cred struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return fmt.Errorf("%s: invalid credential: %w", a.cfg.ServiceID, err)
	}
	if cred.Token == "" {
		return fmt.Errorf("%s: credential missing token", a.cfg.ServiceID)
	}
	return nil
}

func (a *MCPAdapter) RequiredScopes() []string {
	spec := a.oauthSpec()
	if spec == nil {
		return nil
	}
	// Return a non-nil slice even when no scopes are declared — the catalog
	// handler treats nil as "not an OAuth service", so OAuth-MCP with an
	// empty scope list would otherwise be misclassified as API-key.
	if spec.Scopes == nil {
		return []string{}
	}
	return spec.Scopes
}

// ── MetadataProvider ─────────────────────────────────────────────────────────
// Display fields come from the spec; action metadata is built from the
// discovered tool list (name, annotations → risk classification, inputSchema
// → param signature). Future work: pull more from MCP server `serverInfo`
// and `instructions` responses.

func (a *MCPAdapter) ServiceMetadata() adapters.ServiceMetadata {
	actionMeta := make(map[string]adapters.ActionMeta)
	for _, t := range a.Tools() {
		// Default to write/medium per the MCP spec: when annotations are
		// absent, readOnlyHint defaults to false and destructiveHint
		// defaults to true. An unannotated tool must therefore be treated
		// as writable and destructive so it doesn't accidentally bypass
		// approval/scope gates. readOnlyHint=true is the only way to
		// downgrade to read/low; destructiveHint=false (with readOnly not
		// set) downgrades from high to medium.
		category := "write"
		sensitivity := "high"
		if dh, ok := t.Annotations["destructiveHint"].(bool); ok && !dh {
			sensitivity = "medium"
		}
		if ro, _ := t.Annotations["readOnlyHint"].(bool); ro {
			// Read-only wins over any destructive hint — a server that
			// sets both is misconfigured, but read-only is a stronger
			// safety claim than destructive (which is the unknown default).
			category = "read"
			sensitivity = "low"
		}
		actionMeta[t.Name] = adapters.ActionMeta{
			DisplayName: t.Name,
			Category:    category,
			Sensitivity: sensitivity,
			Description: t.Description, // raw — catalog renderer applies OneLineSummary
			Params:      SchemaParams(t.InputSchema),
		}
	}

	displayName := a.cfg.ServiceID
	description := "MCP-backed adapter"
	// KeyHint only applies to API-key adapters. For OAuth-MCP it stays
	// empty so the modal doesn't render a misleading paste field.
	keyHint := ""
	if a.oauthSpec() == nil {
		keyHint = "API token for the downstream service"
	}
	setupURL := ""
	iconURL := ""
	iconSVG := ""
	autoIdentity := false
	oauthEndpoint := ""
	if a.oauthSpec() != nil {
		// Signals to the catalog UI that this service activates via OAuth
		// rather than an API-key paste. The discriminator string is
		// per-service (mcp:notion, mcp:supabase, etc.) so the frontend can route to
		// the right authorize URL.
		oauthEndpoint = "mcp:" + a.cfg.ServiceID
	}
	if a.cfg.Spec != nil {
		if a.cfg.Spec.Service.DisplayName != "" {
			displayName = a.cfg.Spec.Service.DisplayName
		}
		if a.cfg.Spec.Service.Description != "" {
			description = a.cfg.Spec.Service.Description
		}
		if a.cfg.Spec.Service.KeyHint != "" {
			keyHint = a.cfg.Spec.Service.KeyHint
		}
		setupURL = a.cfg.Spec.Service.SetupURL
		iconURL = a.cfg.Spec.Service.IconURL
		iconSVG = a.cfg.Spec.Service.IconSVG
		autoIdentity = a.cfg.Spec.MCP.Whoami != nil && a.cfg.Spec.MCP.Whoami.Tool != ""
	}

	return adapters.ServiceMetadata{
		DisplayName:   displayName,
		Description:   description,
		KeyHint:       keyHint,
		SetupURL:      setupURL,
		IconURL:       iconURL,
		IconSVG:       iconSVG,
		OAuthEndpoint: oauthEndpoint,
		AutoIdentity:  autoIdentity,
		ActionMeta:    actionMeta,
	}
}
