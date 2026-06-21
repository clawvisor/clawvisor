package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestInstallerHermesRender(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGet(t, h, "hermes", "abcDEF1234")

	assertContainsAll(t, body,
		"# Connect Hermes to Clawvisor",
		"swap mode",
		// Step 1: skill registers the agent via claim auto-approve.
		"## 1. Register and persist the token",
		"claim=abcDEF1234",
		"/api/agents/connect",
		`"$TOKEN_FILE"`,
		// Step 2: detect provider — read env + ~/.hermes/config.yaml safely
		// (python yaml extracts only base_url, never the api_key).
		"## 2. Detect the upstream LLM provider",
		"python3 -c \"import yaml",
		"model.base_url",
		"DO NOT `cat`, `grep`, `head`, or `tail`",
		// Detection branches by signal (env + config base_url host).
		`[ -n "$ANTHROPIC_API_KEY" ]`,
		`[ -n "$OPENAI_API_KEY" ]`,
		"*anthropic.com*",
		"*openai.com*",
		// Re-install signal: if Hermes's existing base_url points at a
		// Clawvisor instance, the trailing path tells us which provider
		// was picked last time.
		"*/api/v1*",
		"*/api|*/api/",
		// model.default name pattern — actively-used model is the
		// strongest single hint.
		`DEFAULT=$(python3 -c "import yaml`,
		`(d.get('model') or {}).get('default')`,
		"anthropic/*|*claude*",
		"openai/*|*gpt*|*o1-*|*o3-*|*o4-*",
		// HARD CONSTRAINT: the helper must ask the user and wait for a
		// reply. The earlier shape of this skill let helpers default
		// silently — we have a real bug report from the field on that.
		"HARD CONSTRAINT: you must not pick `$PROVIDER` yourself",
		"DO NOT decide silently",
		"Wait for the user's reply before going further",
		// Case block derives every per-provider variable at runtime.
		`case "$PROVIDER" in`,
		"PROVIDER_LABEL='Anthropic'",
		"PROVIDER_LABEL='OpenAI'",
		"BASE_PATH='/api'",
		"BASE_PATH='/api/v1'",
		"BASE_ENV='ANTHROPIC_BASE_URL'",
		"BASE_ENV='OPENAI_BASE_URL'",
		"KEY_ENV='ANTHROPIC_API_KEY'",
		"KEY_ENV='OPENAI_API_KEY'",
		"KEY_PREFIX='sk-ant-'",
		"KEY_PREFIX='sk-'",
		// Step 3: credential check uses $PROVIDER, not a baked string.
		"## 3. Ensure a vaulted upstream key exists",
		"/api/runtime/llm-credentials",
		`select(.provider==$p`,
		`### 3.a. Vault a $PROVIDER_LABEL API key`,
		// Step 4-6: probe, preflight, configure all use shell vars.
		"## 4. Probe the Hermes deployment",
		"$HERMES_MODE",
		"## 5. Preflight: confirm Hermes can reach Clawvisor",
		"/api/skill/catalog",
		"## 6. Configure Hermes",
		// env-var snippet uses `env NAME=VALUE` with dynamic names from
		// $BASE_ENV / $KEY_ENV — no provider literal.
		`"$BASE_ENV=$CLAWVISOR_LLM_URL$BASE_PATH"`,
		`"$KEY_ENV=$TOKEN"`,
		"~/.hermes/config.yaml",
		"hermes-cv",
		// Config-file mode: secrets in ~/.hermes/.env (Hermes docs are
		// explicit — config.yaml carries non-secret config only), with
		// ${HERMES_CV_API_KEY} substitution into config.yaml.
		"~/.hermes/.env",
		"HERMES_CV_API_KEY=$TOKEN",
		`chmod 600 ~/.hermes/.env`,
		// Backslash-escaped in the heredoc so bash doesn't expand it; the
		// file that actually lands on disk is `api_key: "${HERMES_CV_API_KEY}"`,
		// which Hermes resolves from ~/.hermes/.env at runtime.
		`api_key: "\${HERMES_CV_API_KEY}"`,
		// Setup-shape cleanup paths.
		"rm -f ~/.claude/commands/clawvisor-setup.md",
		"rm -rf ~/.codex/skills/clawvisor-setup",
	)
	// The skill must NOT bake the provider — no static ANTHROPIC_/OPENAI_
	// env-var names, no static /api or /api/v1 path, no provider-specific
	// vault headings, no "Anthropic key" / "OpenAI key" prose in the
	// ensure-vaulted step.
	for _, forbidden := range []string{
		"already been minted",
		"dashboard step before this skill",
		"Dashboard answers",
		"rm -f ~/.claude/commands/clawvisor-install.md",
		// Provider literals that would mean we're baking the choice:
		"## 2. Ensure a OpenAI key is vaulted",
		"## 2. Ensure a Anthropic key is vaulted",
		"### 2.a. Vault a OpenAI API key",
		"### 2.a. Vault a Anthropic API key",
		"ANTHROPIC_API_KEY=\"$TOKEN\"",
		"OPENAI_API_KEY=\"$TOKEN\"",
		"ANTHROPIC_BASE_URL=http://",
		"OPENAI_BASE_URL=http://",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("Hermes one-paste skill should not contain provider-baked text %q", forbidden)
		}
	}
}

