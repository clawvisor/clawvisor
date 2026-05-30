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
		"install_mode\": \"host\"",
		"## 2. Mint a connection request",
		"Do not reuse a token",
		"claim=ABCDEFGHIJ",
		"wait=true",
		"## 3. Persist the token",
		"~/.clawvisor/agents/claude-code.json",
		"## 4. Configure Claude Code",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_CUSTOM_HEADERS",
		"X-Clawvisor-Agent-Token",
		"Dashboard answers",
		"Claude Code routing scope: alias",
		"The user chose **scoped routing**",
		"Leave permissions unchanged",
		"## 5. Offer a shell alias",
		"claude-cv()",
		"## 6. Connectivity smoke test",
		"/api/skill/catalog",
		"-o /dev/null && echo OK || echo REVOKED",
		"## 7. Save an uninstall reference",
		"## 8. Self-uninstall automatically",
		"rm -f ~/.claude/commands/clawvisor-install.md",
		"rm -rf ~/.codex/skills/clawvisor-install",
	)
	if strings.Contains(body, "Check for an existing token") {
		t.Errorf("installer should not offer to reuse an existing token")
	}
}

func TestInstallerCodexRender(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGet(t, h, "codex", "CLAIMCODE0")

	assertContainsAll(t, body,
		"# Connect Codex to Clawvisor",
		"passthrough mode",
		"codex login",
		"## 1. Check the local CLI",
		"install_mode\": \"host\"",
		"## 2. Mint a connection request",
		"Do not reuse a token",
		"claim=CLAIMCODE0",
		"## 4. Configure Codex",
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
	if strings.Contains(body, "Check for an existing token") {
		t.Errorf("installer should not offer to reuse an existing token")
	}
}

func TestInstallerHermesRender(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGet(t, h, "hermes", "")

	assertContainsAll(t, body,
		"# Connect Hermes to Clawvisor",
		"swap mode",
		"dashboard step before this skill",
		"upstream OpenAI API key",
		// Token is already on disk; the skill reads it from there.
		"already been minted",
		"~/.clawvisor/agents/hermes.json",
		"OPENAI_BASE_URL=",
		"/api/v1",
		"~/.hermes/config.yaml",
		"hermes-cv()",
		"rm -f ~/.claude/commands/clawvisor-install.md",
		"rm -rf ~/.codex/skills/clawvisor-install",
	)
	// The mint step has been moved to the dashboard's bootstrap script; the
	// skill should NOT contain a fresh `POST /api/agents/connect` block.
	for _, forbidden := range []string{
		"## 2. Mint a connection request",
		"Do not reuse a token",
		"RESPONSE=$(curl",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("Hermes skill should no longer mint a connection request: contains %q", forbidden)
		}
	}
	if strings.Contains(body, "Check for an existing token") {
		t.Errorf("installer should not offer to reuse an existing token")
	}
}

func TestInstallerHermesAnthropicProviderRender(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGetQuery(t, h, "hermes", "llm_provider=anthropic")

	assertContainsAll(t, body,
		"LLM provider: Anthropic",
		"upstream Anthropic API key",
		"ANTHROPIC_BASE_URL=http://",
		"/api \\",
		"ANTHROPIC_API_KEY=$(jq -r .token ~/.clawvisor/agents/hermes.json)",
	)
	for _, forbidden := range []string{
		"OPENAI_BASE_URL=",
		"OPENAI_API_KEY=$(jq -r .token ~/.clawvisor/agents/hermes.json)",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("Hermes Anthropic setup should not contain OpenAI command text %q", forbidden)
		}
	}
}

