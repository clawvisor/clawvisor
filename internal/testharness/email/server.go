package email

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Server impersonates the Resend API on /emails. The cloud binary, when
// started with RESEND_BASE_URL pointed here, will route every send through
// the server — captured and queryable via the same Mock helpers (LastTo,
// WaitForSendTo, etc.) as the in-process mailer.
//
// The server captures full requests (subject, html, text), not just tokens —
// which means tests can also assert on email body content if they care.
type Server struct {
	mock *Mock
	srv  *httptest.Server
}

// NewServer starts a Resend-impersonating HTTP server backed by the given
// Mock. The mock and the server share captured state — a send via either
// surface shows up in mock.All().
func NewServer(t *testing.T, mock *Mock) *Server {
	t.Helper()
	s := &Server{mock: mock}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /emails", s.handleSend)
	mux.HandleFunc("POST /emails/", s.handleSend)
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// URL returns the base URL to pass as RESEND_BASE_URL (trailing slash, as
// resend-go expects).
func (s *Server) URL() string {
	return s.srv.URL + "/"
}

// resend SendEmailRequest shape — minimal fields we care about.
type sendReq struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	Html    string   `json:"html"`
	Text    string   `json:"text"`
	ReplyTo string   `json:"reply_to"`
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req sendReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	to := ""
	if len(req.To) > 0 {
		to = req.To[0]
	}
	kind := inferKind(req.Subject)
	token, baseURL := extractTokenAndBaseURL(req.Text)
	s.mock.mu.Lock()
	s.mock.sent = append(s.mock.sent, Sent{
		Kind:    kind,
		To:      to,
		Token:   token,
		BaseURL: baseURL,
		At:      time.Now(),
	})
	s.mock.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"id": fmt.Sprintf("mock-%d", time.Now().UnixNano()),
	})
}

// inferKind maps a Resend subject line to our internal kind tag so the same
// in-process assertion helpers work over the HTTP capture path.
func inferKind(subject string) string {
	s := strings.ToLower(subject)
	switch {
	case strings.Contains(s, "confirm your email"):
		return "verification"
	case strings.Contains(s, "reset your password"):
		return "password_reset"
	case strings.Contains(s, "welcome"):
		return "welcome"
	}
	return "unknown"
}

// extractTokenAndBaseURL recovers the link parts from the plain-text body.
// We send a canonical "...?token=XXX" link in every email — parse it back
// out so tests can use s.Link() or s.Token directly.
func extractTokenAndBaseURL(text string) (token, baseURL string) {
	idx := strings.Index(text, "?token=")
	if idx < 0 {
		return "", ""
	}
	// Walk back from idx to find the URL start.
	start := strings.LastIndex(text[:idx], "http")
	if start < 0 {
		return "", ""
	}
	url := text[start:]
	// Trim trailing whitespace/newlines.
	url = strings.TrimRightFunc(url, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\r' || r == '\t'
	})
	end := strings.IndexAny(url, " \n\r\t")
	if end > 0 {
		url = url[:end]
	}
	parts := strings.SplitN(url, "?token=", 2)
	if len(parts) != 2 {
		return "", ""
	}
	baseURL = strings.TrimSuffix(parts[0], "/verify-email")
	baseURL = strings.TrimSuffix(baseURL, "/reset-password")
	return parts[1], baseURL
}
