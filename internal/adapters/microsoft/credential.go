// Package microsoft provides shared credential types and HTTP helpers
// for all Microsoft Graph API adapters (Outlook, OneDrive, Teams).
//
// Credentials are stored encrypted in the vault under the key "microsoft"
// (shared across all microsoft.* services), mirroring how Google services
// share the "google" vault key.
package microsoft

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/oauth2"

	"github.com/clawvisor/clawvisor/internal/adapters/format"
	"github.com/clawvisor/clawvisor/pkg/adapters"
)

// Stored is the JSON structure saved (encrypted) in the vault under key "microsoft".
type Stored struct {
	Type         string    `json:"type"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
	Scopes       []string  `json:"scopes"`
}

// Parse unmarshals vault credential bytes into a Stored credential.
func Parse(data []byte) (*Stored, error) {
	var c Stored
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("microsoft credential: invalid JSON: %w", err)
	}
	return &c, nil
}

// Validate checks whether stored credential bytes are parseable and contain
// at least one token (access or refresh).
func Validate(data []byte) error {
	c, err := Parse(data)
	if err != nil {
		return err
	}
	if c.RefreshToken == "" && c.AccessToken == "" {
		return fmt.Errorf("microsoft credential: missing tokens")
	}
	return nil
}

// FromToken builds storable vault bytes from an OAuth2 token and scope list.
func FromToken(token *oauth2.Token, scopes []string) ([]byte, error) {
	c := Stored{
		Type:         "oauth2",
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		Expiry:       token.Expiry,
		Scopes:       scopes,
	}
	return json.Marshal(c)
}

// ToOAuth2Token converts the stored credential to an oauth2.Token.
func (c *Stored) ToOAuth2Token() *oauth2.Token {
	return &oauth2.Token{
		AccessToken:  c.AccessToken,
		RefreshToken: c.RefreshToken,
		Expiry:       c.Expiry,
		TokenType:    "Bearer",
	}
}

// MicrosoftEndpoint is the OAuth2 endpoint for Microsoft identity platform (v2.0).
// Uses the "common" tenant to support both personal and organizational accounts.
var MicrosoftEndpoint = oauth2.Endpoint{
	AuthURL:  "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
	TokenURL: "https://login.microsoftonline.com/common/oauth2/v2.0/token",
}

// HTTPClient builds an *http.Client with automatic token refresh using the
// Microsoft OAuth2 endpoint. This is the primary way Go overrides should
// obtain an authenticated client.
func HTTPClient(ctx context.Context, credBytes []byte, provider adapters.OAuthCredentialProvider) (*http.Client, error) {
	if err := Validate(credBytes); err != nil {
		return nil, fmt.Errorf("microsoft: %w", err)
	}

	cred, err := Parse(credBytes)
	if err != nil {
		return nil, fmt.Errorf("microsoft: %w", err)
	}

	clientID, clientSecret := provider.OAuthClientCredentials()
	if clientID == "" {
		return nil, fmt.Errorf("microsoft: OAuth not configured — missing app credentials")
	}

	cfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     MicrosoftEndpoint,
		Scopes:       cred.Scopes,
	}

	ts := cfg.TokenSource(ctx, cred.ToOAuth2Token())
	return oauth2.NewClient(ctx, ts), nil
}

// FetchMicrosoftEmail calls the Microsoft Graph /me endpoint and returns the
// authenticated user's email address. Used to auto-detect account identity.
func FetchMicrosoftEmail(ctx context.Context, client *http.Client) (string, error) {
	resp, err := client.Get("https://graph.microsoft.com/v1.0/me?$select=mail,userPrincipalName")
	if err != nil {
		return "", fmt.Errorf("microsoft userinfo request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("microsoft userinfo: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("microsoft userinfo: read body: %w", err)
	}

	var info struct {
		Mail               string `json:"mail"`
		UserPrincipalName  string `json:"userPrincipalName"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("microsoft userinfo: parse: %w", err)
	}
	email := info.Mail
	if email == "" {
		email = info.UserPrincipalName
	}
	return email, nil
}

// GraphGET makes an authenticated GET request to the Microsoft Graph API.
// The client should be an OAuth2 auto-refreshing client obtained from HTTPClient.
func GraphGET(ctx context.Context, client *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("graph API GET read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("graph API GET %s: %d: %s", url, resp.StatusCode, format.Truncate(string(body), 200))
	}

	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

// GraphPOST makes an authenticated POST request to the Microsoft Graph API.
func GraphPOST(ctx context.Context, client *http.Client, url string, payload any, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("graph API POST read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("graph API POST %s: %d: %s", url, resp.StatusCode, format.Truncate(string(body), 200))
	}

	if out != nil && len(body) > 0 {
		return json.Unmarshal(body, out)
	}
	return nil
}
