package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"
)

const testSecret = "8f742231b10e8888abcd99yyyzzz85a5"

func testNotifier() *Notifier {
	return New(nil, testSecret, AppCredentials{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// signFor produces the header pair Slack would send for this body.
func signFor(t *testing.T, secret string, ts time.Time, body []byte) (string, string) {
	t.Helper()
	stamp := strconv.FormatInt(ts.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "v0:%s:%s", stamp, body)
	return stamp, "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature_AcceptsGenuineRequest(t *testing.T) {
	n := testNotifier()
	body := []byte("payload=%7B%22type%22%3A%22block_actions%22%7D")
	ts, sig := signFor(t, testSecret, time.Now(), body)

	if err := n.verifySignature(ts, sig, body); err != nil {
		t.Fatalf("genuine request rejected: %v", err)
	}
}

func TestVerifySignature_RejectsWrongSecret(t *testing.T) {
	n := testNotifier()
	body := []byte("payload=%7B%7D")
	ts, sig := signFor(t, "not-the-real-secret", time.Now(), body)

	if err := n.verifySignature(ts, sig, body); err == nil {
		t.Fatal("accepted a signature made with the wrong secret")
	}
}

// A signature is only bound to the body it was computed over; swapping the
// body after signing must not verify, or an attacker could replace an
// approval target with one of their choosing.
func TestVerifySignature_RejectsTamperedBody(t *testing.T) {
	n := testNotifier()
	original := []byte("payload=%7B%22target%22%3A%22a%22%7D")
	ts, sig := signFor(t, testSecret, time.Now(), original)

	tampered := []byte("payload=%7B%22target%22%3A%22b%22%7D")
	if err := n.verifySignature(ts, sig, tampered); err == nil {
		t.Fatal("accepted a tampered body under a valid signature")
	}
}

func TestVerifySignature_RejectsStaleAndFutureTimestamps(t *testing.T) {
	n := testNotifier()
	body := []byte("payload=%7B%7D")

	for _, tc := range []struct {
		name string
		when time.Time
	}{
		{"stale", time.Now().Add(-slackTimestampSkew - time.Minute)},
		{"future", time.Now().Add(slackTimestampSkew + time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, sig := signFor(t, testSecret, tc.when, body)
			if err := n.verifySignature(ts, sig, body); err == nil {
				t.Fatalf("accepted a %s timestamp", tc.name)
			}
		})
	}
}

func TestVerifySignature_RejectsMissingHeadersAndUnconfiguredSecret(t *testing.T) {
	n := testNotifier()
	body := []byte("payload=%7B%7D")
	ts, sig := signFor(t, testSecret, time.Now(), body)

	if err := n.verifySignature("", sig, body); err == nil {
		t.Fatal("accepted a request with no timestamp header")
	}
	if err := n.verifySignature(ts, "", body); err == nil {
		t.Fatal("accepted a request with no signature header")
	}
	if err := n.verifySignature(ts, "malformed", body); err == nil {
		t.Fatal("accepted a malformed signature")
	}

	// An unconfigured signing secret must fail closed, not accept everything.
	unset := New(nil, "", AppCredentials{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := unset.verifySignature(ts, sig, body); err == nil {
		t.Fatal("accepted a request when no signing secret is configured")
	}
}