func TestInstallerOpenClawRender(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGet(t, h, "openclaw", "CLAIMOPEN12")

	assertContainsAll(t, body,
		"# Connect OpenClaw to Clawvisor",
		// Step 1: claim auto-approve register.
		"## 1. Register and persist the token",
		"claim=CLAIMOPEN12",
		"/api/agents/connect",
		`"$TOKEN_FILE"`,
		// Step 2: detect provider — reads env + the global openclaw.json
		// provider keys via jq (no per-agent models.json file exists, per
		// docs.openclaw.ai/concepts/model-providers).
		"## 2. Detect the upstream LLM provider",
		`~/.openclaw/openclaw.json`,
		`jq -r '.models.providers // {} | keys[]?'`,
		"anthropic*|claude*",
		"openai*|gpt*",
		// Strongest signal: scan EVERY provider's `api` field so a
		// non-default-named provider (`custom-host-docker-internal-25297`,
		// `local-llm`, etc.) still gives a wire-protocol hit.
		`for api in $(jq -r '.models.providers // {} | to_entries[]?.value.api`,
		"anthropic-messages)                  DETECTED=",
		"openai-completions|openai-responses) DETECTED=",
		// Default model — strongest hint of what's actively used.
		`DEFAULT_MODEL=$(jq -r '.models.default`,
		`DEFAULT_PROVIDER="${DEFAULT_MODEL%%/*}"`,
		`DEFAULT_API=$(jq -r --arg p "$DEFAULT_PROVIDER" '.models.providers[$p].api`,
		// HARD CONSTRAINT: ask the user, don't pick silently. Lock this
		// against regression — we got a real bug report from the field.
		"HARD CONSTRAINT: you must not pick `$PROVIDER` yourself",
		"DO NOT decide silently",
		"Wait for the user's reply before going further",
		// Case block (shared with Hermes via providerCaseBlock). OPENCLAW_API
		// is the on-disk `api` field value (distinct from the
		// --custom-compatibility flag value that onboard takes).
		`case "$PROVIDER" in`,
		"PROVIDER_LABEL='Anthropic'",
		"PROVIDER_LABEL='OpenAI'",
		"MODEL_ID='claude-sonnet-4-6'",
		"MODEL_ID='gpt-5.4'",
		"CONTEXT_WINDOW=200000",
		"CONTEXT_WINDOW=1000000",
		"OPENCLAW_API='anthropic-messages'",
		"OPENCLAW_API='openai-completions'",
		// Step 3: ensure vaulted key (provider-agnostic title).
		"## 3. Ensure a vaulted upstream key exists",
		"/api/runtime/llm-credentials",
		`### 3.a. Vault a $PROVIDER_LABEL API key`,
		// Step 4-5: probe + preflight. Probe uses bare `openclaw` binary
		// (not `openclaw-cli`).
		"## 4. Probe the OpenClaw deployment",
		`command -v openclaw >/dev/null 2>&1`,
		"$OPENCLAW_MODE",
		"## 5. Preflight: confirm OpenClaw can reach Clawvisor",
		"-H \"X-Clawvisor-Agent-Token: $CLAWVISOR_TOKEN\"",
		"host.docker.internal:",
		"/api/skill/catalog",
		// Step 6: configure via `openclaw config set models.providers.clawvisor`.
		// Onboard is for first-time auth, not provider registration —
		// docs.openclaw.ai/cli/config covers the merge pattern.
		"## 6. Point OpenClaw at Clawvisor",
		"PROVIDER_JSON=$(jq -n",
		`--arg baseUrl "$CLAWVISOR_LLM_URL$BASE_PATH"`,
		`--arg apiKey  "$TOKEN"`,
		`--arg api     "$OPENCLAW_API"`,
		`--arg modelId "$MODEL_ID"`,
		`--argjson contextWindow "$CONTEXT_WINDOW"`,
		"--argjson maxTokens 8192",
		`openclaw config set models.providers.clawvisor "$PROVIDER_JSON" --strict-json --merge`,
		`docker exec "$OPENCLAW_CONTAINER" openclaw config set models.providers.clawvisor`,
		// Remote uses $(cat) substitution to pipe the JSON over stdin.
		`ssh "$OPENCLAW_REMOTE" 'openclaw config set models.providers.clawvisor "$(cat)" --strict-json --merge'`,
		"export OPENCLAW_CLAWVISOR_URL",
		"$OPENCLAW_CLAWVISOR_URL$BASE_PATH",
		// Setup-shape cleanup paths.
		"rm -f ~/.claude/commands/clawvisor-setup.md",
		"rm -rf ~/.codex/skills/clawvisor-setup",
	)
	for _, forbidden := range []string{
		"already been minted",
		"Dashboard answers",
		"OpenClaw running mode: host",
		"callback_secret",
		"callback secret",
		"CLAWVISOR_CALLBACK_SECRET",
		"OPENCLAW_HOOKS_URL",
		"clawvisor-webhook",
		"clawhub install",
		"rm -f ~/.claude/commands/clawvisor-install.md",
		// The community `openclaw-cli` shim is NOT the install target;
		// the real binary is `openclaw`.
		"openclaw-cli",
		// `openclaw onboard` is the first-time auth flow, not the path for
		// adding a custom provider after install. Verified against
		// docs.openclaw.ai/cli/onboard — onboard doesn't support idempotent
		// re-runs for provider switching.
		"openclaw onboard --non-interactive",
		// Old per-agent models.json patch — there is no such file. All
		// provider config lives in the global ~/.openclaw/openclaw.json.
		"REMOTE_OPENCLAW_PATCH",
		"OPENCLAW_MODELS_JSON",
		"/agents/*/agent/models.json",
		// --custom-compatibility used the wrong value (`anthropic` instead
		// of `anthropic-messages`) — keep the door closed on that.
		"--custom-compatibility anthropic --accept-risk",
		// Per-provider literals would mean we baked the choice instead of
		// deriving from the case block.
		"## 2. Ensure a Anthropic key is vaulted",
		"## 2. Ensure a OpenAI key is vaulted",
		"OPENCLAW_MODEL_CONTEXT_WINDOW=200000",
		"OPENCLAW_MODEL_CONTEXT_WINDOW=1000000",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("OpenClaw one-paste skill should not contain %q", forbidden)
		}
	}
}

