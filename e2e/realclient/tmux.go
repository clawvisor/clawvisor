//go:build realclient

// Package realclient drives real Claude Code / Codex TUIs in tmux against a
// live proxy-lite server and asserts server-side state (attribution, key
// injection, approval round-trips) over HTTP. It is gated behind the
// `realclient` build tag so it never runs in `go test ./...`, and every test
// additionally skips unless CLAWVISOR_REALCLIENT=1 and the required provider
// key is set.
//
// Design notes (see docs spec 07 and global gotcha #9):
//   - Never fixed sleeps for readiness: poll with deadlines.
//   - tmux capture-pane is a snapshot; clients repaint constantly, so we poll.
//   - On any pane assertion timeout we dump the FULL pane so CI failures are
//     diagnosable.
//   - Never assert on model output text; assert on server-side records.
package realclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// ansiEscape matches CSI escape sequences so pane text can be matched on stable
// substrings rather than raw terminal bytes (spec: regexp on \x1b\[[0-9;]*[a-zA-Z]).
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// ── gating ──────────────────────────────────────────────────────────────────

// requireRealClient skips unless the lane is explicitly opted into. Keeps the
// tag-compiled tests inert on a normal checkout.
func requireRealClient(t *testing.T) {
	t.Helper()
	if os.Getenv("CLAWVISOR_REALCLIENT") != "1" {
		t.Skip("realclient lane disabled (set CLAWVISOR_REALCLIENT=1 to run)")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed; skipping realclient lane")
	}
}

// requireKey skips unless the named provider key env var is present.
func requireKey(t *testing.T, envName string) string {
	t.Helper()
	key := strings.TrimSpace(os.Getenv(envName))
	if key == "" {
		t.Skipf("%s not set; skipping", envName)
	}
	return key
}

// requireCommand skips when the client binary isn't installed.
func requireCommand(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%q not on PATH; skipping (install the client to run this lane)", name)
	}
}

// ── tmux driver ─────────────────────────────────────────────────────────────

// tmuxSession is a handle to a detached tmux session, torn down via t.Cleanup.
type tmuxSession struct {
	t    *testing.T
	name string
}

// newSession starts a detached tmux session sized 200x50 and registers cleanup.
func newSession(t *testing.T, name string) *tmuxSession {
	t.Helper()
	run(t, "tmux", "new-session", "-d", "-s", name, "-x", "200", "-y", "50")
	s := &tmuxSession{t: t, name: name}
	t.Cleanup(func() {
		// Best-effort kill; ignore errors (session may already be gone).
		_ = exec.Command("tmux", "kill-session", "-t", name).Run()
	})
	return s
}

// sendKeys types text into the session and presses Enter.
func (s *tmuxSession) sendKeys(text string) {
	s.t.Helper()
	run(s.t, "tmux", "send-keys", "-t", s.name, text, "Enter")
}

// sendRaw sends a key sequence without appending Enter (e.g. control keys).
func (s *tmuxSession) sendRaw(keys ...string) {
	s.t.Helper()
	args := append([]string{"send-keys", "-t", s.name}, keys...)
	run(s.t, "tmux", args...)
}

// capture returns the current pane contents (last 200 lines), ANSI-stripped.
func (s *tmuxSession) capture() string {
	s.t.Helper()
	out, err := exec.Command("tmux", "capture-pane", "-pt", s.name, "-S", "-200").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("<capture-pane failed: %v>", err)
	}
	return ansiEscape.ReplaceAllString(string(out), "")
}

