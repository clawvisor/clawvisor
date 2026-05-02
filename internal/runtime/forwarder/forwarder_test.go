package forwarder

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStartProxiesEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello "+r.URL.Path)
	}))
	defer upstream.Close()

	upstreamAddr, err := net.ResolveTCPAddr("tcp", strings.TrimPrefix(upstream.URL, "http://"))
	if err != nil {
		t.Fatalf("resolve upstream: %v", err)
	}

	fwd, err := Start(context.Background(), "127.0.0.1:0", upstreamAddr.String())
	if err != nil {
		t.Fatalf("Start: %v", err)
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
	if got, want := string(body), "hello /world"; got != want {
		t.Fatalf("body: got %q want %q", got, want)
	}
}

func TestCloseStopsAccepting(t *testing.T) {
	fwd, err := Start(context.Background(), "127.0.0.1:0", "127.0.0.1:1")
	if err != nil {
		t.Fatalf("Start: %v", err)
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

func TestRejectsEmptyArgs(t *testing.T) {
	if _, err := Start(context.Background(), "", "127.0.0.1:1"); err == nil {
		t.Fatal("expected error for empty bind addr")
	}
	if _, err := Start(context.Background(), "127.0.0.1:0", ""); err == nil {
		t.Fatal("expected error for empty target")
	}
}

func TestAllowFuncRejectsBlocked(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	upstreamAddr := strings.TrimPrefix(upstream.URL, "http://")

	deny := func(net.Addr) bool { return false }
	fwd, err := Start(context.Background(), "127.0.0.1:0", upstreamAddr, WithAllowFunc(deny))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer fwd.Close()

	conn, err := net.Dial("tcp", fwd.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// The forwarder should close the connection without proxying — a read
	// returns EOF immediately.
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err == nil && n > 0 {
		t.Fatalf("expected EOF on rejected connection, got %d bytes", n)
	}
}
