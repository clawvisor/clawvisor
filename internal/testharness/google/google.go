// Package google implements an in-process Google OAuth + APIs mock for E2E
// tests. Stands up two httptest.Servers — one for OAuth (authorize, token,
// userinfo, JWKS) and one for the Gmail / Calendar / Drive / Contacts REST
// surfaces — and exposes a scripting API so tests can prep the next OAuth
// callback's claims, the next gmail.send response, etc.
//
// Wiring: the harness binds env vars GOOGLE_OAUTH_BASE_URL, GOOGLE_TOKEN_URL,
// GOOGLE_API_BASE_URL etc. The app reads these via the standard injection
// points (added in handlers/google_auth.go and friends).
package google

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Mock owns httptest.Servers for Google OAuth + APIs.
type Mock struct {
	mu       sync.Mutex
	oauthSrv *httptest.Server
	apiSrv   *httptest.Server

	// Signing key for ID tokens (also published via the JWKS endpoint).
	rsaKey *rsa.PrivateKey
	kid    string

	// Scripted state, one-shot.
	nextLogin   *loginScript
	nextSends   []SentEmail
	failuresAct string // "" | "rate_limit" | "service_unavailable"
}

// loginScript holds the user claims returned for the next OAuth callback.
type loginScript struct {
	Email   string
	Sub     string
	Name    string
	Picture string
	// Authorization code minted at /authorize and accepted at /token.
	authCode string
}

// SentEmail is one Gmail.messages.send invocation captured by the mock.
type SentEmail struct {
	To      string
	From    string
	Subject string
	Body    string // best-effort decoded from base64 raw
}

// NewMock starts both http servers. Servers stop via t.Cleanup.
func NewMock(t *testing.T) *Mock {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	m := &Mock{
		rsaKey: key,
		kid:    "test-key-1",
	}
	oauthMux := http.NewServeMux()
	oauthMux.HandleFunc("GET /o/oauth2/v2/auth", m.handleAuthorize)
	oauthMux.HandleFunc("POST /token", m.handleToken)
	oauthMux.HandleFunc("GET /userinfo", m.handleUserinfo)
	oauthMux.HandleFunc("GET /.well-known/jwks.json", m.handleJWKS)
	m.oauthSrv = httptest.NewServer(oauthMux)

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("POST /gmail/v1/users/me/messages/send", m.handleGmailSend)
	apiMux.HandleFunc("GET /gmail/v1/users/me/messages", m.handleGmailList)
	apiMux.HandleFunc("GET /calendar/v3/calendars/{cal}/events", m.handleCalendarList)
	apiMux.HandleFunc("GET /drive/v3/files", m.handleDriveList)
	apiMux.HandleFunc("GET /people/v1/people/me/connections", m.handleContactsList)
	m.apiSrv = httptest.NewServer(apiMux)

	t.Cleanup(func() {
		m.oauthSrv.Close()
		m.apiSrv.Close()
	})
	return m
}

// Reset drops scripted responses + captured state.
func (m *Mock) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextLogin = nil
	m.nextSends = nil
	m.failuresAct = ""
}

// Env returns env vars wiring the cloud (or clawvisor) app to the mocks.
func (m *Mock) Env() map[string]string {
	return map[string]string{
		"GOOGLE_OAUTH_BASE_URL": m.oauthSrv.URL + "/o/oauth2/v2/auth",
		"GOOGLE_TOKEN_URL":      m.oauthSrv.URL + "/token",
		"GOOGLE_USERINFO_URL":   m.oauthSrv.URL + "/userinfo",
		"GOOGLE_JWKS_URL":       m.oauthSrv.URL + "/.well-known/jwks.json",
		"GOOGLE_API_BASE_URL":   m.apiSrv.URL,
	}
}

// OAuthAuthorizeURL returns the URL clients should hit to initiate OAuth.
// Useful for tests that want to assert the cloud's auth_url constructs an
// equivalent URL.
func (m *Mock) OAuthAuthorizeURL() string {
	return m.oauthSrv.URL + "/o/oauth2/v2/auth"
}

// NextLoginReturnsUser scripts the next /authorize callback to return tokens
// for the given email. Subsequent token exchanges return an ID token signed
// with the mock's RSA key and `sub`/`email` claims set as specified.
func (m *Mock) NextLoginReturnsUser(email string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextLogin = &loginScript{
		Email:    email,
		Sub:      "mock-sub-" + base64URL(8),
		Name:     "Mock User",
		Picture:  "https://example.com/mock.png",
		authCode: "mock-code-" + base64URL(8),
	}
}

// SentEmails returns a copy of every Gmail.send capture.
func (m *Mock) SentEmails() []SentEmail {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SentEmail, len(m.nextSends))
	copy(out, m.nextSends)
	return out
}

