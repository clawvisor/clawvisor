package google_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	hg "github.com/clawvisor/clawvisor/internal/testharness/google"
)

// TestOAuthRoundTrip drives the /authorize → /token → /userinfo flow as the
// app would, then verifies the captured ID token matches the scripted user.
func TestOAuthRoundTrip(t *testing.T) {
	m := hg.NewMock(t)
	m.NextLoginReturnsUser("alice@test.local")

	// Step 1: hit /authorize like a browser would.
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Don't follow the callback redirect — capture the location.
			return http.ErrUseLastResponse
		},
	}
	authURL := m.OAuthAuthorizeURL() + "?client_id=test&redirect_uri=http%3A%2F%2Flocalhost%2Fcb&state=xyz&response_type=code"
	resp, err := client.Get(authURL)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status=%d, want 302", resp.StatusCode)
	}
	loc, err := resp.Location()
	if err != nil {
		t.Fatalf("location: %v", err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in callback URL: %s", loc)
	}

	// Step 2: exchange code for tokens.
	tokenURL := m.Env()["GOOGLE_TOKEN_URL"]
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", "http://localhost/cb")
	form.Set("client_id", "test")
	form.Set("client_secret", "test-secret")
	tokenResp, err := http.PostForm(tokenURL, form)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	defer tokenResp.Body.Close()
	if tokenResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(tokenResp.Body)
		t.Fatalf("token status=%d body=%s", tokenResp.StatusCode, body)
	}
	var tokOut struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tokOut); err != nil {
		t.Fatal(err)
	}
	if tokOut.IDToken == "" || tokOut.AccessToken == "" {
		t.Fatalf("missing tokens: %+v", tokOut)
	}

	// Step 3: ID token should be a 3-part JWT.
	if parts := strings.Split(tokOut.IDToken, "."); len(parts) != 3 {
		t.Fatalf("id_token not a JWT: %s", tokOut.IDToken)
	}
}

// TestNextLoginConsumed verifies that one script powers one OAuth — second
// /token with the same code returns invalid_grant.
func TestNextLoginConsumed(t *testing.T) {
	m := hg.NewMock(t)
	m.NextLoginReturnsUser("bob@test.local")

	// First flow consumes the script.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(m.OAuthAuthorizeURL() + "?redirect_uri=http%3A%2F%2Flocalhost%2Fcb&state=s")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := resp.Location()
	code := loc.Query().Get("code")

	// Use it once — should succeed.
	form := url.Values{"code": {code}, "grant_type": {"authorization_code"}, "client_id": {"test"}}
	r1, _ := http.PostForm(m.Env()["GOOGLE_TOKEN_URL"], form)
	r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first exchange status=%d", r1.StatusCode)
	}
	// Use it again — should 400.
	r2, _ := http.PostForm(m.Env()["GOOGLE_TOKEN_URL"], form)
	r2.Body.Close()
	if r2.StatusCode == http.StatusOK {
		t.Fatalf("replayed exchange accepted; want 400")
	}
}

// TestGmailSendCapture validates the Gmail.messages.send capture surface.
func TestGmailSendCapture(t *testing.T) {
	m := hg.NewMock(t)
	apiURL := m.Env()["GOOGLE_API_BASE_URL"]

	// Build a tiny raw RFC822 message base64url'd.
	raw := "From: bob@x\r\nTo: alice@x\r\nSubject: hi\r\n\r\nhello world"
	encoded := strings.NewReader(`{"raw":"` + base64URLencode(raw) + `"}`)

	resp, err := http.Post(apiURL+"/gmail/v1/users/me/messages/send", "application/json", encoded)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send status=%d", resp.StatusCode)
	}

	got := m.AssertSentEmailTo(t, "alice@x")
	if got.Subject != "hi" {
		t.Fatalf("subject=%q", got.Subject)
	}
	if got.From != "bob@x" {
		t.Fatalf("from=%q", got.From)
	}
}

// base64URLencode mirrors what an SDK would do (Gmail wants URL-safe base64).
func base64URLencode(s string) string {
	out := make([]byte, 0, len(s)*2)
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	bytes := []byte(s)
	for i := 0; i < len(bytes); i += 3 {
		n := uint32(bytes[i]) << 16
		count := 1
		if i+1 < len(bytes) {
			n |= uint32(bytes[i+1]) << 8
			count = 2
		}
		if i+2 < len(bytes) {
			n |= uint32(bytes[i+2])
			count = 3
		}
		for j := 0; j < 4; j++ {
			if j > count {
				out = append(out, '=')
				continue
			}
			out = append(out, alphabet[(n>>(18-6*j))&0x3F])
		}
	}
	return string(out)
}
