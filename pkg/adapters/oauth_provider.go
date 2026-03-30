package adapters

import (
	"context"
	"encoding/json"
	"os"

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
	vault       vault.Vault
	redirectURL string // derived from server host:port, not a secret
}

// NewVaultOAuthProvider creates a provider that reads Google OAuth creds from the vault.
func NewVaultOAuthProvider(v vault.Vault, redirectURL string) *VaultOAuthProvider {
	return &VaultOAuthProvider{vault: v, redirectURL: redirectURL}
}

func (p *VaultOAuthProvider) OAuthClientCredentials() (clientID, clientSecret, redirectURL string) {
	// Check env vars first (backward compat for Docker/CI).
	if id := os.Getenv("GOOGLE_CLIENT_ID"); id != "" {
		return id, os.Getenv("GOOGLE_CLIENT_SECRET"), p.redirectURL
	}

	// Read from vault.
	data, err := p.vault.Get(context.Background(), SystemUserID, SystemVaultKeyGoogleOAuth)
	if err != nil || len(data) == 0 {
		return "", "", ""
	}

	var cred googleOAuthCred
	if err := json.Unmarshal(data, &cred); err != nil {
		return "", "", ""
	}

	return cred.ClientID, cred.ClientSecret, p.redirectURL
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