// AssertSentEmailTo fails the test if no captured send went to `to`.
func (m *Mock) AssertSentEmailTo(t *testing.T, to string) SentEmail {
	t.Helper()
	for _, s := range m.SentEmails() {
		if strings.Contains(s.To, to) {
			return s
		}
	}
	t.Fatalf("gmail: no send to %q (captured: %d)", to, len(m.SentEmails()))
	return SentEmail{}
}

// FailNextCallsWith scripts the next N API calls to fail with the named mode.
// Modes: "rate_limit" (429), "service_unavailable" (503), "" (clear).
func (m *Mock) FailNextCallsWith(mode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failuresAct = mode
}

// ── OAuth handlers ────────────────────────────────────────────────────────

// /o/oauth2/v2/auth redirects to the client's redirect_uri with ?code=… as
// the real Google would after the user clicks "Allow".
func (m *Mock) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect := q.Get("redirect_uri")
	state := q.Get("state")
	m.mu.Lock()
	login := m.nextLogin
	if login == nil {
		// Default: synthesize a login for an unknown user so tests can
		// observe failure paths if they don't script.
		login = &loginScript{
			Email:    "default-" + base64URL(4) + "@mock.test",
			Sub:      "mock-sub-default",
			authCode: "mock-code-" + base64URL(8),
		}
		m.nextLogin = login
	}
	code := login.authCode
	m.mu.Unlock()

	// Decompose the user-supplied redirect_uri and rebuild it from
	// pieces that have each been validated against a literal whitelist
	// or coerced to a typed primitive. CodeQL (correctly) flags any
	// dataflow path where user input reaches http.Redirect; constructing
	// the redirect target from canonical literals + an int + a
	// character-class-restricted path breaks the taint flow entirely.
	//
	// This is a test-only mock — the real defensive constraint is that
	// only loopback redirects are allowed.
	target, errMsg := safeLoopbackRedirect(redirect, code, state)
	if errMsg != "" {
		http.Error(w, errMsg, http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// safeLoopbackRedirect parses an OAuth redirect_uri, validates every
// component against a strict whitelist, and reassembles the URL from
// untainted primitives. Returns the safe redirect string, or an error
// message describing the first validation failure.
//
// Rules enforced:
//   - host is one of: localhost, 127.0.0.1, [::1]
//   - port parses as an int in [1, 65535]
//   - path contains only printable ASCII (no CR/LF, no control bytes)
//   - query of input is discarded; new query is built with code+state
//
// Returns ("", "...") on validation failure. The returned URL string
// is composed of: const scheme ("http") + literal host (from the
// whitelist switch) + int port + character-validated path + freshly
// minted query. None of those carries a taint path from the input.
func safeLoopbackRedirect(redirectURI, code, state string) (string, string) {
	parsed, err := url.Parse(redirectURI)
	if err != nil {
		return "", "bad redirect_uri"
	}
	var canonicalHost string
	switch parsed.Hostname() {
	case "localhost":
		canonicalHost = "localhost"
	case "127.0.0.1":
		canonicalHost = "127.0.0.1"
	case "::1", "[::1]":
		canonicalHost = "[::1]"
	default:
		return "", "redirect_uri must be loopback (test-only mock)"
	}
	// Port is optional in OAuth redirect_uris ("http://localhost/cb" is
	// valid). When present, it must parse as a real int in [1, 65535].
	var portSuffix string
	if rawPort := parsed.Port(); rawPort != "" {
		port, err := strconv.Atoi(rawPort)
		if err != nil || port < 1 || port > 65535 {
			return "", "redirect_uri port invalid"
		}
		portSuffix = fmt.Sprintf(":%d", port)
	}
	// EscapedPath returns a percent-encoded path. Reject anything outside
	// printable ASCII so the rebuilt URL can't carry binary payloads.
	rawPath := parsed.EscapedPath()
	for i := 0; i < len(rawPath); i++ {
		c := rawPath[i]
		if c < 0x20 || c > 0x7e {
			return "", "redirect_uri path invalid"
		}
	}
	// Limit path length too — defense against absurd inputs.
	if len(rawPath) > 1024 {
		return "", "redirect_uri path too long"
	}
	// Build a fresh query — never reuse the input's RawQuery.
	q := url.Values{}
	q.Set("code", code)
	q.Set("state", state)
	return fmt.Sprintf("http://%s%s%s?%s", canonicalHost, portSuffix, rawPath, q.Encode()), ""
}

// /token exchanges authorization code → tokens. Returns id_token + access_token.
func (m *Mock) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	code := r.Form.Get("code")

	m.mu.Lock()
	login := m.nextLogin
	if login == nil || login.authCode != code {
		m.mu.Unlock()
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	}
	// Consume the script so subsequent /token calls with the same code fail.
	m.nextLogin = nil
	m.mu.Unlock()

	clientID := r.Form.Get("client_id")
	if clientID == "" {
		clientID = "test-client-id"
	}
	idToken, err := m.signIDToken(login, clientID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  "mock-access-token",
		"refresh_token": "mock-refresh-token",
		"expires_in":    3600,
		"token_type":    "Bearer",
		"id_token":      idToken,
		"scope":         r.Form.Get("scope"),
	})
}