func TestInstallerOpenClawRendersAllModes(t *testing.T) {
	// In the one-paste shape, mode (host / docker / remote) is no longer
	// picked by the dashboard — the helper probes and picks at runtime, so
	// the rendered markdown must contain command variants for all three.
	// Provider is also no longer baked, so the per-mode snippets use the
	// $BASE_PATH / $OPENCLAW_API shell vars instead of hardcoded literals.
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGet(t, h, "openclaw", "CLAIMOPEN12")

	assertContainsAll(t, body,
		// Host: bare `openclaw config set` (no openclaw-cli; no onboard).
		`openclaw config set models.providers.clawvisor "$PROVIDER_JSON" --strict-json --merge`,
		// Docker: docker exec into the already-running container (probe
		// captures its name as $OPENCLAW_CONTAINER), then `openclaw config
		// set` inside the container against the mounted ~/.openclaw.
		`docker exec "$OPENCLAW_CONTAINER" openclaw config set models.providers.clawvisor`,
		"host.docker.internal:",
		// Remote: ssh + stdin-piped JSON via $(cat) so the embedded double
		// quotes don't fight with ssh argv quoting.
		`ssh "$OPENCLAW_REMOTE" 'openclaw config set models.providers.clawvisor "$(cat)" --strict-json --merge'`,
		"export OPENCLAW_CLAWVISOR_URL",
		"$OPENCLAW_CLAWVISOR_URL$BASE_PATH",
	)
}

