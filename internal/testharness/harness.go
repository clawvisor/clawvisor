// Package testharness boots in-process mocks of every external service
// clawvisor talks to (Google, GitHub, Slack, Resend, etc.) and exposes
// a scripting API tests use to script responses and assert
// interactions.
//
// Lifecycle:
//
//	h := testharness.New(t)            // starts all mock servers
//	defer h.Reset()                    // also runs via t.Cleanup
//	s := testapp.Start(t, h)           // boots clawvisor-server wired to h
//	h.Email.AssertSentTo(t, "alice@…") // assert via per-mock helpers
//
// Each sub-mock is independent (its own httptest.Server) so tests can
// run in parallel. State is reset between tests via Reset().
package testharness

import (
	"testing"

	"github.com/clawvisor/clawvisor/internal/testharness/email"
	"github.com/clawvisor/clawvisor/internal/testharness/github"
	"github.com/clawvisor/clawvisor/internal/testharness/google"
	"github.com/clawvisor/clawvisor/internal/testharness/linear"
	"github.com/clawvisor/clawvisor/internal/testharness/microsoft"
	"github.com/clawvisor/clawvisor/internal/testharness/notion"
	"github.com/clawvisor/clawvisor/internal/testharness/slack"
	"github.com/clawvisor/clawvisor/internal/testharness/telegram"
	"github.com/clawvisor/clawvisor/internal/testharness/twilio"
)

// Harness owns the lifecycle of every mock external service. Tests
// construct one via New(t); cleanup is automatic via t.Cleanup.
type Harness struct {
	t *testing.T

	Email       *email.Mock
	EmailServer *email.Server // Resend-impersonating HTTP server (for subprocess tests)
	Google      *google.Mock
	GitHub      *github.Mock
	Slack       *slack.Mock
	Notion      *notion.Mock
	Linear      *linear.Mock
	Twilio      *twilio.Mock
	Telegram    *telegram.Mock
	Microsoft   *microsoft.Mock
}

// New starts every mock server and registers cleanup. Subsequent calls
// to Reset() (also run via t.Cleanup) zero state without tearing down
// the servers.
func New(t *testing.T) *Harness {
	t.Helper()
	mailMock := email.NewMock(t)
	h := &Harness{
		t:           t,
		Email:       mailMock,
		EmailServer: email.NewServer(t, mailMock),
		Google:      google.NewMock(t),
		GitHub:      github.NewMock(t),
		Slack:       slack.NewMock(t),
		Notion:      notion.NewMock(t),
		Linear:      linear.NewMock(t),
		Twilio:      twilio.NewMock(t),
		Telegram:    telegram.NewMock(t),
		Microsoft:   microsoft.NewMock(t),
	}
	t.Cleanup(h.Reset)
	return h
}

// Reset zeroes captured state across every mock. Safe to call multiple
// times. Sub-tests can call Reset() between cases for isolation.
func (h *Harness) Reset() {
	if h.Email != nil {
		h.Email.Reset()
	}
	if h.Google != nil {
		h.Google.Reset()
	}
	if h.GitHub != nil {
		h.GitHub.Reset()
	}
	if h.Slack != nil {
		h.Slack.Reset()
	}
	if h.Notion != nil {
		h.Notion.Reset()
	}
	if h.Linear != nil {
		h.Linear.Reset()
	}
	if h.Twilio != nil {
		h.Twilio.Reset()
	}
	if h.Telegram != nil {
		h.Telegram.Reset()
	}
	if h.Microsoft != nil {
		h.Microsoft.Reset()
	}
}

// Env returns the env-var overrides that wire the app at boot to point
// at every mock service. Used by testapp.Start.
func (h *Harness) Env() map[string]string {
	env := map[string]string{}
	merge := func(src map[string]string) {
		for k, v := range src {
			env[k] = v
		}
	}
	merge(h.Google.Env())
	merge(h.GitHub.Env())
	merge(h.Slack.Env())
	merge(h.Notion.Env())
	merge(h.Linear.Env())
	merge(h.Twilio.Env())
	merge(h.Telegram.Env())
	merge(h.Microsoft.Env())
	if h.EmailServer != nil {
		env["RESEND_BASE_URL"] = h.EmailServer.URL()
		env["RESEND_API_KEY"] = "test-resend-key"
		env["RESEND_FROM"] = "test@harness.local"
	}
	return env
}
