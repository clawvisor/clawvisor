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

// TestUserinfoReadsAfterTokenConsumed — earlier, /token cleared
// nextLogin, then /userinfo fell back to the default fixture. Real
// OAuth clients call /userinfo AFTER /token, so they'd see
// "default@mock.test" instead of the user they just authenticated as.
func TestUserinfoReadsAfterTokenConsumed(t *testing.T) {
	m := hg.NewMock(t)
	m.NextLoginReturnsUser("alice@test.local")
	env := m.Env()

	// Step 1: /authorize.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(env["GOOGLE_OAUTH_BASE_URL"] + "?redirect_uri=http%3A%2F%2Flocalhost%2Fcb&state=s")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, err := resp.Location()
	if err != nil {
		t.Fatal(err)
	}
	code := loc.Query().Get("code")

	// Step 2: /token.
	form := url.Values{"code": {code}, "grant_type": {"authorization_code"}, "client_id": {"test"}}
	tokResp, err := http.PostForm(env["GOOGLE_TOKEN_URL"], form)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, tokResp.Body)
	tokResp.Body.Close()

	// Step 3: /userinfo — MUST return alice, not the default.
	infoResp, err := http.Get(env["GOOGLE_USERINFO_URL"])
	if err != nil {
		t.Fatal(err)
	}
	defer infoResp.Body.Close()
	var info struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(infoResp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.Email != "alice@test.local" {
		t.Fatalf("/userinfo after /token: email=%q, want alice@test.local (currentLogin not preserved?)", info.Email)
	}
}

// TestParseRFC822CRLFBodyOffset — the parser must skip the actual
// separator length, not a fixed 2. For "\r\n\r\n" (4 bytes) the
// previous version left the body prefixed with a stray "\r\n".
func TestParseRFC822CRLFBodyOffset(t *testing.T) {
	m := hg.NewMock(t)
	apiURL := m.Env()["GOOGLE_API_BASE_URL"]

	// Build a CRLF RFC822 message.
	raw := "From: bob@x\r\nTo: alice@x\r\nSubject: hi\r\n\r\nhello world"
	encoded := strings.NewReader(`{"raw":"` + base64URLencodeShim(raw) + `"}`)
	resp, err := http.Post(apiURL+"/gmail/v1/users/me/messages/send", "application/json", encoded)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	got := m.AssertSentEmailTo(t, "alice@x")
	if got.Body != "hello world" {
		t.Fatalf("body=%q, want %q (CRLF separator offset wrong?)", got.Body, "hello world")
	}
}

// TestParseRFC822LFOnlyBodyOffset — non-standard but real "\n\n"
// separator (2 bytes) still gives a clean body.
func TestParseRFC822LFOnlyBodyOffset(t *testing.T) {
	m := hg.NewMock(t)
	apiURL := m.Env()["GOOGLE_API_BASE_URL"]
	raw := "From: bob@x\nTo: alice@x\nSubject: lf\n\nlf-only body"
	encoded := strings.NewReader(`{"raw":"` + base64URLencodeShim(raw) + `"}`)
	resp, err := http.Post(apiURL+"/gmail/v1/users/me/messages/send", "application/json", encoded)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := m.AssertSentEmailTo(t, "alice@x")
	if got.Body != "lf-only body" {
		t.Fatalf("body=%q, want %q", got.Body, "lf-only body")
	}
}

// base64URLencodeShim mirrors the existing helper in google_test.go without
// requiring this file to share that package-private name.
func base64URLencodeShim(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	out := make([]byte, 0, len(s)*2)
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