func TestInstallerRejectsMaliciousClaim(t *testing.T) {
	// claim is interpolated into a shell-quoted curl URL inside the rendered
	// skill. Any character outside URL-safe base64 must be silently dropped
	// rather than embedded, so a paste like
	//   `/skill/install/hermes.md?claim=foo";+rm+-rf+~;+echo+"`
	// can't break out of the shell string and execute arbitrary commands.
	// Exercised against hermes because the claude-code / codex markdown
	// installers no longer exist; the rejection lives in Setup() and is
	// target-agnostic, so any surviving markdown target covers it.
	h := NewInstallerHandler("", "", true, "", "")
	bad := []string{
		`foo"; rm -rf ~; echo "`,
		"foo'$(touch /tmp/pwn)'",
		"foo bar",  // space
		"foo;bar",  // semicolon
		"foo\nbar", // newline
		"foo`id`",  // backtick
		"foo$bar",  // dollar sign
	}
	for _, claim := range bad {
		body := installerGetQuery(t, h, "hermes", "claim="+url.QueryEscape(claim))
		if strings.Contains(body, "claim="+claim) {
			t.Errorf("malicious claim %q was interpolated unescaped into rendered body", claim)
		}
	}
}

func TestInstallerAcceptsValidClaim(t *testing.T) {
	// Sanity check the positive path — a real 10-char base64 claim is
	// accepted and lands in the rendered curl URL. Exercised against hermes
	// because the claude-code / codex markdown installers no longer exist.
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGet(t, h, "hermes", "abcDEF_-09")
	if !strings.Contains(body, "claim=abcDEF_-09") {
		t.Errorf("valid claim was dropped; body excerpt:\n%s", body[:min(len(body), 500)])
	}
}

// TestInstallerMarkdownFrontmatter — Codex / Hermes / OpenClaw all reject
// skills without YAML frontmatter at load time; we caught this in the field
// after a real install, so guard against regression by asserting the exact
// shape on every surviving markdown target. Claude Code / Codex now use the
// shell installer (no frontmatter) — TestInstallerShellShape covers their
// new shape.
func TestInstallerMarkdownFrontmatter(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	for _, target := range []string{"hermes", "openclaw"} {
		body := installerGet(t, h, target, "")
		want := "---\nname: clawvisor-setup\ndescription:"
		if !strings.HasPrefix(body, want) {
			t.Errorf("[%s] missing required YAML frontmatter (want prefix %q). First 200 chars:\n%s",
				target, want, body[:min(len(body), 200)])
		}
		fenceEnd := strings.Index(body, "\n---\n")
		heading := strings.Index(body, "# Connect")
		if fenceEnd < 0 || heading < 0 || fenceEnd > heading {
			t.Errorf("[%s] frontmatter not properly closed before heading (fenceEnd=%d, heading=%d)",
				target, fenceEnd, heading)
		}
	}
}

