package isolation

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestForwarderProxiesEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello "+r.URL.Path)
	}))
	defer upstream.Close()

	upstreamURL, err := net.ResolveTCPAddr("tcp", strings.TrimPrefix(upstream.URL, "http://"))
	if err != nil {
		t.Fatalf("resolve upstream: %v", err)
	}

	fwd, err := StartForwarder(context.Background(), "127.0.0.1:0", upstreamURL.String())
	if err != nil {
		t.Fatalf("StartForwarder: %v", err)
	}
	defer fwd.Close()

	resp, err := http.Get("http://" + fwd.Addr() + "/world")
	if err != nil {
		t.Fatalf("GET via forwarder: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := string(body)
	want := "hello /world"
	if got != want {
		t.Fatalf("body: got %q want %q", got, want)
	}
}

func TestForwarderCloseStopsAccepting(t *testing.T) {
	fwd, err := StartForwarder(context.Background(), "127.0.0.1:0", "127.0.0.1:1")
	if err != nil {
		t.Fatalf("StartForwarder: %v", err)
	}
	addr := fwd.Addr()
	if err := fwd.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := fwd.Close(); err != nil {
		t.Fatalf("second Close should be a no-op, got %v", err)
	}
	conn, err := net.Dial("tcp", addr)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("expected dial to fail after Close")
	}
}

func TestForwarderRejectsEmptyArgs(t *testing.T) {
	if _, err := StartForwarder(context.Background(), "", "127.0.0.1:1"); err == nil {
		t.Fatal("expected error for empty bind addr")
	}
	if _, err := StartForwarder(context.Background(), "127.0.0.1:0", ""); err == nil {
		t.Fatal("expected error for empty target")
	}
}
