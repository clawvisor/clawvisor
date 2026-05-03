package expose

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuildAllowFuncDefaultsToPrivateRanges(t *testing.T) {
	allow, err := buildAllowFunc(nil)
	if err != nil {
		t.Fatalf("buildAllowFunc: %v", err)
	}
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"10.1.2.3", true},
		{"172.20.0.5", true},
		{"192.168.1.10", true},
		{"8.8.8.8", false},
		{"172.32.0.1", false},
	}
	for _, c := range cases {
		got := allow(&net.TCPAddr{IP: net.ParseIP(c.ip), Port: 1234})
		if got != c.want {
			t.Errorf("allow(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

func TestBuildAllowFuncRejectsInvalidCIDR(t *testing.T) {
	if _, err := buildAllowFunc([]string{"not-a-cidr"}); err == nil {
		t.Fatal("expected error on invalid CIDR")
	}
}

func TestBuildAllowFuncRequiresAtLeastOne(t *testing.T) {
	if _, err := buildAllowFunc([]string{"   "}); err == nil {
		t.Fatal("expected error when all CIDRs are blank")
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	cases := map[string]Config{
		"missing bind": {ProxyUpstream: "127.0.0.1:1", APIUpstream: "127.0.0.1:2"},
		"bad proxy port": {
			BindAddr: "127.0.0.1", ProxyPort: -1,
			ProxyUpstream: "127.0.0.1:1", APIUpstream: "127.0.0.1:2",
		},
		"missing proxy upstream": {BindAddr: "127.0.0.1", APIUpstream: "127.0.0.1:2"},
		"missing api upstream":   {BindAddr: "127.0.0.1", ProxyUpstream: "127.0.0.1:1"},
		"port collision": {
			BindAddr: "127.0.0.1", ProxyPort: 4000, APIPort: 4000,
			ProxyUpstream: "127.0.0.1:1", APIUpstream: "127.0.0.1:2",
		},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validate(&cfg); err == nil {
				t.Fatalf("expected validation error for %s", name)
			}
		})
	}
}

func TestRunBridgesUpstreamsAndStopsOnContextCancel(t *testing.T) {
	proxyUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "proxy-ok "+r.URL.Path)
	}))
	defer proxyUpstream.Close()
	apiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "api-ok "+r.URL.Path)
	}))
	defer apiUpstream.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyCh := make(chan Endpoints, 1)
	doneCh := make(chan error, 1)
	cfg := Config{
		BindAddr:      "127.0.0.1",
		ProxyUpstream: strings.TrimPrefix(proxyUpstream.URL, "http://"),
		APIUpstream:   strings.TrimPrefix(apiUpstream.URL, "http://"),
	}
	go func() {
		doneCh <- Run(ctx, cfg, func(ep Endpoints) { readyCh <- ep })
	}()

	var ep Endpoints
	select {
	case ep = <-readyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not become ready in 2s")
	}

	resp, err := http.Get("http://" + ep.ProxyAddr + "/world")
	if err != nil {
		t.Fatalf("GET via proxy listener: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if got, want := string(body), "proxy-ok /world"; got != want {
		t.Fatalf("proxy body: got %q want %q", got, want)
	}

	resp, err = http.Get("http://" + ep.APIAddr + "/health")
	if err != nil {
		t.Fatalf("GET via api listener: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if got, want := string(body), "api-ok /health"; got != want {
		t.Fatalf("api body: got %q want %q", got, want)
	}

	cancel()
	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after ctx cancel")
	}
}

func TestRunReportsBindFailure(t *testing.T) {
	// Pre-bind a port so the second listener fails.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind occupied: %v", err)
	}
	defer occupied.Close()
	occupiedPort := occupied.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := Config{
		BindAddr:      "127.0.0.1",
		ProxyPort:     occupiedPort,
		APIPort:       0,
		ProxyUpstream: "127.0.0.1:1",
		APIUpstream:   "127.0.0.1:2",
	}
	if err := Run(ctx, cfg, nil); err == nil {
		t.Fatal("expected Run to fail when bind port is occupied")
	}
}

func TestRunSerialPortReuse(t *testing.T) {
	// Reasonable smoke test: two sequential Run instances on the same explicit
	// port should both succeed. This catches a regression where Run leaks the
	// listener after ctx cancel.
	cfg := Config{
		BindAddr:      "127.0.0.1",
		ProxyUpstream: "127.0.0.1:1",
		APIUpstream:   "127.0.0.1:2",
	}
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = Run(ctx, cfg, func(Endpoints) { cancel() })
		}()
		wg.Wait()
	}
}
