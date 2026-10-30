package adapters

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPathSegment(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "msg-1", want: "msg-1"},
		{in: "a/b", want: "a%2Fb"},
		{in: "id?x=1#", want: "id%3Fx=1%23"},
		{in: "file.v2..txt", want: "file.v2..txt"},
		{in: "", wantErr: true},
		{in: ".", wantErr: true},
		{in: "..", wantErr: true},
		{in: "../../user/repos", wantErr: true},
		{in: "a/../b", wantErr: true},
	}
	for _, tc := range cases {
		got, err := PathSegment(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("PathSegment(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("PathSegment(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPathSegments(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "people/c123", want: "people/c123"},
		{in: "a b/c?d", want: "a%20b/c%3Fd"},
		{in: "", wantErr: true},
		{in: "people//c123", wantErr: true},
		{in: "/people", wantErr: true},
		{in: "people/../other", wantErr: true},
		{in: "people/.", wantErr: true},
	}
	for _, tc := range cases {
		got, err := PathSegments(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("PathSegments(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("PathSegments(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHasDotSegment(t *testing.T) {
	cases := map[string]bool{
		"/repos/o/r/issues/42":            false,
		"/files/a..b":                     false,
		"/repos/o/r/issues/../../user":    true,
		"/a/./b":                          true,
		"/a/%2e%2E/b":                     true,
		"/a/..%2F..%2Fuser":               true,
		"/a/b/..":                         true,
		"/drive/v3/files/report.v1.final": false,
	}
	for path, want := range cases {
		if got := HasDotSegment(path); got != want {
			t.Errorf("HasDotSegment(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestGuardPathTraversal(t *testing.T) {
	var reached []string
	base := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		reached = append(reached, req.URL.EscapedPath())
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	client := GuardPathTraversal(base)

	resp, err := client.Get("https://api.example.com/repos/o/r/issues/42")
	if err != nil {
		t.Fatalf("clean path refused: %v", err)
	}
	resp.Body.Close()

	if _, err := client.Get("https://api.example.com/repos/o/r/issues/../../../../user/repos"); err == nil {
		t.Fatal("expected dot-segment request to be refused")
	}
	if len(reached) != 1 {
		t.Fatalf("upstream saw %v, want only the clean request", reached)
	}
	if base.Transport == client.Transport {
		t.Fatal("GuardPathTraversal mutated the original client")
	}
}

func TestGuardPathTraversalForwardsCloseIdleConnections(t *testing.T) {
	base := &closeIdleRecorder{}
	GuardPathTraversal(&http.Client{Transport: base}).CloseIdleConnections()
	if !base.closed {
		t.Fatal("CloseIdleConnections was not forwarded to the wrapped transport")
	}
}

type closeIdleRecorder struct {
	http.RoundTripper
	closed bool
}

func (c *closeIdleRecorder) CloseIdleConnections() { c.closed = true }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