func TestInstallerSplitsAppAndLLMURLs(t *testing.T) {
	// Control plane (registration, dashboard, credentials, skill catalog)
	// lives on the app host; the LLM proxy is a separate host. The install
	// script exports both — CLAWVISOR_APP_URL for control-plane curls and
	// CLAWVISOR_LLM_URL for what gets baked into ANTHROPIC_BASE_URL etc.
	// Conflating the two (using the proxy URL for /api/agents/connect)
	// 404s in split deployments — that's the regression this test guards.
	// Exercised against hermes; the URL-resolution logic lives in
	// resolveAppURL / resolveLLMURL and is target-agnostic.
	h := NewInstallerHandler("", "", false, "https://llm.example.com", "https://app.example.com")
	body := installerGet(t, h, "hermes", "TESTCLAIM0")
	if !strings.Contains(body, `export CLAWVISOR_APP_URL="https://app.example.com"`) {
		t.Errorf("expected CLAWVISOR_APP_URL export to use server public URL; body excerpt:\n%s",
			body[:min(len(body), 800)])
	}
	if !strings.Contains(body, `export CLAWVISOR_LLM_URL="https://llm.example.com"`) {
		t.Errorf("expected CLAWVISOR_LLM_URL export to use LLM proxy URL; body excerpt:\n%s",
			body[:min(len(body), 800)])
	}
	if !strings.Contains(body, `"$CLAWVISOR_APP_URL/api/agents/connect`) {
		t.Errorf("agent registration must target $CLAWVISOR_APP_URL, not the LLM proxy")
	}
	if strings.Contains(body, `"$CLAWVISOR_LLM_URL/api/agents/connect`) {
		t.Errorf("agent registration must NOT target $CLAWVISOR_LLM_URL")
	}
	if strings.Contains(body, "http://127.0.0.1:") {
		t.Errorf("embedded URLs should not fall back to request host when both PublicURLs are configured")
	}
}

func TestInstallerFallsBackToServerPublicURL(t *testing.T) {
	// If there is no dedicated lite-proxy URL, both AppURL and LLMURL fall
	// back to Server.PublicURL (single-host deployment).
	h := NewInstallerHandler("", "", false, "", "https://app.example.com")
	body := installerGet(t, h, "hermes", "")
	if !strings.Contains(body, `export CLAWVISOR_APP_URL="https://app.example.com"`) {
		t.Errorf("expected CLAWVISOR_APP_URL export to use server public URL; body excerpt:\n%s",
			body[:min(len(body), 800)])
	}
	if !strings.Contains(body, `export CLAWVISOR_LLM_URL="https://app.example.com"`) {
		t.Errorf("expected CLAWVISOR_LLM_URL to fall back to server public URL when no proxy URL is set; body excerpt:\n%s",
			body[:min(len(body), 800)])
	}
	if strings.Contains(body, "http://127.0.0.1:") {
		t.Errorf("embedded URL should not fall back to request host when server public URL is configured")
	}
}

func TestInstallerEmbedsRequestHost(t *testing.T) {
	// When neither public URL is configured, both env vars fall through to
	// the request host so agents on the user's box talk to the daemon directly.
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGet(t, h, "hermes", "")
	// httptest binds an ephemeral 127.0.0.1 host; both exports should embed it.
	if !strings.Contains(body, `export CLAWVISOR_APP_URL="http://127.0.0.1:`) {
		t.Errorf("expected request host to be embedded as CLAWVISOR_APP_URL export, body excerpt:\n%s", body[:min(len(body), 800)])
	}
	if !strings.Contains(body, `export CLAWVISOR_LLM_URL="http://127.0.0.1:`) {
		t.Errorf("expected request host to be embedded as CLAWVISOR_LLM_URL export, body excerpt:\n%s", body[:min(len(body), 800)])
	}
}

// ── Shell installer tests (claude-code, codex) ───────────────────────────────

// installerGetShell hits the .sh installer endpoint and returns the rendered
// shell body. Mirrors installerGet but asserts the application/x-sh content
// type and ".sh" path suffix.
func installerGetShell(t *testing.T, h *InstallerHandler, target, claim string) string {
	t.Helper()
	path := "/skill/install/" + target + ".sh"
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
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-sh") {
		t.Fatalf("expected application/x-sh, got %q", ct)
	}
	return string(body)
}

