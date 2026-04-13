package handlers

import (
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/adapters/google/credential"
)

func TestCredentialFromTokenPathResponse_PreservesRotatedTokens(t *testing.T) {
	now := time.Date(2026, 4, 13, 12, 0, 0, 0, time.UTC)
	rawResp := map[string]any{
		"authed_user": map[string]any{
			"access_token":  "xoxp-access",
			"refresh_token": "xoxe-refresh",
			"expires_in":    float64(43200),
		},
	}

	credBytes, err := credentialFromTokenPathResponse(
		rawResp,
		"authed_user.access_token",
		[]string{"channels:read"},
		"",
		now,
	)
	if err != nil {
		t.Fatalf("credentialFromTokenPathResponse failed: %v", err)
	}

	stored, err := credential.Parse(credBytes)
	if err != nil {
		t.Fatalf("credential.Parse failed: %v", err)
	}
	if stored.Type != "oauth2" {
		t.Fatalf("expected oauth2 credential type, got %q", stored.Type)
	}
	if stored.AccessToken != "xoxp-access" {
		t.Fatalf("expected access token to be preserved, got %q", stored.AccessToken)
	}
	if stored.RefreshToken != "xoxe-refresh" {
		t.Fatalf("expected refresh token to be preserved, got %q", stored.RefreshToken)
	}
	if got, want := stored.Expiry, now.Add(12*time.Hour); !got.Equal(want) {
		t.Fatalf("expected expiry %s, got %s", want.Format(time.RFC3339), got.Format(time.RFC3339))
	}
	if len(stored.Scopes) != 1 || stored.Scopes[0] != "channels:read" {
		t.Fatalf("expected scopes to be preserved, got %#v", stored.Scopes)
	}
}

func TestCredentialFromTokenPathResponse_FallsBackToExistingRefreshToken(t *testing.T) {
	rawResp := map[string]any{
		"authed_user": map[string]any{
			"access_token": "xoxp-access",
		},
	}

	credBytes, err := credentialFromTokenPathResponse(
		rawResp,
		"authed_user.access_token",
		nil,
		"xoxe-existing",
		time.Date(2026, 4, 13, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("credentialFromTokenPathResponse failed: %v", err)
	}

	stored, err := credential.Parse(credBytes)
	if err != nil {
		t.Fatalf("credential.Parse failed: %v", err)
	}
	if stored.RefreshToken != "xoxe-existing" {
		t.Fatalf("expected existing refresh token fallback, got %q", stored.RefreshToken)
	}
}

func TestCredentialFromTokenPathResponse_RequiresAccessToken(t *testing.T) {
	_, err := credentialFromTokenPathResponse(
		map[string]any{"authed_user": map[string]any{}},
		"authed_user.access_token",
		nil,
		"",
		time.Date(2026, 4, 13, 12, 0, 0, 0, time.UTC),
	)
	if err != errEmptyAccessToken {
		t.Fatalf("expected errEmptyAccessToken, got %v", err)
	}
}
