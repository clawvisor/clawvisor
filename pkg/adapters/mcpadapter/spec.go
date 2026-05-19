package mcpadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Spec is the minimal configuration needed to expose an MCP server as a
// Clawvisor adapter. Everything else — actions, params, risk classification,
// response sanitization — is derived from the MCP protocol or applied
// generically by gateway middleware.
type Spec struct {
	Service struct {
		ID          string `yaml:"id"`
		DisplayName string `yaml:"display_name"`
		Description string `yaml:"description"`
		SetupURL    string `yaml:"setup_url"`
		KeyHint     string `yaml:"key_hint"`
		IconURL     string `yaml:"icon_url"`
		IconSVG     string `yaml:"icon_svg"`
	} `yaml:"service"`

	MCP struct {
		// Transport selects how Clawvisor reaches the MCP server.
		// "stdio" (default) spawns a subprocess locally; "http" speaks
		// MCP streamable HTTP against a remote vendor-hosted endpoint.
		Transport string `yaml:"transport,omitempty"`

		// ── stdio transport ────────────────────────────────────────────
		// Command is the argv to launch the MCP server over stdio.
		// e.g. ["npx", "-y", "@notionhq/notion-mcp-server"].
		Command []string `yaml:"command,omitempty"`
		// CredentialEnv is the env var the MCP server reads for auth.
		// The Clawvisor vault credential's `token` field is passed in here.
		// e.g. "NOTION_TOKEN", "GITHUB_TOKEN".
		CredentialEnv string `yaml:"credential_env,omitempty"`
		// ExtraEnv is additional static env passed through (rarely used).
		ExtraEnv map[string]string `yaml:"extra_env,omitempty"`

		// ── http transport ─────────────────────────────────────────────
		// Endpoint is the MCP streamable-HTTP URL, e.g. "https://mcp.notion.com".
		Endpoint string `yaml:"endpoint,omitempty"`
		// HeaderName is the HTTP header used for auth. Defaults to "Authorization".
		HeaderName string `yaml:"header_name,omitempty"`
		// HeaderPrefix is prepended to the credential token. Defaults to "Bearer ".
		HeaderPrefix string `yaml:"header_prefix,omitempty"`

		// OAuth, when present, switches activation to a browser-based OAuth
		// authorization code flow. Tokens are stored as the standard
		// {"access_token","refresh_token","expiry"} envelope and refreshed
		// transparently by golang.org/x/oauth2 at request time.
		OAuth *MCPOAuthSpec `yaml:"oauth,omitempty"`

		// Whoami declares which MCP tool returns identity, and which field
		// of the JSON response holds the human-readable identifier.
		// Optional — if absent, the adapter has no auto-identity.
		Whoami *WhoamiSpec `yaml:"whoami,omitempty"`
	} `yaml:"mcp"`
}

// MCPOAuthSpec describes the MCP server's OAuth 2.0 authorization endpoints
// and the scopes Clawvisor should request. client_id / client_secret are
// NOT in the spec — they're stored under "__system__"/"mcp.oauth.{serviceID}"
// in the vault, populated by the settings UI (self-hosted) or pre-seeded
// during deploy (cloud), exactly the same pattern as google.oauth.
type MCPOAuthSpec struct {
	AuthorizeURL string   `yaml:"authorize_url"`
	TokenURL     string   `yaml:"token_url"`
	Scopes       []string `yaml:"scopes,omitempty"`
}

// transport returns "stdio" if unset, lower-cased for comparison.
func (s *Spec) transport() string {
	t := strings.ToLower(strings.TrimSpace(s.MCP.Transport))
	if t == "" {
		return "stdio"
	}
	return t
}