// waitForPane polls the pane every 500ms until the regex matches or the
// deadline passes. On timeout it fails with the FULL pane dump for diagnosis.
func (s *tmuxSession) waitForPane(re *regexp.Regexp, timeout time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = s.capture()
		if re.MatchString(last) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	s.t.Fatalf("timed out after %s waiting for /%s/ in pane %q.\n=== FULL PANE DUMP ===\n%s\n=== END PANE ===",
		timeout, re.String(), s.name, last)
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// ── server + agent setup ────────────────────────────────────────────────────

// liteServer bundles the running server with an agent registered against it and
// the vaulted provider key.
type liteServer struct {
	srv      *testapp.Server
	user     *testapp.LocalUser
	agent    *testapp.Agent
	agentTok string
	baseURL  string
}

// bootLite boots a real clawvisor-server with proxy-lite enabled, logs in the
// local user, registers an agent, and stores the provider key in the vault. The
// agent process is then given ONLY its cvis_ token — which is what makes key
// injection assertable. Real upstreams stand (no CLAWVISOR_LLM_UPSTREAM_*).
func bootLite(t *testing.T, provider, providerKey string) *liteServer {
	t.Helper()
	h := testharness.New(t)
	srv := testapp.StartWith(t, h, nil)
	user := srv.LoginAsLocalUser(t)
	agent := srv.CreateAgent(t, user, fmt.Sprintf("realclient-%s-%d", provider, time.Now().Unix()))
	srv.SetLLMCredential(t, user, provider, "", providerKey)
	return &liteServer{
		srv:      srv,
		user:     user,
		agent:    agent,
		agentTok: agent.Token,
		baseURL:  srv.URL,
	}
}

// deleteVaultKey removes the vaulted provider key (used by the key-injection
// negative check).
func (l *liteServer) deleteVaultKey(t *testing.T, provider string) {
	t.Helper()
	req, _ := http.NewRequest("DELETE", l.baseURL+"/api/runtime/llm-credentials/"+provider, nil)
	req.Header.Set("Authorization", "Bearer "+l.user.AccessToken)
	resp, err := l.srv.Client.Do(req)
	if err != nil {
		t.Fatalf("delete vault key: %v", err)
	}
	resp.Body.Close()
}

// ── client env contracts (mirror internal/clawvisorcli buildLiteProxyEnv) ────

// claudeEnv returns the env that routes Claude Code through proxy-lite with the
// agent token, plus a pre-seeded config dir that skips onboarding/trust prompts.
func claudeEnv(t *testing.T, l *liteServer) []string {
	t.Helper()
	apiBase := strings.TrimRight(l.baseURL, "/") + "/api"
	cfgDir := claudeConfigDir(t)
	return append(os.Environ(),
		"ANTHROPIC_BASE_URL="+apiBase,
		"ANTHROPIC_CUSTOM_HEADERS=X-Clawvisor-Agent-Token: "+l.agentTok,
		"ANTHROPIC_AUTH_TOKEN=",
		"ANTHROPIC_API_KEY=",
		"CLAUDE_CONFIG_DIR="+cfgDir,
		// Non-interactive-friendly defaults.
		"CI=1",
	)
}

// codexEnv returns the env that routes Codex through proxy-lite. Codex sends
// OPENAI_API_KEY as the bearer, which proxy-lite reads as the agent token.
func codexEnv(t *testing.T, l *liteServer) []string {
	t.Helper()
	apiBase := strings.TrimRight(l.baseURL, "/") + "/api/v1"
	return append(os.Environ(),
		"OPENAI_BASE_URL="+apiBase,
		"OPENAI_API_KEY="+l.agentTok,
		"CI=1",
	)
}

// claudeConfigDir writes a minimal Claude Code config dir that skips the
// first-run onboarding/trust prompts so the TUI reaches a prompt immediately.
func claudeConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	settings := map[string]any{
		"hasCompletedOnboarding": true,
		"hasTrustDialogAccepted": true,
		"theme":                  "dark",
	}
	b, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.WriteFile(dir+"/settings.json", b, 0600); err != nil {
		t.Fatalf("write claude settings: %v", err)
	}
	return dir
}

// launchInTmux writes the env to a wrapper script and launches the command in
// the given session (env can't be passed through send-keys cleanly).
func (s *tmuxSession) launch(env []string, command string) {
	s.t.Helper()
	dir := s.t.TempDir()
	script := dir + "/launch.sh"
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\n")
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && isLaunchEnvKey(k) {
			fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(v))
		}
	}
	b.WriteString(command + "\n")
	if err := os.WriteFile(script, []byte(b.String()), 0700); err != nil {
		s.t.Fatalf("write launch script: %v", err)
	}
	s.sendKeys("bash " + script)
}

