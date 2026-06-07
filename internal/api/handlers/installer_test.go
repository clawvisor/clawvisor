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

func TestInstallerClaudeCodeRender(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGet(t, h, "claude-code", "ABCDEFGHIJ")

	// New one-paste flow with passthrough-first, swap-as-fallback:
	//   1. connect-with-claim (auto-approved)
	//   2. smoke-test in PASSTHROUGH mode using the user's existing
	//      claude login / env API key
	//   3. on auth failure ONLY: vault key + retry in swap mode
	//   4. ask make-default (gated on smoke-test pass)
	//   5. apply (env vars to settings.json OR claude-cv alias, in the
	//      mode that passed)
	//   6. self-uninstall
	//
	// NB: vault step is recovery-only — users with `claude login` or
	// `ANTHROPIC_API_KEY` in env never hit it.
	assertContainsAll(t, body,
		// Header
		"# Connect Claude Code to Clawvisor",
		"name: clawvisor-setup",
		// Mode preamble — explains why two modes exist
		"**passthrough**",
		"**swap**",
		"keeps their\nsubscription billing intact",
		// Step 1: claim-authenticated connect with existing-install detection
		"## 1. Register and persist the token",
		"Pre-flight: detect an existing install",
		`if [ -f "$TOKEN_FILE" ]; then`,
		"Overwrite it with a fresh install?",
		`rm -f "$TOKEN_FILE"`,
		"/api/agents/connect?claim=ABCDEFGHIJ",
		"&harness=claude-code",
		"~/.clawvisor/agents/$AGENT_NAME.json",
		"INVALID_CLAIM",
		// Step 2: PASSTHROUGH smoke test FIRST (no env clearing)
		"## 2. Smoke-test Clawvisor routing in **passthrough mode**",
		"We do NOT\nclear `ANTHROPIC_AUTH_TOKEN` or `ANTHROPIC_API_KEY`",
		"ANTHROPIC_BASE_URL=\"$CLAWVISOR_URL/api\"",
		"ANTHROPIC_CUSTOM_HEADERS=\"X-Clawvisor-Agent-Token: $TOKEN\"",
		"claude -p \"respond with the word OK\"",
		"`MODE=passthrough`",
		// Step 3: swap-mode fallback (vault + retry)
		"## 3. Fall back to **swap mode**",
		"only if step 2 failed with upstream auth",
		"### 3.a. Vault an Anthropic API key",
		"HARD CONSTRAINTS",
		"DO NOT `grep`",
		"printf '%s' \"$ANTHROPIC_API_KEY\" | jq -Rs '{api_key:.}'",
		"/api/runtime/llm-credentials/anthropic",
		"/dashboard/keys/anthropic?for=$AGENT_ID",
		"### 3.b. Re-run the smoke test in swap mode",
		"ANTHROPIC_AUTH_TOKEN=\"$TOKEN\"",
		"`MODE=swap`",
		// Step 4: make-default
		"## 4. Ask the user: make Clawvisor the default?",
		"`claude-cv` shell function",
		// Step 5: apply choice — both branches have mode-aware configs
		"## 5. Apply the user's choice",
		"### 5.a. Default-everywhere — commit env to `~/.claude/settings.json`",
		"**If MODE=passthrough**",
		"**If MODE=swap**",
		"### 5.b. Alias-only — append `claude-cv` to the shell rc",
		"claude-cv()",
		// YOLO opt-in: alias step asks the user once whether to bake the
		// permission-skip flag into the function. Default is no.
		"`--dangerously-skip-permissions`",
		"`$YOLO`",
		"Default is **no**",
		// Diff records under ~/.clawvisor/diffs/<agent>/ — the user's files
		// stay free of marker comments AND sentinel keys. JSON additions
		// record dot-paths; text additions record the exact appended block.
		"~/.clawvisor/diffs/$AGENT_NAME/settings.json",
		"~/.clawvisor/diffs/$AGENT_NAME/claude_cv.json",
		`type: "json_keys"`,
		`type: "text_append"`,
		// Prior-value capture: each entry has the prior_value the install
		// overwrote, so uninstall restores instead of just deletes.
		`[$paths[] as $p | {path: $p, prior_value: ($prior | getpath($p / "."))}]`,
		// Paths are emitted via --argjson (jq array) — pre-merge capture.
		`"env.ANTHROPIC_BASE_URL","env.ANTHROPIC_CUSTOM_HEADERS"`,
		`"env.ANTHROPIC_BASE_URL","env.ANTHROPIC_AUTH_TOKEN","env.ANTHROPIC_API_KEY"`,
		// Defensive jq parse — handles empty / invalid-JSON / non-object
		// settings.json without crashing the install partway through.
		`jq -c 'if type == "object" then . else {} end'`,
		`[ -n "$PRIOR_JSON" ] || PRIOR_JSON='{}'`,
		// Step 6: drop uninstall skill + self-uninstall the setup file
		"## 6. Drop the uninstall skill, then self-uninstall",
		"/skill/uninstall/claude-code.md?agent_name=$AGENT_NAME",
		"~/.claude/commands/clawvisor-uninstall.md",
		"rm -f ~/.claude/commands/clawvisor-setup.md",
		"`/clawvisor-uninstall`",
	)
	// LLM proxy mediates tool calls server-side; the install skill must NOT
	// drop the service-routing skill onto the agent's disk in proxy-lite mode.
	if strings.Contains(body, "Install the Clawvisor skill") {
		t.Errorf("proxy-lite flow should not install agent-side Clawvisor skill")
	}
	if strings.Contains(body, "~/.claude/skills/clawvisor") {
		t.Errorf("proxy-lite flow should not write to ~/.claude/skills/clawvisor")
	}
	// The new flow has no second dashboard click — assert claim auto-approves
	// (the curl returns the token immediately, no long-poll on connect).
	if strings.Contains(body, "wait=true") {
		t.Errorf("new flow uses claim auto-approval, not wait=true long-poll")
	}
	if strings.Contains(body, "## 2. Mint a connection request") {
		t.Errorf("two-phase mint+approve replaced by one-shot claim auto-approve")
	}
	if strings.Contains(body, "Dashboard answers") {
		t.Errorf("dashboard-driven configure-questions removed in the one-paste flow")
	}
	if strings.Contains(body, "Check for an existing token") {
		t.Errorf("installer should not offer to reuse an existing token")
	}
}