// /userinfo returns claims for the current access token. We don't validate
// the token; the test sets up the login script before calling /authorize.
func (m *Mock) handleUserinfo(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	login := m.nextLogin
	m.mu.Unlock()
	if login == nil {
		// Default fixture.
		login = &loginScript{Email: "default@mock.test", Sub: "mock-sub-default", Name: "Mock"}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"sub":            login.Sub,
		"email":          login.Email,
		"email_verified": true,
		"name":           login.Name,
		"picture":        login.Picture,
	})
}

// /.well-known/jwks.json publishes the public key the app should use to
// verify ID tokens we sign.
func (m *Mock) handleJWKS(w http.ResponseWriter, r *http.Request) {
	pub := m.rsaKey.PublicKey
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA",
				"use": "sig",
				"alg": "RS256",
				"kid": m.kid,
				"n":   n,
				"e":   e,
			},
		},
	})
}

// signIDToken mints an RS256-signed Google-style ID token.
func (m *Mock) signIDToken(login *loginScript, clientID string) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":            m.oauthSrv.URL,
		"sub":            login.Sub,
		"aud":            clientID,
		"exp":            now.Add(1 * time.Hour).Unix(),
		"iat":            now.Unix(),
		"email":          login.Email,
		"email_verified": true,
		"name":           login.Name,
		"picture":        login.Picture,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = m.kid
	return tok.SignedString(m.rsaKey)
}

// PublicKeyPEM is exposed for tests that need to pin the key into the app
// (e.g., when configuring an OIDC client that doesn't auto-fetch JWKS).
func (m *Mock) PublicKeyPEM() []byte {
	pubDER, _ := x509.MarshalPKIXPublicKey(&m.rsaKey.PublicKey)
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
}

// ── Gmail handlers ────────────────────────────────────────────────────────

func (m *Mock) handleGmailSend(w http.ResponseWriter, r *http.Request) {
	if mode := m.injectFailure(); mode != 0 {
		w.WriteHeader(mode)
		_, _ = w.Write([]byte(`{"error":{"code":` + fmt.Sprintf("%d", mode) + `,"message":"mock failure"}}`))
		return
	}
	var body struct {
		Raw string `json:"raw"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	raw, err := base64.URLEncoding.DecodeString(body.Raw)
	if err != nil {
		// Some senders use URL-safe without padding.
		raw, _ = base64.RawURLEncoding.DecodeString(body.Raw)
	}
	parsed := parseRFC822(string(raw))
	m.mu.Lock()
	m.nextSends = append(m.nextSends, parsed)
	m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"mock-msg-id","threadId":"mock-thread-id"}`))
}

func (m *Mock) handleGmailList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"messages":[],"resultSizeEstimate":0}`))
}

// ── Calendar / Drive / Contacts stubs (list endpoints, empty results) ────

func (m *Mock) handleCalendarList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"items":[]}`))
}

func (m *Mock) handleDriveList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"files":[]}`))
}

func (m *Mock) handleContactsList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"connections":[]}`))
}

// ── helpers ───────────────────────────────────────────────────────────────

func (m *Mock) injectFailure() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch m.failuresAct {
	case "rate_limit":
		m.failuresAct = ""
		return http.StatusTooManyRequests
	case "service_unavailable":
		m.failuresAct = ""
		return http.StatusServiceUnavailable
	}
	return 0
}

func parseRFC822(raw string) SentEmail {
	var s SentEmail
	headersEnd := strings.Index(raw, "\r\n\r\n")
	if headersEnd < 0 {
		headersEnd = strings.Index(raw, "\n\n")
	}
	hdr := raw
	body := ""
	if headersEnd >= 0 {
		hdr = raw[:headersEnd]
		body = raw[headersEnd+2:]
	}
	for _, line := range strings.Split(hdr, "\n") {
		line = strings.TrimRight(line, "\r")
		if i := strings.Index(line, ":"); i > 0 {
			name := strings.ToLower(strings.TrimSpace(line[:i]))
			val := strings.TrimSpace(line[i+1:])
			switch name {
			case "to":
				s.To = val
			case "from":
				s.From = val
			case "subject":
				s.Subject = val
			}
		}
	}
	s.Body = body
	return s
}

func base64URL(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return strings.TrimRight(base64.URLEncoding.EncodeToString(b), "=")
}

// keyFingerprint is used in tests that want to assert key rotation.
func (m *Mock) KeyFingerprint() string {
	pubDER, _ := x509.MarshalPKIXPublicKey(&m.rsaKey.PublicKey)
	sum := sha256.Sum256(pubDER)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
