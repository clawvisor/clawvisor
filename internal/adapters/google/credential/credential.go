// Package credential provides shared credential types and scope utilities
// for all Google adapters. Credentials are stored encrypted in the vault
// under the key "google" (shared across Gmail, Calendar, Drive, Contacts).
package credential

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"golang.org/x/oauth2"
)

// Stored is the JSON structure saved (encrypted) in the vault under key "google".
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
		return nil, fmt.Errorf("google credential: invalid JSON: %w", err)
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
		return fmt.Errorf("google credential: missing tokens")
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

// MergeScopes returns the sorted set union of existing and additional scopes.
func MergeScopes(existing, additional []string) []string {
	seen := make(map[string]bool, len(existing)+len(additional))
	for _, s := range existing {
		seen[s] = true
	}
	for _, s := range additional {
		seen[s] = true
	}
	merged := make([]string, 0, len(seen))
	for s := range seen {
		merged = append(merged, s)
	}
	sort.Strings(merged)
	return merged
}

// FetchGoogleEmail calls the Google userinfo endpoint and returns the
// authenticated user's email address. This is used to auto-detect the
// account identity when connecting a Google service.
func FetchGoogleEmail(ctx context.Context, client *http.Client) (string, error) {
	resp, err := client.Get("https://www.googleapis.com/oauth2/v2/userinfo")
	if err != nil {
		return "", fmt.Errorf("google userinfo request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("google userinfo: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("google userinfo: read body: %w", err)
	}

	var info struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("google userinfo: parse: %w", err)
	}
	return info.Email, nil
}

// HasAllScopes returns true if existing contains all of the required scopes.
func HasAllScopes(existing, required []string) bool {
	set := make(map[string]bool, len(existing))
	for _, s := range existing {
		set[s] = true
	}
	for _, s := range required {
		if !set[s] {
			return false
		}
	}
	return true
}
