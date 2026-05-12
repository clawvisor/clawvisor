package adapters

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/vault"
)

// googleOAuthCred is the JSON structure stored in the vault for Google OAuth app credentials.
type googleOAuthCred struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// VaultOAuthProvider reads OAuth app credentials from the vault under the
// system user. Falls back to env vars (GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET)
// for backward compatibility with Docker Compose deployments.
type VaultOAuthProvider struct {
	vault vault.Vault
}

// NewVaultOAuthProvider creates a provider that reads Google OAuth creds from the vault.
func NewVaultOAuthProvider(v vault.Vault) *VaultOAuthProvider {
	return &VaultOAuthProvider{vault: v}
}

func (p *VaultOAuthProvider) OAuthClientCredentials() (clientID, clientSecret string) {
	// Check env vars first (backward compat for Docker/CI).
	if id := os.Getenv("GOOGLE_CLIENT_ID"); id != "" {
		return id, os.Getenv("GOOGLE_CLIENT_SECRET")
	}

	// Read from vault.
	data, err := p.vault.Get(context.Background(), SystemUserID, SystemVaultKeyGoogleOAuth)
	if err != nil || len(data) == 0 {
		return "", ""
	}

	var cred googleOAuthCred
	if err := json.Unmarshal(data, &cred); err != nil {
		return "", ""
	}

	return cred.ClientID, cred.ClientSecret
}

// SetGoogleOAuthCredentials stores Google OAuth app credentials in the system vault.
func SetGoogleOAuthCredentials(ctx context.Context, v vault.Vault, clientID, clientSecret string) error {
	data, err := json.Marshal(googleOAuthCred{
		ClientID:     clientID,
		ClientSecret: clientSecret,
	})
	if err != nil {
		return err
	}
	return v.Set(ctx, SystemUserID, SystemVaultKeyGoogleOAuth, data)
}

// GetGoogleOAuthCredentials reads Google OAuth app credentials from the system vault.
// Returns empty strings if not configured.
func GetGoogleOAuthCredentials(ctx context.Context, v vault.Vault) (clientID, clientSecret string) {
	data, err := v.Get(ctx, SystemUserID, SystemVaultKeyGoogleOAuth)
	if err != nil || len(data) == 0 {
		return "", ""
	}
	var cred googleOAuthCred
	if err := json.Unmarshal(data, &cred); err != nil {
		return "", ""
	}
	return cred.ClientID, cred.ClientSecret
}

// ── Microsoft OAuth provider ────────────────────────────────────────────────

// microsoftOAuthCred is the JSON structure stored in the vault for Microsoft OAuth app credentials.
type microsoftOAuthCred struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// MicrosoftVaultOAuthProvider reads Microsoft OAuth app credentials from the
// vault under the system user. Falls back to env vars (MICROSOFT_CLIENT_ID,
// MICROSOFT_CLIENT_SECRET) for backward compatibility with Docker Compose
// deployments.
type MicrosoftVaultOAuthProvider struct {
	vault vault.Vault
}

// NewMicrosoftVaultOAuthProvider creates a provider that reads Microsoft OAuth
// creds from the vault.
func NewMicrosoftVaultOAuthProvider(v vault.Vault) *MicrosoftVaultOAuthProvider {
	return &MicrosoftVaultOAuthProvider{vault: v}
}

func (p *MicrosoftVaultOAuthProvider) OAuthClientCredentials() (clientID, clientSecret string) {
	// Check env vars first (backward compat for Docker/CI).
	if id := os.Getenv("MICROSOFT_CLIENT_ID"); id != "" {
		return id, os.Getenv("MICROSOFT_CLIENT_SECRET")
	}

	// Read from vault.
	data, err := p.vault.Get(context.Background(), SystemUserID, SystemVaultKeyMicrosoftOAuth)
	if err != nil || len(data) == 0 {
		return "", ""
	}

	var cred microsoftOAuthCred
	if err := json.Unmarshal(data, &cred); err != nil {
		return "", ""
	}

	return cred.ClientID, cred.ClientSecret
}