// WhoamiSpec describes how to derive the user's identity from a tool call.
// This is the *only* per-service hook that the MCP-driven path needs:
// everything else (actions, params, response sanitization) is generic.
type WhoamiSpec struct {
	Tool string `yaml:"tool"` // tool name to call, e.g. "notion-get-users"
	// Params, optional, are passed as the tool's `arguments`. Some servers
	// require a parameter to select the current user (e.g. Notion takes
	// `{"user_id": "self"}` to scope notion-get-users to the authed user).
	Params map[string]any `yaml:"params,omitempty"`
	// Field is a dot-path into the JSON response. Supports `results[0].email`
	// style array indexing for endpoints that return a list (Notion does).
	Field string `yaml:"field"`
}

// LoadFromFS walks an embed.FS (or any fs.FS), parses every *.mcp.yaml file
// it finds, and returns ready-to-register MCPAdapters. Provides the inverted
// architecture's "minimum integration unit" — a spec file, no Go.
func LoadFromFS(filesystem fs.FS, root string) ([]*MCPAdapter, error) {
	var out []*MCPAdapter
	err := fs.WalkDir(filesystem, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".mcp.yaml") && !strings.HasSuffix(path, ".mcp.yml") {
			return nil
		}
		data, err := fs.ReadFile(filesystem, path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		var spec Spec
		if err := yaml.Unmarshal(data, &spec); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if spec.Service.ID == "" {
			return fmt.Errorf("%s: service.id is required", filepath.Base(path))
		}
		transport, err := transportFromSpec(spec)
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		out = append(out, FromSpec(spec, transport))
		return nil
	})
	// Preserve any partial results so callers can degrade gracefully —
	// a single malformed bundled .mcp.yaml shouldn't take down every
	// other MCP service. Callers that want strict semantics still
	// check err.
	if err != nil {
		return out, err
	}
	return out, nil
}

// FromSpec builds an MCPAdapter from a parsed Spec, given a Transport. The
// transport is separated so tests can swap in InProcessTransport without
// touching the spec.
func FromSpec(spec Spec, transport Transport) *MCPAdapter {
	envFn := func(cred []byte) (map[string]string, error) {
		env := map[string]string{}
		for k, v := range spec.MCP.ExtraEnv {
			env[k] = v
		}
		tok := tokenFromCredential(cred)
		if spec.MCP.CredentialEnv != "" && tok != "" {
			env[spec.MCP.CredentialEnv] = tok
		}
		// HTTPTransport reads the token from this slot to build the auth
		// header; stdio transports ignore it. Keeping a single env map
		// across transports keeps the activation hook transport-agnostic.
		if tok != "" {
			env[httpTokenEnvKey] = tok
		}
		return env, nil
	}
	return New(Config{
		ServiceID:        spec.Service.ID,
		Transport:        transport,
		EnvForCredential: envFn,
		Spec:             &spec,
	})
}

// transportFromSpec constructs the right Transport for the spec's declared
// transport type. Validation lives here so LoadFromFS surfaces config errors
// at startup rather than at first request.
func transportFromSpec(spec Spec) (Transport, error) {
	switch spec.transport() {
	case "stdio":
		if len(spec.MCP.Command) == 0 {
			return nil, fmt.Errorf("mcp.command is required for stdio transport")
		}
		return &StdioTransport{Command: spec.MCP.Command}, nil
	case "http":
		if spec.MCP.Endpoint == "" {
			return nil, fmt.Errorf("mcp.endpoint is required for http transport")
		}
		return &HTTPTransport{
			Endpoint:     spec.MCP.Endpoint,
			HeaderName:   spec.MCP.HeaderName,
			HeaderPrefix: spec.MCP.HeaderPrefix,
		}, nil
	default:
		return nil, fmt.Errorf("unknown mcp.transport %q (want stdio or http)", spec.MCP.Transport)
	}
}

// tokenFromCredential pulls the bearer-shaped token out of the standard
// Clawvisor credential JSON envelope ({"type":"api_key","token":"..."}).
// Returns the raw bytes if the envelope doesn't parse, so a plain-string
// credential still works.
func tokenFromCredential(cred []byte) string {
	if len(cred) == 0 {
		return ""
	}
	var env struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(cred, &env); err == nil {
		if env.Token != "" {
			return env.Token
		}
		if env.AccessToken != "" {
			return env.AccessToken
		}
	}
	return string(cred)
}