func TestInstallerCodexRender(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGet(t, h, "codex", "CLAIMCODE0")

	// New one-paste flow with passthrough-first, swap-as-fallback:
	//   1. connect-with-claim (auto-approved)
	//   2. write [model_providers.clawvisor] block in PASSTHROUGH form
	//   3. smoke-test in passthrough mode (uses user's codex login / env key)
	//   4. on auth failure ONLY: vault key, rewrite block to swap form, retry
	//   5. ask make-default (gated on smoke-test pass)
	//   6. apply (model_provider top-level OR codex-cv alias, mode-aware)
	//   7. self-uninstall
	assertContainsAll(t, body,
		// Header + frontmatter
		"# Connect Codex to Clawvisor",
		"name: clawvisor-setup",
		// Mode preamble
		"**passthrough**",
		"**swap**",
		"keeps their\nsubscription billing intact",
		// Step 1: claim-authenticated connect with existing-install detection
		"## 1. Register and persist the token",
		"Pre-flight: detect an existing install",
		`if [ -f "$TOKEN_FILE" ]; then`,
		"Overwrite it with a fresh install?",
		`rm -f "$TOKEN_FILE"`,
		"/api/agents/connect?claim=CLAIMCODE0",
		"&harness=codex",
		"~/.clawvisor/agents/$AGENT_NAME.json",
		// Step 2: write provider block in passthrough form
		"## 2. Write the Clawvisor provider block (passthrough form)",
		"[model_providers.clawvisor]",
		`base_url = "$CLAWVISOR_URL/api/v1"`,
		"requires_openai_auth = true",
		"X-Clawvisor-Agent-Token = \"CLAWVISOR_AGENT_TOKEN\"",
		// Step 3: passthrough smoke test
		"## 3. Smoke-test Clawvisor routing in **passthrough mode**",
		"timeout 30 codex",
		"-c model_provider=clawvisor",
		`exec "respond with the word OK"`,
		"`MODE=passthrough`",
		// Step 4: swap-mode fallback
		"## 4. Fall back to **swap mode**",
		"only if step 3 failed with upstream auth",
		"### 4.a. Vault an OpenAI API key",
		"DO NOT `grep`",
		"DO NOT `echo \"$OPENAI_API_KEY\"`",
		"printf '%s' \"$OPENAI_API_KEY\" | jq -Rs '{api_key:.}'",
		"/api/runtime/llm-credentials/openai",
		"/dashboard/keys/openai?for=$AGENT_ID",
		"### 4.b. Rewrite the provider block to swap form",
		"requires_openai_auth = false",
		"Authorization = \"CLAWVISOR_AGENT_BEARER\"",
		"### 4.c. Re-run the smoke test in swap mode",
		"CLAWVISOR_AGENT_BEARER=\"Bearer $TOKEN\"",
		"`MODE=swap`",
		// Step 5: make-default
		"## 5. Ask the user: make Clawvisor the default?",
		"`codex-cv` shell function",
		// Step 6: apply
		"## 6. Apply the user's choice",
		`### 6.a. Default-everywhere — set ` + "`model_provider = \"clawvisor\"`" + ` as the default`,
		"**If MODE=passthrough**",
		"**If MODE=swap**",
		"CLAWVISOR_AGENT_BEARER",
		"### 6.b. Alias-only — append `codex-cv` to the shell rc",
		"codex-cv()",
		// YOLO opt-in (Codex equivalent).
		"`--dangerously-bypass-approvals-and-sandbox`",
		"`$YOLO`",
		"Default is **no**",
		// Diff records under ~/.clawvisor/diffs/<agent>/ for each Codex
		// modification site. User config files are unannotated.
		"~/.clawvisor/diffs/$AGENT_NAME/provider_block.json",
		"~/.clawvisor/diffs/$AGENT_NAME/default_provider.json",
		"~/.clawvisor/diffs/$AGENT_NAME/rc_export.json",
		"~/.clawvisor/diffs/$AGENT_NAME/codex_cv.json",
		`type: "text_append"`,
		`type: "text_prepend"`,
		// Step 7: drop uninstall skill + self-uninstall the setup directory
		"## 7. Drop the uninstall skill, then self-uninstall",
		"/skill/uninstall/codex.md?agent_name=$AGENT_NAME",
		"~/.codex/skills/clawvisor-uninstall/SKILL.md",
		"rm -rf ~/.codex/skills/clawvisor-setup",
		"`clawvisor-uninstall` skill",
	)
	if strings.Contains(body, "Install the Clawvisor skill") {
		t.Errorf("proxy-lite flow should not install agent-side Clawvisor skill")
	}
	if strings.Contains(body, "~/.codex/skills/clawvisor/SKILL.md") {
		t.Errorf("proxy-lite flow should not write to ~/.codex/skills/clawvisor (the service-routing skill)")
	}
	if strings.Contains(body, "## 2. Mint a connection request") {
		t.Errorf("two-phase mint+approve replaced by one-shot claim auto-approve")
	}
	if strings.Contains(body, "Dashboard answers") {
		t.Errorf("dashboard-driven configure-questions removed in the one-paste flow")
	}
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
// Claude Code and Codex use the `clawvisor-setup` slash command (one-paste
// connect existing CLI); Hermes and OpenClaw use `clawvisor-install` (install
// a harness binary). Both names are accepted here — the test asserts the
// frontmatter shape, not the specific name.
// uninstallGet hits the uninstall endpoint with a target + optional agent
// name and returns the rendered markdown body. Mirrors installerGet.
func uninstallGet(t *testing.T, h *InstallerHandler, target, agentName string) string {
	t.Helper()
	path := "/skill/uninstall/" + target + ".md"
	if agentName != "" {
		path += "?agent_name=" + agentName
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /skill/uninstall/{target}", h.Uninstall)
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

func TestUninstallClaudeCodeRender(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := uninstallGet(t, h, "claude-code", "claude-code-7")

	// Frontmatter + mode-detection + reversals for both default-everywhere
	// (settings.json) and alias-only (shell rc) paths. Each path checks for
	// the install-time backup first and prefers restore-from-backup over
	// surgical edit. Token file delete + dashboard cleanup pointer at the end.
	assertContainsAll(t, body,
		"# Uninstall Clawvisor from Claude Code",
		"name: clawvisor-uninstall",
		"AGENT_NAME=\"claude-code-7\"",
		"## 1. Detect the install mode",
		"~/.claude/settings.json",
		"`claude-cv()`",
		"## 2. Reverse the config from the diff records",
		// Uninstall walks the diff records and reverses each. Python3 is
		// universal on macOS / modern Linux and handles json_keys,
		// text_append, and text_prepend uniformly.
		"~/.clawvisor/diffs/$AGENT_NAME/",
		"ls ~/.clawvisor/diffs/$AGENT_NAME/",
		"python3 - <<'PY'",
		"json_keys",
		"text_append",
		"text_prepend",
		`if rec['type'] == 'json_keys':`,
		// Prior-value restore for json_keys: if prior is non-null we set
		// the key back to it instead of just deleting (preserves any value
		// the user had before install).
		"prior_value",
		`if entry.get('prior_value') is None:`,
		"set_at(doc, parts, entry['prior_value'])",
		// Legacy 'paths' fallback for pre-prior-value diff records.
		"rec.get('paths', [])",
		// Legacy fallback for installs that pre-date the diff-records design.
		"legacy install",
		"ANTHROPIC_BASE_URL",
		"claude-cv()",
		// 3 / 4 / 5
		"## 3. Delete the local token file",
		`rm -f "$TOKEN_FILE"`,
		"## 4. Tell the user about dashboard cleanup",
		"/dashboard/agents",
		"/dashboard/keys/anthropic",
		"## 5. Self-uninstall",
		"rm -rf ~/.clawvisor/diffs/$AGENT_NAME",
		"rm -f ~/.claude/commands/clawvisor-uninstall.md",
	)
}

func TestUninstallCodexRender(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	body := uninstallGet(t, h, "codex", "codex-2")

	// Backup-first uninstall: check for `<file>.clawvisor-backup-<agent>` and
	// restore if present; surgical edit otherwise. Two files in scope:
	// config.toml and the shell rc (which holds either the env export for
	// default-everywhere OR the codex-cv function for alias-only — same
	// rc-handling step covers both).
	assertContainsAll(t, body,
		"# Uninstall Clawvisor from Codex",
		"name: clawvisor-uninstall",
		"AGENT_NAME=\"codex-2\"",
		"## 1. Detect the install state",
		"~/.codex/config.toml",
		"`codex-cv()`",
		"## 2. Reverse the config from the diff records",
		// Same diff-walker as Claude Code uninstall — harness-agnostic.
		"~/.clawvisor/diffs/$AGENT_NAME/",
		"ls ~/.clawvisor/diffs/$AGENT_NAME/",
		"python3 - <<'PY'",
		"json_keys",
		"text_append",
		"text_prepend",
		// Legacy fallback for pre-diff-records installs (still strips by
		// section header / sed line-delete).
		"legacy install",
		`awk 'BEGIN{skip=0} /^\[model_providers\.clawvisor/{skip=1; next}`,
		`sed -i.bak '/^model_provider = "clawvisor"$/d' ~/.codex/config.toml`,
		"CLAWVISOR_AGENT_TOKEN",
		"codex-cv()",
		// 3 / 4 / 5
		"## 3. Delete the local token file",
		"## 4. Tell the user about dashboard cleanup",
		"/dashboard/agents",
		"/dashboard/keys/openai",
		"## 5. Self-uninstall",
		"rm -rf ~/.clawvisor/diffs/$AGENT_NAME",
		"rm -rf ~/.codex/skills/clawvisor-uninstall",
	)
}

func TestInstallerRejectsMaliciousClaim(t *testing.T) {
	// claim is interpolated into a shell-quoted curl URL inside the rendered
	// skill. Any character outside URL-safe base64 must be silently dropped
	// rather than embedded, so a paste like
	//   `/skill/install/claude-code.md?claim=foo";+rm+-rf+~;+echo+"`
	// can't break out of the shell string and execute arbitrary commands.
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
		body := installerGetQuery(t, h, "claude-code", "claim="+url.QueryEscape(claim))
		if strings.Contains(body, "claim="+claim) {
			t.Errorf("malicious claim %q was interpolated unescaped into rendered body", claim)
		}
		// Without a valid claim, the body still renders but without claim= in the
		// curl URL — the skill prints an explanatory "no claim code" message.
		if !strings.Contains(body, "no claim code") {
			t.Errorf("expected no-claim fallback in body for malicious claim %q", claim)
		}
	}
}

func TestInstallerAcceptsValidClaim(t *testing.T) {
	// Sanity check the positive path — a real 10-char base64 claim is
	// accepted and lands in the rendered curl URL.
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGet(t, h, "claude-code", "abcDEF_-09")
	if !strings.Contains(body, "claim=abcDEF_-09") {
		t.Errorf("valid claim was dropped; body excerpt:\n%s", body[:min(len(body), 500)])
	}
}

func TestUninstallUnknownTargetIs404(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /skill/uninstall/{target}", h.Uninstall)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Only claude-code and codex have uninstall renderers — Hermes / OpenClaw
	// uninstall lives in the inline `uninstall-<harness>.md` reference doc
	// the existing installer flow writes to ~/.clawvisor/.
	for _, target := range []string{"hermes", "openclaw", "perplexity"} {
		resp, err := http.Get(srv.URL + "/skill/uninstall/" + target + ".md")
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("[%s] expected 404, got %d", target, resp.StatusCode)
		}
	}
}

func TestInstallerAllTargetsHaveFrontmatter(t *testing.T) {
	h := NewInstallerHandler("", "", true, "", "")
	wantName := map[string]string{
		"claude-code": "clawvisor-setup",
		"codex":       "clawvisor-setup",
		"hermes":      "clawvisor-install",
		"openclaw":    "clawvisor-install",
	}
	for target, name := range wantName {
		body := installerGet(t, h, target, "")
		want := "---\nname: " + name + "\ndescription:"
		if !strings.HasPrefix(body, want) {
			t.Errorf("[%s] missing required YAML frontmatter (want prefix %q). First 200 chars:\n%s",
				target, want, body[:min(len(body), 200)])
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
	//
	// The new one-paste flow exports CLAWVISOR_URL once at the top of the
	// skill body and uses $CLAWVISOR_URL everywhere downstream. So we
	// assert the export line uses the proxy URL, plus that the app URL
	// never appears anywhere in the body.
	h := NewInstallerHandler("", "", false, "https://llm.example.com", "https://app.example.com")
	body := installerGet(t, h, "claude-code", "")
	if !strings.Contains(body, `export CLAWVISOR_URL="https://llm.example.com"`) {
		t.Errorf("expected CLAWVISOR_URL export to use configured LLM proxy URL; body excerpt:\n%s",
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
	// before falling back to the request host. The new one-paste flow
	// exports CLAWVISOR_URL once at the top and uses $CLAWVISOR_URL
	// downstream — assert on the export line.
	h := NewInstallerHandler("", "", false, "", "https://app.example.com")
	body := installerGet(t, h, "codex", "")
	if !strings.Contains(body, `export CLAWVISOR_URL="https://app.example.com"`) {
		t.Errorf("expected CLAWVISOR_URL export to use server public URL; body excerpt:\n%s",
			body[:min(len(body), 500)])
	}
	if strings.Contains(body, "http://127.0.0.1:") {
		t.Errorf("embedded URL should not fall back to request host when server public URL is configured")
	}
}

func TestInstallerEmbedsRequestHost(t *testing.T) {
	// When not via the relay, the resolved URL should mirror the request host so
	// agents on the user's box talk to the daemon directly. The new one-paste
	// flow embeds the URL via `export CLAWVISOR_URL=…` at the top of the body.
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGet(t, h, "claude-code", "")
	if !strings.Contains(body, "ANTHROPIC_BASE_URL") {
		t.Fatalf("rendered body missing ANTHROPIC_BASE_URL: %s", body)
	}
	if !strings.Contains(body, "/api/runtime/llm-credentials/anthropic") {
		t.Fatalf("rendered body missing llm-credentials endpoint: %s", body)
	}
	// The httptest server uses an ephemeral 127.0.0.1 host; the body should
	// embed that as the CLAWVISOR_URL export so the user's curl actually
	// reaches the daemon.
	if !strings.Contains(body, `export CLAWVISOR_URL="http://127.0.0.1:`) {
		t.Errorf("expected request host to be embedded as CLAWVISOR_URL export, body excerpt:\n%s", body[:min(len(body), 500)])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
