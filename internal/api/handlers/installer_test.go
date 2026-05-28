package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// installerGet hits the installer endpoint with a target + optional claim and
// returns the rendered markdown body. Fails the test on non-200.
func installerGet(t *testing.T, h *InstallerHandler, target, claim string) string {
	t.Helper()
	path := "/skill/install/" + target + ".md"
	if claim != "" {
		path += "?claim=" + claim
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /skill/install/{target}", h.Setup)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: status %d, body: %s", path, resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("expected text/markdown, got %q", ct)
	}
	return string(body)
}

// assertContainsAll fails the test if any of the needles is missing from body.
// Reports each missing needle individually so a single run surfaces every gap.
func assertContainsAll(t *testing.T, body string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(body, n) {
			t.Errorf("body missing %q", n)
		}
	}
}

func TestInstallerUnknownTargetIs404(t *testing.T) {
	h := NewInstallerHandler("", "", true)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /skill/install/{target}", h.Setup)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/skill/install/perplexity.md")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestInstallerClaudeCodeRender(t *testing.T) {
	h := NewInstallerHandler("", "", true)
	body := installerGet(t, h, "claude-code", "ABCDEFGHIJ")

	assertContainsAll(t, body,
		"# Connect Claude Code to Clawvisor",
		"passthrough mode",
		"## 1. Probe the environment",
		"## 2. Check for an existing token",
		"## 3. Mint a connection request",
		"claim=ABCDEFGHIJ",
		"wait=true",
		"## 4. Persist the token",
		"~/.clawvisor/agents/claude-code.json",
		"## 5. Configure Claude Code",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_CUSTOM_HEADERS",
		"X-Clawvisor-Agent-Token",
		"~/.claude/settings.json",
		"## 6. Offer a shell alias",
		"claude-cv()",
		"--dangerously-skip-permissions",
		"## 7. Connectivity smoke test",
		"/api/skill/catalog",
		"## 8. Save an uninstall reference",
		"## 9. Self-uninstall",
		"rm ~/.claude/commands/clawvisor-install.md",
	)
}

func TestInstallerCodexRender(t *testing.T) {
	h := NewInstallerHandler("", "", true)
	body := installerGet(t, h, "codex", "CLAIMCODE0")

	assertContainsAll(t, body,
		"# Connect Codex to Clawvisor",
		"passthrough mode",
		"codex login",
		"claim=CLAIMCODE0",
		"[model_providers.clawvisor]",
		`base_url = "http://`,
		`/api/v1"`,
		"requires_openai_auth = true",
		"X-Clawvisor-Agent-Token = \"CLAWVISOR_AGENT_TOKEN\"",
		"codex-cv()",
		"--dangerously-bypass-approvals-and-sandbox",
		"~/.codex/skills/clawvisor-install",
	)
}

func TestInstallerHermesRender(t *testing.T) {
	h := NewInstallerHandler("", "", true)
	body := installerGet(t, h, "hermes", "")

	assertContainsAll(t, body,
		"# Connect Hermes to Clawvisor",
		"swap mode",
		"vault their upstream",
		"OPENAI_BASE_URL=",
		"/api/v1",
		"~/.hermes/config.yaml",
		"hermes-cv()",
		"hermes skills uninstall clawvisor-install",
	)
	// No claim → mint URL should not contain `claim=` or `user_id=`.
	if strings.Contains(body, "&claim=") {
		t.Errorf("body should not embed a claim when none was supplied")
	}
}

func TestInstallerOpenClawRender(t *testing.T) {
	h := NewInstallerHandler("", "", true)
	body := installerGet(t, h, "openclaw", "CLAIMOPEN12")

	assertContainsAll(t, body,
		"# Connect OpenClaw to Clawvisor",
		"tool gateway",
		"INTEGRATE_OPENCLAW.md",
		"clawhub install clawvisor",
		"clawvisor-webhook",
		"CLAWVISOR_CALLBACK_SECRET",
		"OPENCLAW_HOOKS_URL",
		"host.docker.internal",
		"openclaw.json",
		"~/.openclaw/workspace/skills/clawvisor-install",
	)
}

func TestInstallerEmbedsRequestHost(t *testing.T) {
	// When not via the relay, the resolved URL should mirror the request host so
	// agents on the user's box talk to the daemon directly.
	h := NewInstallerHandler("", "", true)
	body := installerGet(t, h, "claude-code", "")
	if !strings.Contains(body, "ANTHROPIC_BASE_URL") || !strings.Contains(body, "/api/skill/catalog") {
		t.Fatalf("rendered body missing required scaffolding: %s", body)
	}
	// The httptest server uses an ephemeral 127.0.0.1 host; the body should
	// embed that so the user's curl actually reaches the daemon.
	if !strings.Contains(body, "http://127.0.0.1:") {
		t.Errorf("expected request host to be embedded in URLs, body excerpt:\n%s", body[:min(len(body), 500)])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