func TestInstallerOpenClawRender(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGet(t, h, "openclaw", "CLAIMOPEN12")

	assertContainsAll(t, body,
		"# Connect OpenClaw to Clawvisor",
		"LLM base URL",
		"Anthropic API key",
		"OpenClaw running mode: host",
		"## 1. Confirm how to run OpenClaw onboarding",
		// The bootstrap script writes the token to disk first; the configure
		// step now reads it instead of minting a new connection request.
		"already been minted",
		"~/.clawvisor/agents/openclaw.json",
		// Preflight runs before the onboard command so OpenClaw's
		// connectivity to Clawvisor is verified from OpenClaw's own
		// execution context (helper shell, docker container, etc.) before
		// the URL gets baked into OpenClaw's config.
		"## 2. Preflight: confirm OpenClaw can reach Clawvisor",
		"docker compose run --rm",
		"-H \"X-Clawvisor-Agent-Token: $CLAWVISOR_TOKEN\"",
		// dockerHostURL substitutes host.docker.internal for the resolved URL's
		// localhost host; the port comes from httptest, so don't assert on it.
		"host.docker.internal:",
		"/api/skill/catalog",
		"## 3. Point OpenClaw at Clawvisor",
		"TOKEN=$(jq -r .token ~/.clawvisor/agents/openclaw.json)",
		"openclaw-cli onboard --non-interactive",
		"--auth-choice custom-api-key",
		"--custom-base-url",
		"--custom-api-key \"$TOKEN\"",
		"--custom-compatibility anthropic",
		"docker compose run --rm openclaw-cli onboard",
		"host.docker.internal",
		"OPENCLAW_MODEL_CONTEXT_WINDOW=200000",
		"OPENCLAW_MAX_TOKENS=8192",
		"reasonable floor",
		"Claude Sonnet 4's 1M",
		"models.json",
		"contextWindow: $contextWindow",
		"maxTokens: $maxTokens",
		"rm -f ~/.claude/commands/clawvisor-install.md",
		"rm -rf ~/.codex/skills/clawvisor-install",
	)
	for _, forbidden := range []string{
		"Check for an existing token",
		"callback_secret",
		"callback secret",
		"CLAWVISOR_CALLBACK_SECRET",
		"OPENCLAW_HOOKS_URL",
		"clawvisor-webhook",
		"clawhub install",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("OpenClaw LLM-proxy setup should not contain callback/webhook text %q", forbidden)
		}
	}
}

func TestInstallerOpenClawOpenAIProviderRender(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGetQuery(t, h, "openclaw", "claim=CLAIMOPEN12&llm_provider=openai")

	assertContainsAll(t, body,
		"LLM provider: OpenAI",
		"OpenAI API key",
		"--custom-base-url \"http://",
		"/api/v1",
		"--custom-model-id \"gpt-5.4\"",
		"--custom-compatibility openai",
		// dockerHostURL renders host.docker.internal with httptest's
		// ephemeral port, so assert on the host:port-agnostic suffix.
		"host.docker.internal:",
		"/api/v1\"",
		"OPENCLAW_MODEL_ID=\"gpt-5.4\"",
		"OPENCLAW_MODEL_CONTEXT_WINDOW=1000000",
	)
	for _, forbidden := range []string{
		"--custom-model-id \"claude-sonnet-4-6\"",
		"--custom-compatibility anthropic",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("OpenClaw OpenAI setup should not contain Anthropic command text %q", forbidden)
		}
	}
}

func TestInstallerOpenClawRemoteModeSkipsLocalProbe(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGetQuery(t, h, "openclaw", "claim=CLAIMOPEN12&openclaw_mode=remote")

	assertContainsAll(t, body,
		"Dashboard answers",
		"OpenClaw running mode: remote",
		"## 1. Confirm remote OpenClaw access",
		"Do **not** probe the",
		"export OPENCLAW_REMOTE=",
		"Do **not** probe the",
		"ssh \"$OPENCLAW_REMOTE\"",
		"remote-reachable Clawvisor URL",
		// Preflight defines `OPENCLAW_CLAWVISOR_URL` (base, no path);
		// configure step appends `/api/v1` when building the onboard call.
		"export OPENCLAW_CLAWVISOR_URL",
		"$OPENCLAW_CLAWVISOR_URL/api/v1",
		"--custom-base-url",
		"--custom-api-key '$TOKEN'",
		"--custom-compatibility anthropic",
		"REMOTE_OPENCLAW_PATCH",
		"OPENCLAW_MODEL_CONTEXT_WINDOW=200000",
		"OPENCLAW_MAX_TOKENS=8192",
		// Preflight (step 2) runs from the remote host's perspective before
		// onboarding, so the URL OpenClaw will use is proven reachable
		// before it gets baked into OpenClaw's config.
		"## 2. Preflight: confirm OpenClaw can reach Clawvisor",
		"ssh \"$OPENCLAW_REMOTE\" \"curl -fsSL",
		"$OPENCLAW_CLAWVISOR_URL/api/skill/catalog",
	)

	for _, forbidden := range []string{
		"## 1. Probe the environment",
		"`docker ps",
		"check `~/.openclaw/` on the host",
		"# Host install:",
		"Both OpenClaw and Clawvisor in Docker on same host",
		"Check for an existing token",
		"Preferred task auto-approval default",
		"callback_secret",
		"callback secret",
		"CLAWVISOR_CALLBACK_SECRET",
		"OPENCLAW_HOOKS_URL",
		"clawvisor-webhook",
		"clawhub install",
		"CLAWVISOR_AGENT_TOKEN",
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
