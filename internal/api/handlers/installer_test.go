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
	return installerGetPath(t, h, path)
}

func installerGetQuery(t *testing.T, h *InstallerHandler, target, query string) string {
	t.Helper()
	path := "/skill/install/" + target + ".md"
	if query != "" {
		path += "?" + query
	}
	return installerGetPath(t, h, path)
}

func installerGetPath(t *testing.T, h *InstallerHandler, path string) string {
	t.Helper()
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
	h := NewInstallerHandler("", "", true, "", "")
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
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGet(t, h, "claude-code", "ABCDEFGHIJ")

	assertContainsAll(t, body,
		"# Connect Claude Code to Clawvisor",
		"passthrough mode",
		"## 1. Check the local CLI",
		"install_mode\": \"local\"",
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
		"Dashboard answers",
		"Claude Code routing scope: alias",
		"The user chose **scoped routing**",
		"Leave permissions unchanged",
		"## 6. Offer a shell alias",
		"claude-cv()",
		"## 7. Connectivity smoke test",
		"/api/skill/catalog",
		"## 8. Save an uninstall reference",
		"## 9. Self-uninstall automatically",
		"rm -f ~/.claude/commands/clawvisor-install.md",
		"rm -rf ~/.codex/skills/clawvisor-install",
	)
}

func TestInstallerCodexRender(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGet(t, h, "codex", "CLAIMCODE0")

	assertContainsAll(t, body,
		"# Connect Codex to Clawvisor",
		"passthrough mode",
		"codex login",
		"## 1. Check the local CLI",
		"install_mode\": \"local\"",
		"claim=CLAIMCODE0",
		"[model_providers.clawvisor]",
		`base_url = "http://`,
		`/api/v1"`,
		"requires_openai_auth = true",
		"X-Clawvisor-Agent-Token = \"CLAWVISOR_AGENT_TOKEN\"",
		"Dashboard answers",
		"Alias mode: safe",
		"codex-cv()",
		"rm -rf ~/.codex/skills/clawvisor-install",
	)
}

func TestInstallerHermesRender(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGet(t, h, "hermes", "")

	assertContainsAll(t, body,
		"# Connect Hermes to Clawvisor",
		"swap mode",
		"vault their upstream",
		"OPENAI_BASE_URL=",
		"/api/v1",
		"~/.hermes/config.yaml",
		"hermes-cv()",
		"rm -f ~/.claude/commands/clawvisor-install.md",
		"rm -rf ~/.codex/skills/clawvisor-install",
	)
	// No claim → mint URL should not contain `claim=` or `user_id=`.
	if strings.Contains(body, "&claim=") {
		t.Errorf("body should not embed a claim when none was supplied")
	}
}

func TestInstallerOpenClawRender(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGet(t, h, "openclaw", "CLAIMOPEN12")

	assertContainsAll(t, body,
		"# Connect OpenClaw to Clawvisor",
		"tool gateway",
		"INTEGRATE_OPENCLAW.md",
		"OpenClaw running mode: host",
		"## 1. Probe the environment",
		"clawhub install clawvisor",
		"clawvisor-webhook",
		"CLAWVISOR_CALLBACK_SECRET",
		"OPENCLAW_HOOKS_URL",
		"host.docker.internal",
		"openclaw.json",
		"rm -f ~/.claude/commands/clawvisor-install.md",
		"rm -rf ~/.codex/skills/clawvisor-install",
	)
}

func TestInstallerOpenClawRemoteModeSkipsLocalProbe(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGetQuery(t, h, "openclaw", "claim=CLAIMOPEN12&openclaw_mode=remote&policy_setup=after&task_approval=low")

	assertContainsAll(t, body,
		"Dashboard answers",
		"OpenClaw running mode: remote",
		"Preferred task auto-approval default: low-risk conversation updates.",
		"## 1. Confirm remote OpenClaw access",
		"Do **not** probe the",
		"export OPENCLAW_REMOTE=",
		"install_mode\": \"remote\"",
		"Do not check the local filesystem",
		"ssh \"$OPENCLAW_REMOTE\"",
		"remote-reachable Clawvisor URL",
		"OPENCLAW_CLAWVISOR_URL",
	)

	for _, forbidden := range []string{
		"## 1. Probe the environment",
		"`docker ps",
		"check `~/.openclaw/` on the host",
		"# Host install:",
		"Both OpenClaw and Clawvisor in Docker on same host",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("remote-mode body should not contain local-mode text %q", forbidden)
		}
	}
}

// TestInstallerAllTargetsHaveFrontmatter — Codex rejects skills without YAML
// frontmatter at load time; we caught this in the field after a real install,
// so guard against regression by asserting the exact shape on every target.
func TestInstallerAllTargetsHaveFrontmatter(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	for _, target := range []string{"claude-code", "codex", "hermes", "openclaw"} {
		body := installerGet(t, h, target, "")
		if !strings.HasPrefix(body, "---\nname: clawvisor-install\ndescription:") {
			t.Errorf("[%s] missing required YAML frontmatter at top of body. First 200 chars:\n%s",
				target, body[:min(len(body), 200)])
		}
		// Closing fence must come before the heading or downstream loaders
		// treat the body as part of the frontmatter.
		fenceEnd := strings.Index(body, "\n---\n")
		heading := strings.Index(body, "# Connect")
		if fenceEnd < 0 || heading < 0 || fenceEnd > heading {
			t.Errorf("[%s] frontmatter not properly closed before heading (fenceEnd=%d, heading=%d)",
				target, fenceEnd, heading)
		}
	}
}

func TestInstallerPrefersLLMProxyURL(t *testing.T) {
	// When the deployment has a configured lite-proxy URL, all embedded curl
	// URLs must use that — even if the app has its own public URL. Agents
	// installing the skill need the LLM-proxy endpoint for both registration
	// and model traffic.
	h := NewInstallerHandler("", "", false, "https://llm.example.com", "https://app.example.com")
	body := installerGet(t, h, "claude-code", "")
	if !strings.Contains(body, "https://llm.example.com/api") {
		t.Errorf("expected embedded URL to use configured LLM proxy URL; body excerpt:\n%s",
			body[:min(len(body), 500)])
	}
	if strings.Contains(body, "https://app.example.com") {
		t.Errorf("embedded URL should not use server public URL when LLM proxy URL is configured")
	}
	if strings.Contains(body, "http://127.0.0.1:") {
		t.Errorf("embedded URL should not fall back to request host when LLM proxy URL is configured")
	}
}

func TestInstallerFallsBackToServerPublicURL(t *testing.T) {
	// If there is no dedicated lite-proxy URL, use the general public URL
	// before falling back to the request host.
	h := NewInstallerHandler("", "", false, "", "https://app.example.com")
	body := installerGet(t, h, "codex", "")
	if !strings.Contains(body, "https://app.example.com/api/v1") {
		t.Errorf("expected embedded URL to use server public URL; body excerpt:\n%s",
			body[:min(len(body), 500)])
	}
	if strings.Contains(body, "http://127.0.0.1:") {
		t.Errorf("embedded URL should not fall back to request host when server public URL is configured")
	}
}

func TestInstallerEmbedsRequestHost(t *testing.T) {
	// When not via the relay, the resolved URL should mirror the request host so
	// agents on the user's box talk to the daemon directly.
	h := NewInstallerHandler("", "", true, "", "")
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