// SetMicrosoftOAuthCredentials stores Microsoft OAuth app credentials in the
// system vault.
func SetMicrosoftOAuthCredentials(ctx context.Context, v vault.Vault, clientID, clientSecret string) error {
	data, err := json.Marshal(microsoftOAuthCred{
		ClientID:     clientID,
		ClientSecret: clientSecret,
	})
	if err != nil {
		return err
	}
	return v.Set(ctx, SystemUserID, SystemVaultKeyMicrosoftOAuth, data)
}

// GetMicrosoftOAuthCredentials reads Microsoft OAuth app credentials from the
// system vault. Returns empty strings if not configured.
func GetMicrosoftOAuthCredentials(ctx context.Context, v vault.Vault) (clientID, clientSecret string) {
	data, err := v.Get(ctx, SystemUserID, SystemVaultKeyMicrosoftOAuth)
	if err != nil || len(data) == 0 {
		return "", ""
	}
	var cred microsoftOAuthCred
	if err := json.Unmarshal(data, &cred); err != nil {
		return "", ""
	}
	return cred.ClientID, cred.ClientSecret
}

// ── PKCE client ID management ───────────────────────────────────────────────

// pkceClientIDCred is the JSON structure stored in the vault for per-service PKCE client IDs.
type pkceClientIDCred struct {
	ClientID string `json:"client_id"`
}

// SetPKCEClientID stores a PKCE client ID for a specific service in the system vault.
func SetPKCEClientID(ctx context.Context, v vault.Vault, serviceID, clientID string) error {
	data, err := json.Marshal(pkceClientIDCred{ClientID: clientID})
	if err != nil {
		return err
	}
	return v.Set(ctx, SystemUserID, SystemVaultKeyPKCEPrefix+serviceID, data)
}

// GetPKCEClientID reads a PKCE client ID for a specific service from the system vault.
// Returns empty string if not configured.
func GetPKCEClientID(ctx context.Context, v vault.Vault, serviceID string) string {
	data, err := v.Get(ctx, SystemUserID, SystemVaultKeyPKCEPrefix+serviceID)
	if err != nil || len(data) == 0 {
		return ""
	}
	var cred pkceClientIDCred
	if err := json.Unmarshal(data, &cred); err != nil {
		return ""
	}
	return cred.ClientID
}

// DeletePKCEClientID removes a PKCE client ID for a specific service from the system vault.
func DeletePKCEClientID(ctx context.Context, v vault.Vault, serviceID string) error {
	return v.Delete(ctx, SystemUserID, SystemVaultKeyPKCEPrefix+serviceID)
}

// ListPKCEClientIDs returns a map of serviceID → clientID for all configured PKCE credentials.
func ListPKCEClientIDs(ctx context.Context, v vault.Vault) (map[string]string, error) {
	keys, err := v.List(ctx, SystemUserID)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string)
	for _, key := range keys {
		if !strings.HasPrefix(key, SystemVaultKeyPKCEPrefix) {
			continue
		}
		serviceID := strings.TrimPrefix(key, SystemVaultKeyPKCEPrefix)
		if cid := GetPKCEClientID(ctx, v, serviceID); cid != "" {
			result[serviceID] = cid
		}
	}
	return result, nil
}

// ── MCP OAuth credential management ─────────────────────────────────────────
//
// Per-MCP-service OAuth client credentials, stored as system vault entries
// keyed by "mcp.oauth.{serviceID}". Same shape as Google/Microsoft — admins
// drop their client_id + client_secret in via the settings page and the
// MCPAdapter picks them up on the next request (no restart).

// mcpOAuthCred is the JSON structure stored for each MCP service's OAuth credentials.
type mcpOAuthCred struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// SetMCPOAuthCredentials stores OAuth app credentials for an MCP service in the system vault.
func SetMCPOAuthCredentials(ctx context.Context, v vault.Vault, serviceID, clientID, clientSecret string) error {
	data, err := json.Marshal(mcpOAuthCred{ClientID: clientID, ClientSecret: clientSecret})
	if err != nil {
		return err
	}
	return v.Set(ctx, SystemUserID, SystemVaultKeyMCPOAuthPrefix+serviceID, data)
}

