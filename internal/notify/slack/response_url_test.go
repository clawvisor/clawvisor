package slack

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// response_url arrives inside the interaction payload, so it is
// attacker-chosen input to an outbound request from inside the deployment's
// network. It is rebuilt on a constant host so a forged payload cannot reach
// cloud metadata or internal services.
func TestSanitizedResponseURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		want string
	}{
		{"genuine Slack response_url", "https://hooks.slack.com/actions/T0001/123/abcXYZ", "https://hooks.slack.com/actions/T0001/123/abcXYZ"},
		{"host casing is normalised", "https://Hooks.Slack.Com/actions/T1/2/3", "https://hooks.slack.com/actions/T1/2/3"},
		{"query is preserved", "https://hooks.slack.com/actions/T1/2/3?x=1", "https://hooks.slack.com/actions/T1/2/3?x=1"},
		// Rebuilding drops the parts that make a hostile URL read as Slack's.
		{"userinfo is stripped", "https://hooks.slack.com@evil.example/x", ""},
		{"port is dropped", "https://hooks.slack.com:8443/actions/T1/2/3", "https://hooks.slack.com/actions/T1/2/3"},
		{"fragment is dropped", "https://hooks.slack.com/actions/T1#frag", "https://hooks.slack.com/actions/T1"},

		{"empty", "", ""},
		{"http downgrade", "http://hooks.slack.com/actions/T1/2/3", ""},
		{"unrelated host", "https://evil.example.com/actions/T1/2/3", ""},
		{"cloud metadata", "http://169.254.169.254/latest/meta-data/", ""},
		{"loopback", "http://127.0.0.1:8080/internal", ""},
		{"internal hostname", "https://redis.internal:6379/", ""},
		{"suffix impersonation", "https://hooks.slack.com.evil.example/x", ""},
		{"prefix impersonation", "https://evilhooks.slack.com.attacker/x", ""},
		{"scheme-less", "hooks.slack.com/actions/T1/2/3", ""},
		{"garbage", "://not a url", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizedResponseURL(tc.url); got != tc.want {
				t.Fatalf("sanitizedResponseURL(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

// A host allowlist has to hold for every hop, not just the first. Go's
// default client follows up to 10 redirects, so without this policy a
// permitted hooks.slack.com URL answering 302 would carry the request to an
// arbitrary host and step straight past the hostname lock.
func TestResponseClient_RefusesRedirects(t *testing.T) {
	var reachedInternal bool
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reachedInternal = true
		w.WriteHeader(http.StatusOK)
	}))
	defer internal.Close()

	for _, code := range []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect, // preserves method and body
		http.StatusPermanentRedirect, // preserves method and body
	} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			reachedInternal = false
			redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, internal.URL+"/pivot", code)
			}))
			defer redirector.Close()

			_, err := newResponseClient().Post(redirector.URL, "application/json", strings.NewReader(`{}`))
			if err == nil {
				t.Fatal("redirect was followed; the hostname lock does not survive a hop")
			}
			if !errors.Is(err, errRedirectRefused) {
				t.Fatalf("got %v, want errRedirectRefused", err)
			}
			if reachedInternal {
				t.Fatal("request reached the redirect target")
			}
		})
	}
}

// The ordinary case must still work.
func TestResponseClient_AllowsDirectResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := newResponseClient().Post(srv.URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("direct POST failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
}
