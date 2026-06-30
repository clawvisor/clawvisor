// Package email implements an in-memory cloudemail.Mailer for tests. Captures
// every send and exposes helpers for assertions + link extraction.
package email

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Mailer is the subset of internal/email.Mailer the mock satisfies. Re-declared
// here so the mock package doesn't depend on internal/email (avoids cycles).
type Mailer interface {
	SendVerification(to, token, baseURL string) error
	SendPasswordReset(to, token, baseURL string) error
	SendWelcome(to, appURL string) error
}

// Sent captures one outbound email.
type Sent struct {
	Kind    string // "verification", "password_reset", "welcome"
	To      string
	Token   string // verification + reset only; "" for welcome
	BaseURL string
	At      time.Time
}

// Link returns the link the user would click. Reconstructed from BaseURL + Token
// to mirror what ResendMailer builds.
func (s Sent) Link() string {
	switch s.Kind {
	case "verification":
		return fmt.Sprintf("%s/verify-email?token=%s", strings.TrimRight(s.BaseURL, "/"), s.Token)
	case "password_reset":
		return fmt.Sprintf("%s/reset-password?token=%s", strings.TrimRight(s.BaseURL, "/"), s.Token)
	case "welcome":
		return s.BaseURL
	}
	return ""
}

// Mock captures emails and answers via the Mailer interface.
type Mock struct {
	mu   sync.Mutex
	sent []Sent
}

// NewMock returns an empty mock; nothing is captured until SendXxx is called.
func NewMock(t *testing.T) *Mock { return &Mock{} }

// Reset drops all captured emails. Called by Harness.Reset.
func (m *Mock) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = nil
}

// All returns a copy of every captured send.
func (m *Mock) All() []Sent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Sent, len(m.sent))
	copy(out, m.sent)
	return out
}

// LastTo returns the most recent send to address. nil if no match.
func (m *Mock) LastTo(addr string) *Sent {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.sent) - 1; i >= 0; i-- {
		if m.sent[i].To == addr {
			s := m.sent[i]
			return &s
		}
	}
	return nil
}

// WaitForSendTo polls until a send to addr arrives or timeout. Returns the
// captured Sent or fails the test.
func (m *Mock) WaitForSendTo(t *testing.T, addr string, timeout time.Duration) Sent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s := m.LastTo(addr); s != nil {
			return *s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("email: no send to %q within %s (captured: %v)", addr, timeout, m.All())
	return Sent{}
}

// AssertSentTo fails the test if no send to addr has been captured.
func (m *Mock) AssertSentTo(t *testing.T, addr string) Sent {
	t.Helper()
	if s := m.LastTo(addr); s != nil {
		return *s
	}
	t.Fatalf("email: expected a send to %q, captured: %v", addr, m.All())
	return Sent{}
}

// ExtractLink pulls the first http(s):// link out of the captured email
// matching the given pattern. Useful when the test prefers to "click" the
// link rather than reconstruct it.
func (s Sent) ExtractLink(pattern *regexp.Regexp) string {
	// Mock doesn't store rendered HTML/text; link is canonical.
	link := s.Link()
	if pattern == nil || pattern.MatchString(link) {
		return link
	}
	return ""
}

// SendVerification captures the send. Always nil error.
func (m *Mock) SendVerification(to, token, baseURL string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, Sent{Kind: "verification", To: to, Token: token, BaseURL: baseURL, At: time.Now()})
	return nil
}

// SendPasswordReset captures the send. Always nil error.
func (m *Mock) SendPasswordReset(to, token, baseURL string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, Sent{Kind: "password_reset", To: to, Token: token, BaseURL: baseURL, At: time.Now()})
	return nil
}

// SendWelcome captures the send. Always nil error.
func (m *Mock) SendWelcome(to, appURL string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, Sent{Kind: "welcome", To: to, BaseURL: appURL, At: time.Now()})
	return nil
}