// TestInstallerClaudeCodeShell — sanity check the deterministic shell one-
// liner the dashboard now hands out. Asserts the script's structural anchors
// — shebang, preflight, mint, the make-default question, BOTH apply
// branches (default-everywhere writes settings.json + permission rules;
// alias-only writes a claude-cv() function with optional
// --dangerously-skip-permissions), and a mode-aware uninstall doc.
func TestInstallerClaudeCodeShell(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGetShell(t, h, "claude-code", "ABCDEFGHIJ")
	assertContainsAll(t, body,
		"#!/bin/sh",
		"set -eu",
		"need curl",
		"need jq",
		// Flag parsing — same flags as codex now.
		"--default-everywhere",
		"--alias-only",
		"--yolo",
		"--no-yolo",
		// Mint with claim — claim auto-approves on the daemon, no second click.
		"claim=ABCDEFGHIJ",
		"/api/agents/connect",
		// Token is persisted to ~/.clawvisor/agents/<name>.json with chmod 600.
		"~/.clawvisor/agents",
		"chmod 600",
		// Smoke test — daemon must accept the freshly minted token.
		"/api/skill/catalog",
		// Default-vs-alias prompt + skip-permissions prompt (TUI labels).
		"prompt_choice",
		"How should Clawvisor route your Claude Code calls?",
		"How should 'claude-cv' handle Claude Code's permission prompts?",
		"--dangerously-skip-permissions",
		// Non-interactive escape hatch.
		"--no-tui",
		"CLAWVISOR_NO_TUI",
		// Default-everywhere branch: env keys + permission rules into
		// ~/.claude/settings.json.
		"~/.claude/settings.json",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_CUSTOM_HEADERS",
		"X-Clawvisor-Agent-Token",
		"Bash(curl *https://relay.clawvisor.com/*)",
		// Alias-only branch: writes claude-cv() to the rc file with the
		// optional skip flag.
		"claude-cv()",
		// Uninstall doc with name substitution + mode-specific copy.
		"uninstall-claude-code.md",
		"default-everywhere install",
		"alias-only install",
	)
}

// TestInstallerCodexShell — sanity check the codex shell installer. Asserts
// the provider-block append, the smoke test, both apply branches' flag
// parsing, and the uninstall doc.
//
// The slug + display are baked into shell `SLUG='…'` / `DISPLAY='…'`
// assignments at the top of the codex section. Everything downstream
// references those shell vars (`[model_providers.$SLUG]`, `name = "$DISPLAY"`,
// etc.), so test anchors hit the assignment line plus the literal shell-var
// form. TestInstallerCodexShellProviderSlugByEnv exercises the prod/staging
// mapping.
func TestInstallerCodexShell(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGetShell(t, h, "codex", "CLAIMCODE0")
	assertContainsAll(t, body,
		"#!/bin/sh",
		"set -eu",
		// Flag parsing for non-interactive runs.
		"--default-everywhere",
		"--alias-only",
		"--yolo",
		"--no-yolo",
		"--no-tui",
		"CLAWVISOR_NO_TUI",
		// Arrow-key TUI prompts (with prompt_yn fallback in common.sh.tmpl).
		"prompt_choice",
		"How should Clawvisor route your codex calls?",
		"How should 'codex-cv' handle Codex's approval prompts and sandbox?",
		// Mint + persist + smoke test.
		"claim=CLAIMCODE0",
		"/api/agents/connect",
		"~/.clawvisor/agents",
		"chmod 600",
		"/api/skill/catalog",
		// Slug + display baked in (dev slug for the empty-LLMURL handler).
		"SLUG='clawvisor-dev'",
		"DISPLAY='Clawvisor (dev)'",
		// Provider block — slug/display interpolated via the heredoc at runtime.
		"[model_providers.$SLUG]",
		`name = "$DISPLAY"`,
		`wire_api = "responses"`,
		`requires_openai_auth = true`,
		"X-Clawvisor-Agent-Token =",
		// Default-everywhere branch: prepends model_provider and exports
		// CLAWVISOR_AGENT_TOKEN.
		`model_provider = \"$SLUG\"`,
		"export CLAWVISOR_AGENT_TOKEN",
		// Alias-only branch: writes a codex-cv() function with the slug
		// interpolated into the -c flag.
		"codex-cv()",
		`-c model_provider="$SLUG"`,
		"--dangerously-bypass-approvals-and-sandbox",
		// Uninstall doc.
		"uninstall-codex.md",
	)
}

