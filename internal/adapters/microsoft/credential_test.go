package microsoft

import (
	"context"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestParse_Valid(t *testing.T) {
	data := []byte(`{"type":"oauth2","access_token":"token123","refresh_token":"refresh123","expiry":"2025-01-01T00:00:00Z"}`)
	c, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() unexpected error: %v", err)
	}
	if c.AccessToken != "token123" {
		t.Errorf("got %q, want token123", c.AccessToken)
	}
	if c.RefreshToken != "refresh123" {
		t.Errorf("got %q, want refresh123", c.RefreshToken)
	}
}

func TestParse_InvalidJSON(t *testing.T) {
	data := []byte(`{invalid}`)
	_, err := Parse(data)
	if err == nil {
		t.Fatal("Parse() expected error, got nil")
	}
}

func TestValidate_Valid(t *testing.T) {
	data := []byte(`{"access_token":"token123"}`)
	if err := Validate(data); err != nil {
		t.Errorf("Validate() unexpected error: %v", err)
	}
}

func TestValidate_MissingTokens(t *testing.T) {
	data := []byte(`{"type":"oauth2"}`) // no tokens
	if err := Validate(data); err == nil {
		t.Errorf("Validate() expected error for missing tokens, got nil")
	}
}

func TestFromToken(t *testing.T) {
	token := &oauth2.Token{
		AccessToken:  "at",
		RefreshToken: "rt",
		Expiry:       time.Now(),
	}
	b, err := FromToken(token, []string{"scope1"})
	if err != nil {
		t.Fatalf("FromToken() unexpected error: %v", err)
	}

	c, err := Parse(b)
	if err != nil {
		t.Fatalf("Parse() failed to read FromToken bytes: %v", err)
	}
	if c.AccessToken != "at" {
		t.Errorf("got %q, want at", c.AccessToken)
	}
	if c.Scopes[0] != "scope1" {
		t.Errorf("got %q, want scope1", c.Scopes[0])
	}
}

type mockProvider struct{}

func (m mockProvider) OAuthClientCredentials() (string, string) {
	return "client_id", "client_secret"
}

func TestHTTPClient_InvalidCreds(t *testing.T) {
	_, err := HTTPClient(context.Background(), []byte(`{invalid}`), mockProvider{})
	if err == nil {
		t.Fatal("HTTPClient() expected error for invalid creds, got nil")
	}

	_, err = HTTPClient(context.Background(), []byte(`{}`), mockProvider{}) // Missing tokens
	if err == nil {
		t.Fatal("HTTPClient() expected error for missing tokens, got nil")
	}
}
