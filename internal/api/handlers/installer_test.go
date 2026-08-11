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
	// Exercises the routed (proxy) template specifically — alias setup, key
	// waiting, custom headers. That is now the opt-in variant, so ask for it
	// explicitly; the default is covered by TestInstallerDefaultIsSkillOnly.
	body := installerGetShellQuery(t, h, "claude-code", "claim=ABCDEFGHIJ&route=proxy")
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
		// A fresh dashboard claim must always reach the connect endpoint and
		// mint a new token; an older local token can never short-circuit it.
		"Every installer invocation creates a new agent token",
		"A claim is single-use",
		// End-to-end smoke test fires FIRST — passthrough auth (claude
		// login) flows through Clawvisor untouched, so a working round-
		// trip means we're done regardless of vault state. Only fall
		// back to the vault-and-wait dance when the smoke test fails AND
		// no key is vaulted. Skips gracefully when claude isn't on $PATH.
		"command -v claude",
		"claude -p \"respond with the word OK\"",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_CUSTOM_HEADERS",
		"X-Clawvisor-Agent-Token: $TOKEN",
		// Diagnostic + recovery path: vault-page URL + poll for the key
		// to land, then retry the smoke test once it does.
		"have_anthropic_key",
		"/api/runtime/llm-credentials?agent_id=$AGENT_ID",
		`provider=="anthropic"`,
		"/dashboard/keys/anthropic?for=$AGENT_ID",
		"Waiting for a key",
		// Default-vs-alias prompt + alias-name prompt + skip-permissions prompt.
		"prompt_choice",
		"How should Clawvisor route your Claude Code calls?",
		"prompt_text",
		"What should the alias be called?",
		`"claude-cv"`,
		"--dangerously-skip-permissions",
		// Non-interactive escape hatches.
		"--no-tui",
		"CLAWVISOR_NO_TUI",
		"--alias-name=",
		// Default-everywhere branch: env keys + permission rules into
		// ~/.claude/settings.json.
		"~/.claude/settings.json",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_CUSTOM_HEADERS",
		"X-Clawvisor-Agent-Token",
		"Bash(curl *https://relay.clawvisor.com/*)",
		// Alias-only branch: writes claude-cv() to the rc file with the
		// optional skip flag.
		"$CV_ALIAS_NAME()",
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
	// Routed (proxy) template — now the opt-in variant, so request it
	// explicitly. The default is covered by TestInstallerDefaultIsSkillOnly.
	body := installerGetShellQuery(t, h, "codex", "claim=CLAIMCODE0&route=proxy")
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
		"--alias-name=",
		// Arrow-key TUI prompts + alias-name text prompt.
		"prompt_choice",
		"prompt_text",
		"How should Clawvisor route your codex calls?",
		`"codex-cv"`,
		// Mint + persist + token-accepted smoke test.
		"claim=CLAIMCODE0",
		"/api/agents/connect",
		"~/.clawvisor/agents",
		"chmod 600",
		"/api/skill/catalog",
		// Codex round-trip smoke test mirrors claude's: try first, on
		// failure check the vault state, on no-key offer the dashboard
		// URL and poll until a key shows up, then retry.
		"have_openai_key",
		"run_codex_smoke",
		`provider=="openai"`,
		"--output-last-message",
		"/dashboard/keys/openai?for=$AGENT_ID",
		"Waiting for a key",
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
		"$CV_ALIAS_NAME()",
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
			// The [model_providers.*] block only exists in the routed
			// template, so request route=proxy explicitly now that skill-only
			// is the default.
			body := installerGetShellQuery(t, h, "codex", "claim=CLAIMCODE0&route=proxy")
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