// TestInstallerCodexShellProviderSlugByEnv locks the env-aware slug mapping
// for the codex shell installer. Mirrors the deleted markdown variant so
// prod / staging / dev installs still coexist in one ~/.codex/config.toml.
func TestInstallerCodexShellProviderSlugByEnv(t *testing.T) {
	cases := []struct {
		name           string
		llmProxyURL    string
		wantSlug       string
		wantDisplay    string
		wantNotPresent []string
	}{
		{
			name:        "production",
			llmProxyURL: "https://llm.clawvisor.com",
			wantSlug:    "clawvisor",
			wantDisplay: "Clawvisor",
			wantNotPresent: []string{
				"[model_providers.clawvisor-staging]",
				"[model_providers.clawvisor-dev]",
			},
		},
		{
			name:        "staging",
			llmProxyURL: "https://llm.staging.clawvisor.com",
			wantSlug:    "clawvisor-staging",
			wantDisplay: "Clawvisor (staging)",
			wantNotPresent: []string{
				"[model_providers.clawvisor-dev]",
			},
		},
		{
			name:        "dev_localhost_default",
			llmProxyURL: "",
			wantSlug:    "clawvisor-dev",
			wantDisplay: "Clawvisor (dev)",
			wantNotPresent: []string{
				"[model_providers.clawvisor-staging]",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewInstallerHandler("", "", true, tc.llmProxyURL, "")
			body := installerGetShell(t, h, "codex", "CLAIMCODE0")
			// Slug + display are baked into shell assignments at the top of
			// the codex section; downstream references via $SLUG / $DISPLAY.
			assertContainsAll(t, body,
				"SLUG='"+tc.wantSlug+"'",
				"DISPLAY='"+tc.wantDisplay+"'",
			)
			for _, np := range tc.wantNotPresent {
				// Forbidden envs: a wrong-env slug should not appear in any
				// position — assignment line, comment, or runtime literal.
				if strings.Contains(body, np) {
					t.Errorf("unexpected %q in body (should only appear for that env)", np)
				}
				wrongSlug := strings.TrimPrefix(strings.TrimSuffix(np, "]"), "[model_providers.")
				if strings.Contains(body, "SLUG='"+wrongSlug+"'") {
					t.Errorf("unexpected SLUG='%s' assignment for this env", wrongSlug)
				}
			}
		})
	}
}

