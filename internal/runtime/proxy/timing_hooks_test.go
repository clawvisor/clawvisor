package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimetiming "github.com/clawvisor/clawvisor/internal/runtime/timing"
	"github.com/clawvisor/clawvisor/internal/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestRuntimeProxyTimingTraceWritesJSONLEntry(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-timing.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	userID, agentID := seedRuntimePrincipal(t, st)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)

	if err := st.CreateTask(ctx, &store.Task{
		ID:            "task-timing",
		UserID:        userID,
		AgentID:       agentID,
		Purpose:       "time traced request",
		Status:        "active",
		Lifetime:      "session",
		SchemaVersion: 2,
		ExpectedEgress: mustJSON(t, []map[string]any{{
			"host":   upstreamURL.Hostname(),
			"method": "POST",
			"path":   "/",
			"why":    "Trace a timing-instrumented request.",
		}}),
		ExpiresInSeconds: 3600,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	traceDir := t.TempDir()
	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = true
	cfg.RuntimeProxy.DataDir = t.TempDir()
	cfg.RuntimeProxy.TimingTraceEnabled = true
	cfg.RuntimeProxy.TimingTraceDir = traceDir

	session := createRuntimeSession(t, st, "session-timing", userID, agentID, false)

	srv, err := NewServer(Config{
		DataDir:        cfg.RuntimeProxy.DataDir,
		Addr:           "127.0.0.1:0",
		LogTimings:     true,
		TimingTraceDir: traceDir,
	}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.InstallTimingTrace()
	srv.InstallSessionGuard(&Authenticator{Store: st, Config: cfg})
	srv.InstallRequestContextCarrier()
	srv.InstallEgressPolicy(PolicyHooks{Store: st, Config: cfg})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Shutdown(ctx) }()

	client := proxyHTTPClient(t, srv)
	req, _ := http.NewRequest(http.MethodPost, upstream.URL, strings.NewReader(`{"hello":"world"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Proxy-Authorization", "Bearer "+session.secret)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("expected upstream success, got %d %q", resp.StatusCode, string(body))
	}

	tracePath := filepath.Join(traceDir, time.Now().UTC().Format("20060102")+".jsonl")
	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("ReadFile(trace): %v", err)
	}
	var last runtimetiming.TraceEntry
	lines := splitNonEmptyLines(string(data))
	if len(lines) == 0 {
		t.Fatal("expected at least one timing trace entry")
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("Unmarshal(last trace entry): %v", err)
	}
	if last.SessionID != session.id {
		t.Fatalf("expected session id %q, got %q", session.id, last.SessionID)
	}
	if last.Method != http.MethodPost {
		t.Fatalf("expected POST method, got %q", last.Method)
	}
	if last.Host != upstreamURL.Hostname() {
		t.Fatalf("expected host %q, got %q", upstreamURL.Hostname(), last.Host)
	}
	if last.Path != "/" {
		t.Fatalf("expected path '/', got %q", last.Path)
	}
	if last.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", last.StatusCode)
	}
	assertTraceSpanPresent(t, last.Spans, "session_guard.auth")
	assertTraceSpanPresent(t, last.Spans, "egress.read_body")
	assertTraceSpanPresent(t, last.Spans, "egress.load_rules")
	assertTraceSpanPresent(t, last.Spans, "egress.match_tasks")
}

func splitNonEmptyLines(s string) []string {
	raw := strings.Split(s, "\n")
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func assertTraceSpanPresent(t *testing.T, spans []runtimetiming.TraceSpan, name string) {
	t.Helper()
	for _, span := range spans {
		if span.Name == name {
			return
		}
	}
	t.Fatalf("expected timing trace span %q in %+v", name, spans)
}
