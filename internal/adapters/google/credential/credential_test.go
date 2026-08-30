package credential

import (
	"testing"

	"golang.org/x/oauth2"
)

func TestMissingScopes(t *testing.T) {
	tests := []struct {
		name     string
		existing []string
		required []string
		want     []string
	}{
		{
			name:     "all present",
			existing: []string{"a", "b", "c"},
			required: []string{"a", "b"},
			want:     nil,
		},
		{
			name:     "some missing",
			existing: []string{"a"},
			required: []string{"a", "b", "c"},
			want:     []string{"b", "c"},
		},
		{
			name:     "all missing",
			existing: nil,
			required: []string{"a", "b"},
			want:     []string{"a", "b"},
		},
		{
			name:     "no required",
			existing: []string{"a"},
			required: nil,
			want:     nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MissingScopes(tc.existing, tc.required)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got[%d]=%q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestHasAllScopes(t *testing.T) {
	if !HasAllScopes([]string{"a", "b", "c"}, []string{"a", "b"}) {
		t.Fatal("expected true when all required present")
	}
	if HasAllScopes([]string{"a"}, []string{"a", "b"}) {
		t.Fatal("expected false when scope missing")
	}
}

func TestOAuthConfigForAliasUsesExplicitMapping(t *testing.T) {
	t.Setenv("GOOGLE_OAUTH_ALIAS__EC", "vijay@eightcapital.com")
	t.Setenv("GOOGLE_CLIENT_ID__EC", "ec-client")
	t.Setenv("GOOGLE_CLIENT_SECRET__EC", "ec-secret")
	base := &oauth2.Config{
		ClientID:     "default-client",
		ClientSecret: "default-secret",
	}

	mapped := OAuthConfigForAlias(base, "VIJAY@EIGHTCAPITAL.COM")
	if mapped.ClientID != "ec-client" || mapped.ClientSecret != "ec-secret" {
		t.Fatalf("mapped config = %#v", mapped)
	}
	if base.ClientID != "default-client" {
		t.Fatal("alias selection mutated the default OAuth config")
	}
	unmapped := OAuthConfigForAlias(base, "vijay@vantedgeai.com")
	if unmapped.ClientID != "default-client" {
		t.Fatalf("unmapped client = %q", unmapped.ClientID)
	}
}

func TestOAuthConfigForAliasIgnoresEmptyMapping(t *testing.T) {
	t.Setenv("GOOGLE_OAUTH_ALIAS__EC", "")
	t.Setenv("GOOGLE_CLIENT_ID__EC", "ec-client")
	t.Setenv("GOOGLE_CLIENT_SECRET__EC", "ec-secret")
	base := &oauth2.Config{
		ClientID:     "default-client",
		ClientSecret: "default-secret",
	}

	mapped := OAuthConfigForAlias(base, "")
	if mapped.ClientID != "default-client" ||
		mapped.ClientSecret != "default-secret" {
		t.Fatalf("empty alias selected mapped config: %#v", mapped)
	}
}

func TestOAuthConfigForAliasSupportsAliasOnlyClient(t *testing.T) {
	t.Setenv("GOOGLE_OAUTH_ALIAS__EC", "vijay@eightcapital.com")
	t.Setenv("GOOGLE_CLIENT_ID__EC", "ec-client")
	t.Setenv("GOOGLE_CLIENT_SECRET__EC", "ec-secret")

	mapped := OAuthConfigForAlias(nil, "vijay@eightcapital.com")
	if mapped == nil ||
		mapped.ClientID != "ec-client" ||
		mapped.ClientSecret != "ec-secret" {
		t.Fatalf("alias-only config = %#v", mapped)
	}
	if unmapped := OAuthConfigForAlias(nil, "other@example.com"); unmapped != nil {
		t.Fatalf("unexpected unmapped config = %#v", unmapped)
	}
}

func TestStoredOAuthConfigOverridesDefaultClient(t *testing.T) {
	stored := &Stored{
		OAuthClientID:     "bound-client",
		OAuthClientSecret: "bound-secret",
	}
	base := &oauth2.Config{
		ClientID:     "default-client",
		ClientSecret: "default-secret",
	}
	config := stored.OAuthConfig(base)
	if config.ClientID != "bound-client" ||
		config.ClientSecret != "bound-secret" {
		t.Fatalf("bound config = %#v", config)
	}
	if base.ClientID != "default-client" {
		t.Fatal("credential binding mutated the default OAuth config")
	}
	boundOnly := stored.OAuthConfig(nil)
	if boundOnly == nil ||
		boundOnly.ClientID != "bound-client" ||
		boundOnly.ClientSecret != "bound-secret" {
		t.Fatalf("bound-only config = %#v", boundOnly)
	}
}

func TestFromTokenWithOAuthConfigBindsIssuingClient(t *testing.T) {
	data, err := FromTokenWithOAuthConfig(
		&oauth2.Token{
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
		},
		[]string{"scope"},
		true,
		&oauth2.Config{
			ClientID:     "issuing-client",
			ClientSecret: "issuing-secret",
		},
	)
	if err != nil {
		t.Fatalf("FromTokenWithOAuthConfig: %v", err)
	}
	stored, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if stored.OAuthClientID != "issuing-client" ||
		stored.OAuthClientSecret != "issuing-secret" {
		t.Fatalf("stored client binding = %#v", stored)
	}
}
