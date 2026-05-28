package sheets

import (
	"context"
	"testing"

	"golang.org/x/oauth2"

	"github.com/clawvisor/clawvisor/internal/adapters/google/credential"
	"github.com/clawvisor/clawvisor/pkg/adapters"
)

func testGoogleCredential(t *testing.T, scopes ...string) []byte {
	t.Helper()
	cred, err := credential.FromToken(&oauth2.Token{AccessToken: "access-token"}, scopes, true)
	if err != nil {
		t.Fatalf("credential.FromToken: %v", err)
	}
	return cred
}

type staticOAuthProvider struct{}

func (staticOAuthProvider) OAuthClientCredentials() (string, string) {
	return "client-id", "client-secret"
}

func TestSupportedActions(t *testing.T) {
	a := New(staticOAuthProvider{})
	if len(a.SupportedActions()) != 6 {
		t.Fatalf("expected 6 actions, got %d", len(a.SupportedActions()))
	}
}

func TestValidateCredential(t *testing.T) {
	a := New(staticOAuthProvider{})
	if err := a.ValidateCredential(testGoogleCredential(t, scopeSheets)); err != nil {
		t.Fatalf("ValidateCredential: %v", err)
	}
}

func TestExecuteUnsupportedAction(t *testing.T) {
	a := New(staticOAuthProvider{})
	_, err := a.Execute(context.Background(), adapters.Request{Action: "nope", Credential: testGoogleCredential(t, scopeSheets)})
	if err == nil {
		t.Fatalf("expected error")
	}
}