// fetchIdentityViaWhoami opens a fresh transport, calls the whoami tool from
// the spec, and extracts the configured field from the JSON response.
// Implements the "every server SHOULD expose whoami" convention discussed
// in MCP_INVERSION_FINDINGS.md.
func (a *MCPAdapter) fetchIdentityViaWhoami(ctx context.Context, cred []byte) (string, error) {
	if a.cfg.Spec == nil || a.cfg.Spec.MCP.Whoami == nil {
		return "", nil
	}
	w := a.cfg.Spec.MCP.Whoami
	if w.Tool == "" {
		return "", nil
	}

	client, err := a.openCaller(ctx, cred)
	if err != nil {
		return "", err
	}
	defer client.Close()

	if err := client.Initialize(ctx); err != nil {
		return "", err
	}
	tr, err := client.CallTool(ctx, w.Tool, w.Params)
	if err != nil {
		return "", err
	}
	if tr.IsError {
		if len(tr.Content) > 0 {
			return "", fmt.Errorf("whoami: %s", tr.Content[0].Text)
		}
		return "", fmt.Errorf("whoami: tool returned error")
	}
	if len(tr.Content) == 0 || tr.Content[0].Type != "text" {
		return "", nil
	}
	var data any
	if err := json.Unmarshal([]byte(tr.Content[0].Text), &data); err != nil {
		return "", nil // not JSON; no identity
	}
	return normalizeAlias(extractField(data, w.Field)), nil
}

// normalizeAlias coerces a free-form whoami result into the alias character
// set the rest of Clawvisor enforces: [a-z 0-9 _ - . @ +]. Whitespace
// becomes "-", letters are lowercased, anything else is dropped. Empty
// result triggers the standard "default" alias fallback upstream.
//
// Notion → "eric@clawvisor.com" passes through unchanged.
// Supabase → "Eric Levine's Org" → "eric-levines-org".
func normalizeAlias(raw string) string {
	if raw == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(raw))
	prevDash := false
	for _, r := range raw {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r - 'A' + 'a')
			prevDash = false
		case r == '_' || r == '-' || r == '.' || r == '@' || r == '+':
			b.WriteRune(r)
			prevDash = r == '-'
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if !prevDash {
				b.WriteRune('-')
				prevDash = true
			}
		default:
			// Drop any char outside the allowed set (apostrophes, slashes,
			// non-ASCII letters, etc.). Collapsing rather than rejecting
			// keeps the alias usable when the source has a few stray glyphs.
		}
	}
	return strings.Trim(b.String(), "-.")
}

// extractField walks a path through nested maps and arrays and returns the
// string value at the end, or "" if any segment is missing. Supports two
// forms of array indexing so YAML specs can target list-shaped responses:
//
//	"results[0].email"   ← JSONPath-ish, our preferred form
//	"results.0.email"    ← also accepted; numeric segments index arrays
func extractField(v any, path string) string {
	if path == "" {
		if s, ok := v.(string); ok {
			return s
		}
		return ""
	}
	// Normalize `foo[0].bar` → `foo.0.bar` so we can split on `.` uniformly.
	normalized := strings.ReplaceAll(path, "[", ".")
	normalized = strings.ReplaceAll(normalized, "]", "")
	parts := strings.Split(normalized, ".")
	cur := v
	for _, p := range parts {
		if p == "" {
			continue
		}
		switch x := cur.(type) {
		case map[string]any:
			cur = x[p]
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(x) {
				return ""
			}
			cur = x[i]
		default:
			return ""
		}
	}
	if s, ok := cur.(string); ok {
		return s
	}
	return ""
}

// FetchIdentity implements adapters.IdentityFetcher, using the spec's whoami
// hook. Returning ("", nil) is valid — means "no identity discoverable."
func (a *MCPAdapter) FetchIdentity(ctx context.Context, cred []byte, _ map[string]string) (string, error) {
	return a.fetchIdentityViaWhoami(ctx, cred)
}