// TestInstallerMarkdownReturns410 — old paste blobs that hit
// /skill/install/<self-install-target>.md must return 410 Gone with a body
// pointing at the new .sh URL, so dashboards that haven't updated their URL
// builder surface the cutover cleanly instead of silently 200ing an
// LLM-driven flow that no longer exists.
func TestInstallerMarkdownReturns410(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /skill/install/{target}", h.Setup)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for _, target := range []string{"claude-code", "codex"} {
		resp, err := http.Get(srv.URL + "/skill/install/" + target + ".md")
		if err != nil {
			t.Fatalf("[%s] GET: %v", target, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusGone {
			t.Errorf("[%s] expected 410 Gone, got %d", target, resp.StatusCode)
		}
		if !strings.Contains(string(body), "/skill/install/"+target+".sh") {
			t.Errorf("[%s] 410 body should point at the .sh replacement; got: %s", target, body)
		}
	}
}

// TestUninstallReturnsGoneWithLocalFilePointer — the deprecated markdown
// installer dropped a `/clawvisor-uninstall` slash command on disk that
// fetched the uninstall skill from /skill/uninstall/<target>.md. The route
// no longer serves a real uninstaller (the shell installer writes the
// revert recipe directly to ~/.clawvisor/uninstall-<target>.md), but stale
// slash commands still hit this path. Returning 410 with a pointer at the
// local file is friendlier than a bare 404.
func TestUninstallReturnsGoneWithLocalFilePointer(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /skill/uninstall/{target}", h.Uninstall)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for _, target := range []string{"claude-code", "codex", "claude-code.md"} {
		resp, err := http.Get(srv.URL + "/skill/uninstall/" + target)
		if err != nil {
			t.Fatalf("[%s] GET: %v", target, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusGone {
			t.Errorf("[%s] expected 410, got %d", target, resp.StatusCode)
		}
		if !strings.Contains(string(body), "~/.clawvisor/uninstall-") {
			t.Errorf("[%s] body should point at the local uninstall doc; got: %s", target, body)
		}
	}
}

// TestInstallerBareURLRedirectsByTarget — the no-extension form historically
// redirected to .md, but claude-code and codex now serve .sh (their .md
// route returns 410). A bare URL for those targets must redirect to .sh so
// stale bookmarks / integrations that omit the extension don't land on the
// 410 page. Hermes / OpenClaw still go to .md.
func TestInstallerBareURLRedirectsByTarget(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /skill/install/{target}", h.Setup)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := &http.Client{
		// Capture the redirect target without following it.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	cases := []struct{ target, wantExt string }{
		{"claude-code", ".sh"},
		{"codex", ".sh"},
		{"hermes", ".md"},
		{"openclaw", ".md"},
	}
	for _, tc := range cases {
		resp, err := client.Get(srv.URL + "/skill/install/" + tc.target + "?claim=ABC")
		if err != nil {
			t.Fatalf("[%s] GET: %v", tc.target, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMovedPermanently {
			t.Errorf("[%s] expected 301, got %d", tc.target, resp.StatusCode)
			continue
		}
		want := "/skill/install/" + tc.target + tc.wantExt + "?claim=ABC"
		if got := resp.Header.Get("Location"); got != want {
			t.Errorf("[%s] redirect Location = %q, want %q", tc.target, got, want)
		}
	}
}

// TestInstallerMarkdownGoneBodyHasRealHost — the 410 body for stale .md
// URLs has to interpolate the real host so users can copy-paste the
// recovery snippet directly. A literal `<host>` placeholder leaves them
// guessing which daemon endpoint to point at.
func TestInstallerMarkdownGoneBodyHasRealHost(t *testing.T) {
	h := NewInstallerHandler("", "", false, "", "https://app.example.com")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /skill/install/{target}", h.Setup)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for _, target := range []string{"claude-code", "codex"} {
		resp, err := http.Get(srv.URL + "/skill/install/" + target + ".md")
		if err != nil {
			t.Fatalf("[%s] GET: %v", target, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusGone {
			t.Errorf("[%s] expected 410, got %d", target, resp.StatusCode)
		}
		if strings.Contains(string(body), "<host>") {
			t.Errorf("[%s] body still contains literal `<host>` placeholder; got: %s", target, body)
		}
		want := "https://app.example.com/skill/install/" + target + ".sh"
		if !strings.Contains(string(body), want) {
			t.Errorf("[%s] body should contain %q; got: %s", target, want, body)
		}
	}
}

// TestInstallerShellNotAvailableForMarkdownTargets — guard the inverse: the
// .sh route is defined only for the two self-install targets, so hitting
// /skill/install/hermes.sh (or openclaw.sh) must 404 rather than fall
// through to a markdown renderer with the wrong content-type.
func TestInstallerShellNotAvailableForMarkdownTargets(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /skill/install/{target}", h.Setup)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for _, target := range []string{"hermes", "openclaw"} {
		resp, err := http.Get(srv.URL + "/skill/install/" + target + ".sh")
		if err != nil {
			t.Fatalf("[%s] GET: %v", target, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("[%s] expected 404 on .sh, got %d", target, resp.StatusCode)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
