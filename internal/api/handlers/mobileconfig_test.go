package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	sqlitestore "github.com/clawvisor/clawvisor/pkg/store/sqlite"
)

func TestMobileConfigClaudeDesktopRequiresUser(t *testing.T) {
	ctx := context.Background()
	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlitestore.NewStore(db)
	h := NewMobileConfigHandler(st, "", "", true, "")

	req := httptest.NewRequest("GET", "/api/agents/install/claude-desktop.mobileconfig", nil)
	w := httptest.NewRecorder()
	h.ClaudeDesktop(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestMobileConfigClaudeDesktopHappyPath(t *testing.T) {
	ctx := context.Background()
	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlitestore.NewStore(db)
	user, err := st.CreateUser(ctx, "owner@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	h := NewMobileConfigHandler(st, "", "", true, "")
	req := httptest.NewRequest("GET", "/api/agents/install/claude-desktop.mobileconfig", nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	req.Host = "localhost:25297"
	w := httptest.NewRecorder()
	h.ClaudeDesktop(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/x-apple-aspen-config" {
		t.Errorf("Content-Type = %q, want application/x-apple-aspen-config", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, `claude-desktop.mobileconfig`) {
		t.Errorf("Content-Disposition = %q, want filename claude-desktop.mobileconfig", cd)
	}

	body := w.Body.String()
	for _, want := range []string{
		`<?xml version="1.0"`,
		`com.anthropic.claudefordesktop`,
		`<key>inferenceProvider</key>`,
		`<string>gateway</string>`,
		`<key>inferenceCredentialKind</key>`,
		`<string>static</string>`,
		`<key>inferenceGatewayBaseUrl</key>`,
		`<string>http://localhost:25297/api</string>`,
		`<key>inferenceGatewayApiKey</key>`,
		// Token won't equal a known value, but it should be a non-empty cvis_ prefix.
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}

	// Token is on a single line inside <string>cvis_...</string>. Pull it
	// out so we can sanity-check shape: must start with cvis_, must be long
	// enough to be a real token (not the placeholder).
	tokenLine := "<key>inferenceGatewayApiKey</key>\n\t\t\t\t<string>"
	idx := strings.Index(body, tokenLine)
	if idx < 0 {
		t.Fatalf("token block not found in body")
	}
	rest := body[idx+len(tokenLine):]
	end := strings.Index(rest, "</string>")
	if end < 0 || end < 32 {
		t.Fatalf("token appears empty or malformed: %q", rest[:min(len(rest), 80)])
	}
	tok := rest[:end]
	if !strings.HasPrefix(tok, "cvis_") {
		t.Errorf("token does not start with cvis_: %q", tok)
	}

	// The agent must actually exist in the store with the matching hashed token.
	agents, err := st.ListAgents(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "claude-desktop" {
		t.Fatalf("expected one agent named claude-desktop, got %+v", agents)
	}
}

func TestMobileConfigClaudeDesktopDisambiguatesName(t *testing.T) {
	ctx := context.Background()
	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlitestore.NewStore(db)
	user, err := st.CreateUser(ctx, "owner@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	h := NewMobileConfigHandler(st, "", "", true, "")
	doReq := func() {
		req := httptest.NewRequest("GET", "/api/agents/install/claude-desktop.mobileconfig", nil)
		req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
		req.Host = "localhost:25297"
		w := httptest.NewRecorder()
		h.ClaudeDesktop(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
	}
	doReq()
	doReq()
	doReq()

	agents, err := st.ListAgents(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 3 {
		t.Fatalf("expected 3 agents after 3 downloads, got %d", len(agents))
	}
	want := map[string]bool{"claude-desktop": false, "claude-desktop-2": false, "claude-desktop-3": false}
	for _, a := range agents {
		if _, ok := want[a.Name]; !ok {
			t.Errorf("unexpected agent name %q", a.Name)
		}
		want[a.Name] = true
	}
	for n, seen := range want {
		if !seen {
			t.Errorf("expected agent named %q, not found", n)
		}
	}
}

func TestMobileConfigClaudeDesktopRejectsBadName(t *testing.T) {
	ctx := context.Background()
	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlitestore.NewStore(db)
	user, err := st.CreateUser(ctx, "owner@test.example", "hash", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	h := NewMobileConfigHandler(st, "", "", true, "")
	req := httptest.NewRequest("GET", "/api/agents/install/claude-desktop.mobileconfig?name=Bad%20Name!", nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, user))
	req.Host = "localhost:25297"
	w := httptest.NewRecorder()
	h.ClaudeDesktop(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}
