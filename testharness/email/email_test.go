package email_test

import (
	"strings"
	"testing"
	"time"

	hemail "github.com/clawvisor/clawvisor/testharness/email"
)

// mailerLike is the cloud-side Mailer interface re-declared locally so
// the assertion runs without importing cloud (clawvisor must not depend
// on its own consumer). Cloud has an equivalent assertion against its
// own interface; if either drifts, both tests fail at compile time.
type mailerLike interface {
	SendVerification(to, token, baseURL string) error
	SendPasswordReset(to, token, baseURL string) error
	SendWelcome(to, appURL string) error
}

// TestMockSatisfiesMailerInterface asserts the mock can be used wherever
// the cloud Mailer interface is expected — this is the whole point of
// exposing the mock as a drop-in.
func TestMockSatisfiesMailerInterface(t *testing.T) {
	var _ mailerLike = (*hemail.Mock)(nil)
}

func TestCapturesVerification(t *testing.T) {
	m := hemail.NewMock(t)
	if err := m.SendVerification("alice@example.com", "tok-123", "https://app.test"); err != nil {
		t.Fatal(err)
	}
	got := m.AssertSentTo(t, "alice@example.com")
	if got.Kind != "verification" {
		t.Fatalf("kind=%q", got.Kind)
	}
	if got.Token != "tok-123" {
		t.Fatalf("token=%q", got.Token)
	}
	if !strings.HasSuffix(got.Link(), "/verify-email?token=tok-123") {
		t.Fatalf("link=%q", got.Link())
	}
}

func TestResetClearsCapturedSends(t *testing.T) {
	m := hemail.NewMock(t)
	_ = m.SendVerification("a@x", "t", "u")
	if got := m.All(); len(got) != 1 {
		t.Fatalf("pre-reset len=%d", len(got))
	}
	m.Reset()
	if got := m.All(); len(got) != 0 {
		t.Fatalf("post-reset len=%d", len(got))
	}
}

// TestWaitForSendToResolvesWhenSendArrives — happy path: a send
// arrives within the timeout and WaitForSendTo returns it.
//
// The fatal-on-timeout path isn't exercised in-process because
// t.Fatalf is goroutine-local; covering it from outside the test
// would require subprocessing the test binary. The risk of
// regression is low — that branch is a single conditional fatal.
func TestWaitForSendToResolvesWhenSendArrives(t *testing.T) {
	m := hemail.NewMock(t)
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = m.SendPasswordReset("b@x", "tok", "u")
	}()
	got := m.WaitForSendTo(t, "b@x", 500*time.Millisecond)
	if got.Kind != "password_reset" {
		t.Fatalf("kind=%q", got.Kind)
	}
}