// TestInstallerShellAgentSlotByEnv locks the on-disk agent path namespacing:
// production keeps the bare slot (so ~/.clawvisor/agents/<name>.json — the
// common case — doesn't move) while staging and dev are env-prefixed so the
// same logical name from a different environment doesn't overwrite the prod
// token file. Mirrors the codex slug-by-env shape but covers both .sh
// installers since the wiring is in the shared common.sh.tmpl.
func TestInstallerShellAgentSlotByEnv(t *testing.T) {
	cases := []struct {
		name        string
		llmProxyURL string
		wantSlot    string
	}{
		{"production", "https://llm.clawvisor.com", "codex"},
		{"staging", "https://llm.staging.clawvisor.com", "staging/codex"},
		{"dev_localhost_default", "", "dev/codex"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewInstallerHandler("", "", true, tc.llmProxyURL, "")
			// Uses the routed template: the ~/.clawvisor/diffs/$AGENT_SLOT
			// assertions below only apply where the installer mutates provider
			// config and records a reversal diff. Skill-only mutates nothing,
			// so it has no diffs path by design.
			body := installerGetShellQuery(t, h, "codex", "claim=CLAIMCODE0&route=proxy")
			assertContainsAll(t, body,
				"AGENT_SLOT='"+tc.wantSlot+"'",
				// AGENT_NAME stays the bare logical name regardless of env —
				// it's what the daemon registers and what shows up in the
				// dashboard, so prefixing it would leak path layout into the
				// human-visible name.
				"AGENT_NAME='codex'",
				// File paths reference the slot, not the name.
				`agents/$AGENT_SLOT.json`,
				`diffs/$AGENT_SLOT`,
			)
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

// TestInstallerMarkdownGonePreservesQueryString — when a stale dashboard
// link hits /skill/install/<target>.md with a claim + agent_name in the
// query, the 410 body must echo that query back in the suggested .sh
// URL. Without this the user gets a paste-template that mints a
// different agent than the one the dashboard pre-baked, breaking the
// recovery path.
func TestInstallerMarkdownGonePreservesQueryString(t *testing.T) {
	h := NewInstallerHandler("", "", false, "", "https://app.example.com")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /skill/install/{target}", h.Setup)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/skill/install/codex.md?claim=ABCDEFGHIJ&agent_name=codex-3")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("expected 410, got %d", resp.StatusCode)
	}
	// url.Values.Encode sorts params alphabetically — agent_name before claim.
	want := "https://app.example.com/skill/install/codex.sh?agent_name=codex-3&claim=ABCDEFGHIJ"
	if !strings.Contains(string(body), want) {
		t.Errorf("410 body should preserve the original query string; want substring %q, got: %s", want, body)
	}
	if strings.Contains(string(body), "<your-query>") {
		t.Errorf("410 body should not fall back to the placeholder when the request had a real query string; got: %s", body)
	}
}

// TestInstallerMarkdownGoneDoesNotReflectMaliciousQuery — the 410 body is
// a paste-template ending in `| sh`. Reflecting r.URL.RawQuery directly
// would let an attacker craft a URL whose query string breaks out of the
// quoted URL and injects arbitrary shell commands. Each query param must
// be re-validated against the same regex Setup uses, and invalid ones
// silently dropped.
func TestInstallerMarkdownGoneDoesNotReflectMaliciousQuery(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /skill/install/{target}", h.Setup)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	bad := []string{
		`foo"; rm -rf ~; echo "`,
		"foo'$(touch /tmp/pwn)'",
		"foo bar",
		"foo;bar",
		"foo\nbar",
		"foo`id`",
		"foo$bar",
	}
	for _, malicious := range bad {
		path := "/skill/install/claude-code.md?claim=" + url.QueryEscape(malicious) + "&agent_name=" + url.QueryEscape(malicious)
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %q: %v", malicious, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusGone {
			t.Errorf("[%q] expected 410, got %d", malicious, resp.StatusCode)
			continue
		}
		// The malicious value should NOT appear in the body in any form —
		// raw, partially-escaped, or otherwise. The whole point of the
		// validation is to refuse to echo it back at all.
		if strings.Contains(string(body), malicious) {
			t.Errorf("[%q] body reflected the malicious value unescaped: %s", malicious, body)
		}
		// And the body should still suggest the .sh URL, with the safe-
		// validated placeholder when no valid params survived.
		if !strings.Contains(string(body), "/skill/install/claude-code.sh") {
			t.Errorf("[%q] body should still point at the .sh replacement: %s", malicious, body)
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

// installerGetShellQuery hits the .sh installer endpoint with an arbitrary
// query string and returns the rendered body. Mirrors installerGetShell but
// lets the caller pass route=... etc.
func installerGetShellQuery(t *testing.T, h *InstallerHandler, target, query string) string {
	t.Helper()
	path := "/skill/install/" + target + ".sh"
	if query != "" {
		path += "?" + query
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

// TestInstallerDefaultIsSkillOnly — the DEFAULT generated script for each
// self-install target registers the agent WITHOUT touching where its model
// traffic goes. These routes are unauthenticated, so the installer cannot know
// whether the installing user is entitled to proxy-lite; baking routing by
// default hands an unentitled user a seat that 403s on its first model call.
func TestInstallerDefaultIsSkillOnly(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")

	claude := installerGetShell(t, h, "claude-code", "ABCDEFGHIJ")
	if strings.Contains(claude, "ANTHROPIC_BASE_URL=") {
		t.Errorf("default claude-code script must NOT export ANTHROPIC_BASE_URL; body:\n%s", claude)
	}
	assertContainsAll(t, claude, "skill-only", "/api/agents/connect")

	codex := installerGetShell(t, h, "codex", "CLAIMCODE0")
	if strings.Contains(codex, "OPENAI_BASE_URL=") || strings.Contains(codex, "base_url = \"$LLM_URL") {
		t.Errorf("default codex script must NOT bake a provider base URL; body:\n%s", codex)
	}
	assertContainsAll(t, codex, "skill-only", "/api/agents/connect")
}

// TestSkillOnlyInstallsTheSkill — the whole point of a skill-only install. Until
// this existed the script minted a token and stopped, so the harness ended up
// with a credential on disk and no idea Clawvisor existed: the agent never
// routed tool calls through the gateway because nothing had told it to. The
// installer must leave the skill on disk AND make the env it declares
// (CLAWVISOR_URL, CLAWVISOR_AGENT_TOKEN) resolvable by the harness.
func TestSkillOnlyInstallsTheSkill(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")

	claude := installerGetShell(t, h, "claude-code", "ABCDEFGHIJ")
	assertContainsAll(t, claude,
		// Fetched per-target: the Claude Code render carries permission-glob
		// guidance that would mislead any other harness.
		"/skill/SKILL.md?target=claude-code",
		".claude/skills/clawvisor",
		"CLAWVISOR_URL",
		"CLAWVISOR_AGENT_TOKEN",
		// Without the allow-rules the skill prompts on every gateway call,
		// which is "installed" but not usable.
		"permissions.allow",
		// ...and the rule has to name a host, not a variable. Permission globs
		// are matched literally and never expanded, so a rule written as the
		// literal "Bash(curl *$CLAWVISOR_URL/*)" matches no command that will
		// ever run: every gateway call still prompts, which is the same
		// "installed but unusable" failure the assertion above guards. jq
		// interpolation also makes the rule track the environment being
		// installed against -- staging renders staging, production renders
		// production -- so this must stay an interpolation, not a fixed host.
		`"Bash(curl *\($url)/*)"`,
	)

	codex := installerGetShell(t, h, "codex", "CLAIMCODE0")
	assertContainsAll(t, codex,
		"/skill/SKILL.md?target=codex",
		".codex/skills/clawvisor",
		"CLAWVISOR_URL",
		"CLAWVISOR_AGENT_TOKEN",
	)
}

// TestSkillOnlyProtectsTheTokenAtRest — the agent token is written into
// ~/.claude/settings.json so Claude Code can read it as env. The same secret is
// already chmod 600 in ~/.clawvisor/agents/, and the routed installer leaves the
// settings copy at the ambient umask (commonly 0644). Skill-only must not repeat
// that: umask before the write covers the temp file too, which a trailing chmod
// would not.
func TestSkillOnlyProtectsTheTokenAtRest(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	claude := installerGetShell(t, h, "claude-code", "ABCDEFGHIJ")

	assertContainsAll(t, claude, "umask 077", "chmod 600")

	// The token must never be written BY VALUE into a shell rc, where it would
	// sit at the ambient umask forever. Codex reads it by reference instead.
	//
	// Scoped to the `export` form on purpose: passing the token as a
	// per-invocation process env (as the dry run does when it spawns the
	// harness) is fine and necessary — it is never persisted. An assertion
	// broad enough to catch that is a false positive, not a finding.
	codex := installerGetShell(t, h, "codex", "CLAIMCODE0")
	if strings.Contains(codex, `export CLAWVISOR_AGENT_TOKEN="$TOKEN"`) {
		t.Error("codex skill-only must export the token by reference (jq from the 0600 file), not by value into an rc")
	}
	assertContainsAll(t, codex, "jq -r .token")
}

// TestSkillOnlyDryRunIsOptIn — the dry run spawns the harness, so it must never
// fire unattended. `curl | sh` with no terminal has to fall through to skipping
// it; a default that spawns an agent in CI would be a nasty surprise.
func TestSkillOnlyDryRunIsOptIn(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	for _, tc := range []struct{ target, claim string }{
		{"claude-code", "ABCDEFGHIJ"},
		{"codex", "CLAIMCODE0"},
	} {
		body := installerGetShell(t, h, tc.target, tc.claim)
		assertContainsAll(t, body,
			"cv_dry_run_prompt",
			// Gated on an interactive terminal, not on a bare default.
			"[ -r /dev/tty ]",
			// Load-bearing: under `curl | sh` the harness would otherwise read
			// the rest of this script from the pipe as its prompt.
			"</dev/null",
			// The out-of-scope half is the only thing that proves enforcement.
			"OUTSIDE",
		)
		if !strings.Contains(body, "--dry-run") {
			t.Errorf("%s: dry run must be pre-answerable with --dry-run for non-interactive installs", tc.target)
		}
	}
}

// TestClaudeDryRunUsesAutoPermissionMode protects the noninteractive Claude
// invocation. `claude -p` cannot show a permission prompt, so the default mode
// rejects the skill's authenticated curl as `Contains simple_expansion` when
// it sees $CLAWVISOR_AGENT_TOKEN. Auto mode retains Claude Code's safety
// classifier and permits the Clawvisor-bound request; bypassPermissions would
// be unnecessarily broad.
func TestClaudeDryRunUsesAutoPermissionMode(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGetShell(t, h, "claude-code", "ABCDEFGHIJ")
	assertContainsAll(t, body,
		"cv_claude_transcript",
		"CV_DRY_PROMPT=$(cv_dry_run_prompt)",
		`echo "Initial prompt:"`,
		`printf '%s\n\n' "$CV_DRY_PROMPT"`,
		"--permission-mode auto --verbose",
		`--output-format stream-json "$CV_DRY_PROMPT"`,
		`| cv_claude_transcript | tee "$CV_DRY_OUT"`,
	)
	if strings.Contains(body, "claude -p --dangerously-skip-permissions") {
		t.Error("Claude dry run must not bypass permission checks")
	}
}

// TestCodexDryRunUsesCompactJSONTranscript prevents `codex exec` from dumping
// startup metadata, hook chatter, the prompt twice, and the full SKILL.md into
// installer output. The JSONL formatter retains agent/tool turns and the
// machine-readable verdict.
func TestCodexDryRunUsesCompactJSONTranscript(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGetShell(t, h, "codex", "ABCDEFGHIJ")
	assertContainsAll(t, body,
		"cv_codex_transcript",
		"codex exec --json --ephemeral",
		`| cv_codex_transcript | tee "$CV_DRY_OUT"`,
		"load Clawvisor skill",
	)
}

// TestDryRunVerdictIsNotTheExitCode — the dry run used to set CV_DRY_STATUS=ok
// purely on the harness exiting 0, then print "the in-scope request should have
// succeeded". An agent that cannot make a single call still exits 0 after
// explaining why, so a run in which nothing happened was reported as a pass.
// The verdict has to come from a line the agent prints, and the absence of that
// line has to be its own outcome rather than collapsing into success.
func TestDryRunVerdictIsNotTheExitCode(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	for _, tc := range []struct{ target, claim string }{
		{"claude-code", "ABCDEFGHIJ"},
		{"codex", "CLAIMCODE0"},
	} {
		body := installerGetShell(t, h, tc.target, tc.claim)
		assertContainsAll(t, body,
			// Captured as well as streamed, so the verdict can be read back.
			"cv_dry_run_verdict",
			"CLAWVISOR_DRY_RUN: PASS",
			"CLAWVISOR_DRY_RUN: FAIL",
			// Ran but said nothing is distinct from passed and from skipped.
			"inconclusive",
			// The prompt has to stay narrow. "help me work out why" turned a
			// failed first call into an open-ended debugging session, and
			// SKILL.md's troubleshooting advice sent it re-fetching the skill
			// over a URL the allowlist does not cover.
			"Do nothing else",
			"Do not check the skill version",
			"stop immediately",
			"Dry run results:",
			"In-scope call:",
			"Out-of-scope call:",
			"do not paste raw JSON responses",
		)
		// The old reassurance must not survive: it asserted an outcome the
		// script had not established. Matched on the exact banner rather than
		// the phrase, which also appears in the comment explaining this.
		if strings.Contains(body, "Dry run finished. Read the summary above") {
			t.Errorf("%s: dry run still claims success it has not verified", tc.target)
		}
	}
}

// TestInstallerNeverReusesALocalAgentToken protects the security-sensitive
// bootstrap invariant: a newly minted dashboard claim must result in a newly
// minted agent token. The old installer first authenticated the token already
// present in ~/.clawvisor/agents/<slot>.json and silently reused it, so the
// fresh claim never reached /api/agents/connect at all.
func TestInstallerNeverReusesALocalAgentToken(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	for _, v := range shellInstallerVariants() {
		t.Run(v.name, func(t *testing.T) {
			body := installerGetShellQuery(t, h, v.target, v.query)
			assertContainsAll(t, body,
				"Every installer invocation creates a new agent token",
				"/api/agents/connect?claim=",
			)
			for _, stale := range []string{
				"Reusing existing agent token",
				"EXISTING_TOKEN",
				"EXISTING_JSON",
			} {
				if strings.Contains(body, stale) {
					t.Errorf("installer still contains stale-token reuse path %q", stale)
				}
			}
		})
	}
}

// TestCodexDryRunEnablesItsNetworkSandbox pins the invocation-level sandbox
// settings needed by the check itself. `codex exec` defaults to read-only;
// merely writing sandbox_workspace_write.network_access=true to config does
// not help unless this invocation actually selects workspace-write.
func TestCodexDryRunEnablesItsNetworkSandbox(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGetShell(t, h, "codex", "CLAIMCODE0")
	assertContainsAll(t, body,
		"codex exec --json --ephemeral --sandbox workspace-write",
		"-c sandbox_workspace_write.network_access=true",
	)
}

// TestDryRunRecognizesLocalOnlyCatalogs prevents the preflight from treating
// "no cloud services" as "no services". Local daemon services appear after
// that message and are valid task targets, so presence is determined from the
// catalog's service headings instead.
func TestDryRunRecognizesLocalOnlyCatalogs(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGetShell(t, h, "codex", "CLAIMCODE0")
	assertContainsAll(t, body,
		"grep -q '^## '",
		"valid local-only",
	)
}

// TestCodexSkillOnlyAsksBeforeRelaxingSandbox — persisting
// network_access = true weakens the sandbox for every workspace-write command,
// not just Clawvisor's. It must be consented to, and declining must tell the
// user what to do instead rather than leaving them with a silently broken skill.
func TestCodexSkillOnlyAsksBeforeRelaxingSandbox(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	codex := installerGetShell(t, h, "codex", "CLAIMCODE0")

	assertContainsAll(t, codex,
		"sandbox_workspace_write",
		"network_access = true",
		"CV_ALLOW_NETWORK",
		"[ -r /dev/tty ]",
	)
	// No terminal must mean "leave the sandbox alone", never "relax it quietly".
	assertContainsAll(t, codex, "leaving the Codex sandbox unchanged")
}

// TestCodexSandboxWriteCannotDuplicateTheTable pins the ordering of the sandbox
// decision, which a substring assertion alone would miss.
//
// The first version of this code checked the --allow-network flag before
// checking whether the setting was already present, so a second run with the
// flag appended a second [sandbox_workspace_write] table. Codex then refuses to
// load config.toml at all ("duplicate key") — the installer bricked the harness
// it had just set up. Caught by actually running it twice, not by any test.
//
// Two invariants, both order-dependent:
//   - the "already true" check must precede the flag branches, so a repeat run
//     with --allow-network is a no-op
//   - the "table already exists" check must also precede them, so a user whose
//     config has that section (with other keys) gets instructions instead of a
//     duplicate header
func TestCodexSandboxWriteCannotDuplicateTheTable(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	codex := installerGetShell(t, h, "codex", "CLAIMCODE0")

	alreadyTrue := strings.Index(codex, "network_access[[:space:]]*=[[:space:]]*true")
	tableExists := strings.Index(codex, `\[sandbox_workspace_write\]`)
	flagBranch := strings.Index(codex, `[ "$CV_ALLOW_NETWORK" = yes ]`)

	for _, probe := range []struct {
		name string
		idx  int
	}{
		{"already-true guard", alreadyTrue},
		{"existing-table guard", tableExists},
		{"--allow-network branch", flagBranch},
	} {
		if probe.idx < 0 {
			t.Fatalf("%s not found in the rendered codex installer", probe.name)
		}
	}

	if alreadyTrue > flagBranch {
		t.Error("the already-enabled check must run BEFORE --allow-network, or a repeat " +
			"run with the flag appends a duplicate [sandbox_workspace_write] table and " +
			"Codex stops loading config.toml entirely")
	}
	if tableExists > flagBranch {
		t.Error("the existing-table check must run BEFORE --allow-network, or a user who " +
			"already has [sandbox_workspace_write] gets a duplicate header appended")
	}
}

// TestInstallerProxyRouteOptIn — route=proxy is now the explicit opt-in, and
// must still produce the fully-routed script. Guards the inverted default:
// without this, flipping the fallback could silently make routing unreachable.
func TestInstallerProxyRouteOptIn(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")

	claude := installerGetShellQuery(t, h, "claude-code", "claim=ABCDEFGHIJ&route=proxy")
	if !strings.Contains(claude, "ANTHROPIC_BASE_URL") {
		t.Errorf("route=proxy claude-code script must bake ANTHROPIC_BASE_URL; body:\n%s", claude)
	}

	codex := installerGetShellQuery(t, h, "codex", "claim=CLAIMCODE0&route=proxy")
	if !strings.Contains(codex, "base_url = \"$LLM_URL/api/v1\"") {
		t.Errorf("route=proxy codex script must bake the proxy base_url; body:\n%s", codex)
	}
}

// TestFlipSkillOnlyOptOut — the explicit opt-out (spec 08). route=skill-only
// registers the agent WITHOUT routing: the generated claude-code and codex
// scripts must contain no provider base-URL export, so the seat keeps talking
// to its provider directly. The wizard "no" half of this opt-out is covered by
// TestWizardWritesMarkerAndExplicitKey in internal/setup (enabled: false).
func TestFlipSkillOnlyOptOut(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")

	claude := installerGetShellQuery(t, h, "claude-code", "claim=ABCDEFGHIJ&route=skill-only")
	if strings.Contains(claude, "ANTHROPIC_BASE_URL=") {
		t.Errorf("skill-only claude-code script must NOT export ANTHROPIC_BASE_URL; body:\n%s", claude)
	}
	assertContainsAll(t, claude,
		"skill-only",
		"/api/agents/connect",
		"chmod 600",
		"Done. Claude Code reaches the Clawvisor gateway through the skill.",
	)

	codex := installerGetShellQuery(t, h, "codex", "claim=CLAIMCODE0&route=skill-only")
	if strings.Contains(codex, "OPENAI_BASE_URL=") || strings.Contains(codex, "base_url = \"$LLM_URL") {
		t.Errorf("skill-only codex script must NOT bake a provider base URL; body:\n%s", codex)
	}
	assertContainsAll(t, codex,
		"skill-only",
		"/api/agents/connect",
		"Done. Codex reaches the Clawvisor gateway through the skill.",
	)
}

// TestInstallerSubscriptionModeNoApiKey — a Claude-subscription/OAuth seat
// (route=subscription) must be routed for Observe passthrough WITHOUT assuming
// an API key: the script sets ANTHROPIC_BASE_URL + the X-Clawvisor-Agent-Token
// header but never references ANTHROPIC_API_KEY / sk-ant-, and never clobbers
// the OAuth session in Authorization (no ANTHROPIC_AUTH_TOKEN override). Pairs
// with spec 02's TestObserveCarriesSubscriptionSession (the proxy side).
func TestInstallerSubscriptionModeNoApiKey(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGetShellQuery(t, h, "claude-code", "claim=ABCDEFGHIJ&route=subscription")

	// Routes for Observe.
	assertContainsAll(t, body,
		"ANTHROPIC_BASE_URL",
		"X-Clawvisor-Agent-Token: $TOKEN",
		"subscription",
	)
	// Never references an API key — no injection, no clearing, no sk-ant-.
	for _, forbidden := range []string{
		"ANTHROPIC_API_KEY",
		"sk-ant-",
		// Clearing ANTHROPIC_AUTH_TOKEN would drop the OAuth session; the
		// subscription bearer in Authorization must be left untouched.
		"ANTHROPIC_AUTH_TOKEN=",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("subscription-mode claude-code script must not contain %q (would touch the API key / OAuth session)", forbidden)
		}
	}
}

// TestInstallerInviteStdinWiring — the invite-delivery gap-fix (AGENT-GUIDE
// §2A / spec 04). Every generated shell installer (both self-install harnesses,
// default AND skill-only, plus claude-code's subscription variant) must:
//   - accept the invite on stdin (--invite-stdin) or a file (--invite-file=),
//     with CLAWVISOR_INVITE as the documented CI fallback,
//   - read it into a shell variable (never argv), and
//   - POST it to /api/agents/enroll with the body streamed on stdin.
func TestInstallerInviteStdinWiring(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	for _, tc := range inviteShellVariants() {
		t.Run(tc.name, func(t *testing.T) {
			body := installerGetShellQuery(t, h, tc.target, tc.query)
			assertContainsAll(t, body,
				// Flag surface.
				"--invite-stdin",
				"--invite-file=",
				// Reads the invite into a variable off stdin / file / env —
				// never argv.
				"IFS= read -r CV_INVITE",
				`CV_INVITE=$(cat "$CV_INVITE_FILE")`,
				"${CLAWVISOR_INVITE:-}",
				// Accepts a full invite URL by extracting the token via pure
				// shell parameter expansion (no external command sees it).
				"${CV_INVITE#*token=}",
				// Enrollment call: server-side claim + agent register in one POST.
				"/api/agents/enroll",
				// Body (carrying the invite) is streamed to curl on stdin.
				"--data @-",
			)
		})
	}
}

// TestInstallerInviteNeverOnCommandLine — the shell-history / process-table
// hygiene guard. The invite value is a bearer credential; it must never be
// interpolated onto an external command's argv (where `ps` / shell history
// would capture it). Scans every generated shell installer line-by-line and
// fails if an invite-bearing variable rides a curl / jq --arg / --data <value>
// argument. The only sanctioned path is stdin (`printf '%s' "$CV_INVITE" |
// jq -Rs …` → `… | curl … --data @-`); printf is a POSIX special builtin, so
// the value never forks out to a separate process's argv.
func TestInstallerInviteNeverOnCommandLine(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	for _, tc := range inviteShellVariants() {
		t.Run(tc.name, func(t *testing.T) {
			body := installerGetShellQuery(t, h, tc.target, tc.query)
			for i, line := range strings.Split(body, "\n") {
				code := line
				if idx := strings.Index(code, "#"); idx >= 0 {
					code = code[:idx] // ignore comments — prose may mention argv
				}
				if !strings.Contains(code, "CV_INVITE") && !strings.Contains(code, "CLAWVISOR_INVITE") {
					continue
				}
				// The invite variable must not share a line with a network or
				// arg-passing command — those put it on an external argv. The
				// sanctioned `printf '%s' "$CV_INVITE" | jq -Rs \` continuation
				// line carries neither curl, --arg, nor --data, so it passes.
				// `curl` catches its own -d/--data short forms too, since they
				// only appear on a curl line; `tr -d` (used to strip CRLF from
				// the invite via a printf pipe) is not a leak, so we don't
				// blanket-ban a bare `-d`.
				for _, bad := range []string{"curl", "--arg", "--data", "wget"} {
					if strings.Contains(code, bad) {
						t.Errorf("line %d puts the invite on a command line (contains %q): %q", i+1, bad, line)
					}
				}
			}
		})
	}
}

// inviteShellVariants enumerates every generated shell installer that must
// carry the invite-stdin wiring: both self-install harnesses, in default and
// skill-only form, plus claude-code's subscription variant (which must ALSO
// never touch a provider key while enrolling — covered by
// TestInstallerSubscriptionModeNoApiKey).
func inviteShellVariants() []struct{ name, target, query string } {
	return []struct{ name, target, query string }{
		{"claude-code-default", "claude-code", "claim=ABCDEFGHIJ"},
		{"claude-code-skill-only", "claude-code", "claim=ABCDEFGHIJ&route=skill-only"},
		{"claude-code-subscription", "claude-code", "claim=ABCDEFGHIJ&route=subscription"},
		{"codex-default", "codex", "claim=CLAIMCODE0"},
		{"codex-skill-only", "codex", "claim=CLAIMCODE0&route=skill-only"},
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