// GetMCPOAuthCredentials reads OAuth app credentials for an MCP service from the
// system vault. Returns empty strings if not configured.
func GetMCPOAuthCredentials(ctx context.Context, v vault.Vault, serviceID string) (clientID, clientSecret string) {
	data, err := v.Get(ctx, SystemUserID, SystemVaultKeyMCPOAuthPrefix+serviceID)
	if err != nil || len(data) == 0 {
		return "", ""
	}
	var cred mcpOAuthCred
	if err := json.Unmarshal(data, &cred); err != nil {
		return "", ""
	}
	return cred.ClientID, cred.ClientSecret
}

// DeleteMCPOAuthCredentials removes OAuth app credentials for an MCP service.
func DeleteMCPOAuthCredentials(ctx context.Context, v vault.Vault, serviceID string) error {
	return v.Delete(ctx, SystemUserID, SystemVaultKeyMCPOAuthPrefix+serviceID)
}

// MCPOAuthEntry is a single configured MCP OAuth credential — used by
// settings-page list endpoints. ClientSecret is intentionally omitted; the
// settings UI only needs to know which services are configured.
type MCPOAuthEntry struct {
	ServiceID string `json:"service_id"`
	ClientID  string `json:"client_id"`
}

// MCPClientRecord is the cached result of RFC 8414 discovery + RFC 7591
// dynamic client registration for one MCP service. Persisted as a single
// vault record so a server restart doesn't trigger re-discovery or
// re-registration on the next request.
type MCPClientRecord struct {
	ClientID             string `json:"client_id"`
	ClientSecret         string `json:"client_secret,omitempty"` // empty for public/PKCE clients
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint        string `json:"token_endpoint"`
	RegistrationEndpoint string `json:"registration_endpoint,omitempty"`
}

// SetMCPClientRecord stores the discovered + registered client record for an MCP service.
func SetMCPClientRecord(ctx context.Context, v vault.Vault, serviceID string, rec *MCPClientRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return v.Set(ctx, SystemUserID, SystemVaultKeyMCPClientPrefix+serviceID, data)
}

// GetMCPClientRecord reads the cached client record. Returns nil when no
// discovery has happened yet for this service.
func GetMCPClientRecord(ctx context.Context, v vault.Vault, serviceID string) *MCPClientRecord {
	data, err := v.Get(ctx, SystemUserID, SystemVaultKeyMCPClientPrefix+serviceID)
	if err != nil || len(data) == 0 {
		return nil
	}
	var rec MCPClientRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil
	}
	return &rec
}

// DeleteMCPClientRecord removes the cached record — useful when the admin
// wants to force re-registration (e.g. if Notion rotates their server config).
func DeleteMCPClientRecord(ctx context.Context, v vault.Vault, serviceID string) error {
	return v.Delete(ctx, SystemUserID, SystemVaultKeyMCPClientPrefix+serviceID)
}

// ListMCPOAuthCredentials returns the set of MCP services with configured
// OAuth client credentials. Secrets are never returned.
func ListMCPOAuthCredentials(ctx context.Context, v vault.Vault) ([]MCPOAuthEntry, error) {
	keys, err := v.List(ctx, SystemUserID)
	if err != nil {
		return nil, err
	}
	out := make([]MCPOAuthEntry, 0)
	for _, key := range keys {
		if !strings.HasPrefix(key, SystemVaultKeyMCPOAuthPrefix) {
			continue
		}
		serviceID := strings.TrimPrefix(key, SystemVaultKeyMCPOAuthPrefix)
		cid, _ := GetMCPOAuthCredentials(ctx, v, serviceID)
		if cid == "" {
			continue
		}
		out = append(out, MCPOAuthEntry{ServiceID: serviceID, ClientID: cid})
	}
	return out, nil
}