// isLaunchEnvKey keeps the exported env small: only the client-routing vars, not
// the entire inherited environment (which send-keys/scripts would bloat).
func isLaunchEnvKey(k string) bool {
	switch {
	case strings.HasPrefix(k, "ANTHROPIC_"),
		strings.HasPrefix(k, "OPENAI_"),
		strings.HasPrefix(k, "CLAUDE_"),
		k == "CI", k == "PATH", k == "HOME":
		return true
	}
	return false
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ── server-state assertions ─────────────────────────────────────────────────

// getJSON does an authenticated GET and decodes into dst. The polling endpoints
// (audit, approvals) return 200 on every poll, so any non-2xx status or a
// read/decode failure is a real server error — we fail fast on it rather than
// let it hide behind an opaque poll timeout.
func (l *liteServer) getJSON(t *testing.T, path string, dst any) int {
	t.Helper()
	req, _ := http.NewRequest("GET", l.baseURL+path, nil)
	req.Header.Set("Authorization", "Bearer "+l.user.AccessToken)
	resp, err := l.srv.Client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: read body: %v", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("GET %s: unexpected status %d: %s", path, resp.StatusCode, body)
	}
	if dst != nil && len(body) > 0 {
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(dst); err != nil {
			t.Fatalf("GET %s: decode body: %v: %s", path, err, body)
		}
	}
	return resp.StatusCode
}

// postJSON does an authenticated POST with a JSON body.
func (l *liteServer) postJSON(t *testing.T, path string, body any) int {
	t.Helper()
	var buf io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewReader(b)
	}
	req, _ := http.NewRequest("POST", l.baseURL+path, buf)
	req.Header.Set("Authorization", "Bearer "+l.user.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.srv.Client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// pollUntil polls fn every 500ms until it returns true or the deadline passes.
// Returns false on timeout (callers decide whether that's fatal).
func pollUntil(ctx context.Context, timeout time.Duration, fn func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		if fn() {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// waitForAgentAudit polls GET /api/audit?agent_id= for a proxy-lite audit row
// attributed to the agent — the attribution signal. Proxy-lite writes one
// audit_log row per /api/v1/* request with the agent id (see
// internal/api/handlers/llm_endpoint.go), so a matching row proves the real
// client's traffic reached the server as that agent.
func (l *liteServer) waitForAgentAudit(t *testing.T, agentID string, timeout time.Duration) bool {
	t.Helper()
	return pollUntil(context.Background(), timeout, func() bool {
		var out struct {
			Total   int `json:"total"`
			Entries []struct {
				AgentID *string `json:"agent_id"`
			} `json:"entries"`
		}
		l.getJSON(t, "/api/audit?agent_id="+agentID+"&include_runtime=true&limit=50", &out)
		for _, e := range out.Entries {
			if e.AgentID != nil && *e.AgentID == agentID {
				return true
			}
		}
		return out.Total > 0
	})
}

// waitForPendingApproval polls GET /api/approvals for a live pending entry and
// returns its request_id (the key the approve/deny routes use). Empty on
// timeout. Proxy-lite tool_use holds surface here as store.PendingApproval rows.
func (l *liteServer) waitForPendingApproval(t *testing.T, timeout time.Duration) string {
	t.Helper()
	var found string
	pollUntil(context.Background(), timeout, func() bool {
		var out struct {
			Entries []struct {
				RequestID string `json:"request_id"`
				Status    string `json:"status"`
			} `json:"entries"`
		}
		l.getJSON(t, "/api/approvals", &out)
		for _, a := range out.Entries {
			if a.Status == "pending" || a.Status == "" {
				found = a.RequestID
				return true
			}
		}
		return false
	})
	return found
}

// resolveApproval approves or denies a pending approval by request_id via the
// real approvals routes (POST /api/approvals/{request_id}/approve|deny).
func (l *liteServer) resolveApproval(t *testing.T, requestID string, approve bool) int {
	t.Helper()
	verb := "deny"
	if approve {
		verb = "approve"
	}
	return l.postJSON(t, "/api/approvals/"+requestID+"/"+verb, nil)
}
