package handlers

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

func TestResolver_ScriptSession_HappyPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	h, st, _, agent, nonces, placeholder := newSeededResolver(t)
	h.Client = upstream.Client()
	h.Client.Transport = &redirectTargetTransport{base: upstream.URL}

	scripts := llmproxy.NewMemoryScriptSessionCache()

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, scripts, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	token, err := scripts.Mint(context.Background(), llmproxy.ScriptSession{
		ID:              "sess-1",
		UserID:          agent.UserID,
		AgentID:         agent.ID,
		Placeholder:     placeholder,
		ServiceID:       "github",
		TargetHost:      "api.github.com",
		Methods:         []string{"GET"},
		PathPrefixes:    []string{"/repos/x/y"},
		MaxUses:         3,
		MaxRequestBytes: 1024,
		MaxTotalBytes:   4096,
		ExpiresAt:       time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("mint session: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/proxy/repos/x/y/issues", nil)
	req.Header.Set("X-Clawvisor-Target-Host", "api.github.com")
	req.Header.Set("X-Clawvisor-Caller", "Bearer "+token)
	req.Header.Set("Authorization", "Bearer "+placeholder)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestResolver_ScriptSession_ScopeMismatch(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	h, st, _, agent, nonces, placeholder := newSeededResolver(t)
	h.Client = upstream.Client()
	h.Client.Transport = &redirectTargetTransport{base: upstream.URL}

	scripts := llmproxy.NewMemoryScriptSessionCache()

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, scripts, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	token, err := scripts.Mint(context.Background(), llmproxy.ScriptSession{
		ID:           "sess-1",
		UserID:       agent.UserID,
		AgentID:      agent.ID,
		Placeholder:  placeholder,
		ServiceID:    "github",
		TargetHost:   "api.github.com",
		Methods:      []string{"GET"},
		PathPrefixes: []string{"/repos/x/y/issues"},
		MaxUses:      3,
		ExpiresAt:    time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// Path outside the approved prefix.
	req := httptest.NewRequest(http.MethodGet, "/api/proxy/repos/x/y/pulls", nil)
	req.Header.Set("X-Clawvisor-Target-Host", "api.github.com")
	req.Header.Set("X-Clawvisor-Caller", "Bearer "+token)
	req.Header.Set("Authorization", "Bearer "+placeholder)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 SCRIPT_SESSION_SCOPE_MISMATCH, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "SCRIPT_SESSION_SCOPE_MISMATCH") {
		t.Fatalf("expected scope-mismatch code in body, got %s", rec.Body.String())
	}
}

func TestResolver_ScriptSession_Exhausted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	h, st, _, agent, nonces, placeholder := newSeededResolver(t)
	h.Client = upstream.Client()
	h.Client.Transport = &redirectTargetTransport{base: upstream.URL}

	scripts := llmproxy.NewMemoryScriptSessionCache()

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, scripts, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	token, err := scripts.Mint(context.Background(), llmproxy.ScriptSession{
		ID:           "sess-1",
		UserID:       agent.UserID,
		AgentID:      agent.ID,
		Placeholder:  placeholder,
		ServiceID:    "github",
		TargetHost:   "api.github.com",
		Methods:      []string{"GET"},
		PathPrefixes: []string{"/repos"},
		MaxUses:      1,
		ExpiresAt:    time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	doReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/proxy/repos/x/y/issues", nil)
		req.Header.Set("X-Clawvisor-Target-Host", "api.github.com")
		req.Header.Set("X-Clawvisor-Caller", "Bearer "+token)
		req.Header.Set("Authorization", "Bearer "+placeholder)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if r := doReq(); r.Code != http.StatusOK {
		t.Fatalf("first call: expected 200, got %d (%s)", r.Code, r.Body.String())
	}
	r := doReq()
	if r.Code != http.StatusForbidden {
		t.Fatalf("second call: expected 403, got %d (%s)", r.Code, r.Body.String())
	}
	if !strings.Contains(r.Body.String(), "SCRIPT_SESSION_EXHAUSTED") {
		t.Fatalf("expected SCRIPT_SESSION_EXHAUSTED code, got %s", r.Body.String())
	}
}

// TestResolver_ScriptSession_AggregateCapBoundsCrossingRequest exercises
// the aggregate-budget enforcement. With Authorize's optimistic
// reservation in place (cubic round-3 P2 #1), the request that would
// cross MaxTotalBytes is rejected at Authorize BEFORE reaching the
// upstream — strictly better than the prior streaming-clip behavior
// (which still issued the upstream call and truncated the body).
//
// The session here has MaxRequestBytes=1000 and MaxTotalBytes=1000
// (only one reservation fits). First call reserves 1000, streams 800,
// trues up to 800. Second Authorize sees 800 + 1000 > 1000 and
// rejects.
func TestResolver_ScriptSession_AggregateCapBoundsCrossingRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := bytes.Repeat([]byte("A"), 800)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer upstream.Close()

	h, st, _, agent, nonces, placeholder := newSeededResolver(t)
	h.Client = upstream.Client()
	h.Client.Transport = &redirectTargetTransport{base: upstream.URL}

	scripts := llmproxy.NewMemoryScriptSessionCache()

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, scripts, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	token, err := scripts.Mint(context.Background(), llmproxy.ScriptSession{
		ID:              "sess-1",
		UserID:          agent.UserID,
		AgentID:         agent.ID,
		Placeholder:     placeholder,
		ServiceID:       "github",
		TargetHost:      "api.github.com",
		Methods:         []string{"GET"},
		PathPrefixes:    []string{"/repos"},
		MaxUses:         5,
		MaxRequestBytes: 1000,
		MaxTotalBytes:   1000,
		ExpiresAt:       time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	doReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/proxy/repos/x/y/issues", nil)
		req.Header.Set("X-Clawvisor-Target-Host", "api.github.com")
		req.Header.Set("X-Clawvisor-Caller", "Bearer "+token)
		req.Header.Set("Authorization", "Bearer "+placeholder)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	first := doReq()
	if first.Code != http.StatusOK {
		t.Fatalf("first call: expected 200, got %d (%s)", first.Code, first.Body.String())
	}
	if got := first.Body.Len(); got != 800 {
		t.Fatalf("first body len: want 800, got %d", got)
	}

	// After the first call: TotalBytesUsed = 800 (reservation trued up
	// from 1000 → 800). Second Authorize: 800 + 1000 > 1000 →
	// SCRIPT_SESSION_BYTES_EXCEEDED, no upstream call.
	second := doReq()
	if second.Code != http.StatusForbidden {
		t.Fatalf("second call: expected 403 (aggregate cap reservation full), got %d (%s)", second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "SCRIPT_SESSION_BYTES_EXCEEDED") {
		t.Fatalf("second call body must surface SCRIPT_SESSION_BYTES_EXCEEDED, got %s", second.Body.String())
	}
}

// TestResolver_ScriptSession_StripsTruncatingContentLength exercises
// the bug where the upstream's Content-Length leaked through unchanged
// even when the resolver truncated the body to fit the script-session
// cap. Clients that trust Content-Length would then hang or report a
// short read. After the fix the resolver drops the header so Go falls
// back to chunked transfer encoding.
func TestResolver_ScriptSession_StripsTruncatingContentLength(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := bytes.Repeat([]byte("A"), 5000)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer upstream.Close()

	h, st, _, agent, nonces, placeholder := newSeededResolver(t)
	h.Client = upstream.Client()
	h.Client.Transport = &redirectTargetTransport{base: upstream.URL}

	scripts := llmproxy.NewMemoryScriptSessionCache()

	mux := http.NewServeMux()
	mw := middleware.RequireAgentLLMNonce(st, nonces, scripts, slog.Default())
	mux.Handle("/api/proxy/", mw(http.HandlerFunc(h.Forward)))

	token, err := scripts.Mint(context.Background(), llmproxy.ScriptSession{
		ID:              "sess-1",
		UserID:          agent.UserID,
		AgentID:         agent.ID,
		Placeholder:     placeholder,
		ServiceID:       "github",
		TargetHost:      "api.github.com",
		Methods:         []string{"GET"},
		PathPrefixes:    []string{"/repos"},
		MaxUses:         5,
		MaxRequestBytes: 1000, // upstream sends 5000; expect truncation to 1000
		MaxTotalBytes:   10000,
		ExpiresAt:       time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/proxy/repos/x/y/issues", nil)
	req.Header.Set("X-Clawvisor-Target-Host", "api.github.com")
	req.Header.Set("X-Clawvisor-Caller", "Bearer "+token)
	req.Header.Set("Authorization", "Bearer "+placeholder)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.Len(); got != 1000 {
		t.Fatalf("body must be truncated to per-request cap (1000), got %d", got)
	}
	if cl := rec.Header().Get("Content-Length"); cl != "" {
		t.Fatalf("Content-Length must be stripped on truncated script-session response; got %q (upstream claimed 5000, we sent %d)", cl, rec.Body.Len())
	}
}
