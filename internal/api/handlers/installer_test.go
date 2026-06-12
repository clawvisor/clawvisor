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
		// Step 2: PASSTHROUGH smoke test FIRST (no env clearing).
		// Control-plane and LLM-proxy URLs are deliberately separate env
		// vars (CLAWVISOR_APP_URL vs CLAWVISOR_LLM_URL) — registration
		// goes to the app host, model calls to the LLM proxy host.
		"## 2. Smoke-test Clawvisor routing in **passthrough mode**",
		"We do NOT\nclear `ANTHROPIC_AUTH_TOKEN` or `ANTHROPIC_API_KEY`",
		"ANTHROPIC_BASE_URL=\"$CLAWVISOR_LLM_URL/api\"",
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
		// Step 6: install summary
		"## 6. Print the install summary for the user",
		"Clawvisor install complete",
		"Harness:      Claude Code",
		"Provider:     Anthropic",
		"alias-only via `claude-cv`",
		"Revert:       `/clawvisor-uninstall`",
		// Step 7: drop uninstall skill + self-uninstall the setup file
		"## 7. Drop the uninstall skill, then self-uninstall",
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
		// Step 2: write provider block in passthrough form. Test URL host is
		// 127.0.0.1 → env-derived slug is `clawvisor-dev` (see codexProviderID).
		"## 2. Write the Clawvisor provider block (passthrough form)",
		"[model_providers.clawvisor-dev]",
		`name = "Clawvisor (dev)"`,
		`base_url = "$CLAWVISOR_LLM_URL/api/v1"`,
		"requires_openai_auth = true",
		"X-Clawvisor-Agent-Token = \"CLAWVISOR_AGENT_TOKEN\"",
		// Step 3: passthrough smoke test
		"## 3. Smoke-test Clawvisor routing in **passthrough mode**",
		"CLAWVISOR_AGENT_TOKEN=\"$TOKEN\" codex",
		"-c model_provider=clawvisor-dev",
		`exec --skip-git-repo-check "respond with the word OK"`,
		"-c sandbox_workspace_write.network_access=true",
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
		`### 6.a. Default-everywhere — set ` + "`model_provider = \"clawvisor-dev\"`" + ` as the default`,
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
		// Step 7: install summary
		"## 7. Print the install summary for the user",
		"Clawvisor install complete",
		"Harness:      Codex",
		"Provider:     OpenAI",
		"alias-only via `codex-cv`",
		"Revert:       invoke the `clawvisor-uninstall` skill",
		// Step 8: drop uninstall skill + self-uninstall the setup directory
		"## 8. Drop the uninstall skill, then self-uninstall",
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

// TestInstallerCodexProviderSlugByEnv locks the mapping from LLM proxy URL host
// to the [model_providers.<slug>] key in the rendered Codex install skill, so
// prod / staging / dev installs can coexist in one ~/.codex/config.toml without
// the blocks colliding. Hosts come from the canonical Clawvisor deployments;
// fallback for unknown hosts is dev.
func TestInstallerCodexProviderSlugByEnv(t *testing.T) {
	cases := []struct {
		name          string
		llmProxyURL   string
		wantSlug      string
		wantDisplay   string
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
			llmProxyURL: "", // empty → fallback to request host (127.0.0.1) → dev
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
			body := installerGet(t, h, "codex", "CLAIMCODE0")
			assertContainsAll(t, body,
				"[model_providers."+tc.wantSlug+"]",
				`name = "`+tc.wantDisplay+`"`,
				"-c model_provider="+tc.wantSlug,
				`model_provider = "`+tc.wantSlug+`"`,
			)
			for _, np := range tc.wantNotPresent {
				if strings.Contains(body, np) {
					t.Errorf("unexpected %q in body (should only appear for that env)", np)
				}
			}
		})
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
		// Step 2: detect provider — reads env + models.json provider keys
		// via jq (jq is safe — it can extract structured fields without
		// dumping arbitrary content).
		"## 2. Detect the upstream LLM provider",
		`for cfg in ~/.openclaw/agents/*/agent/models.json`,
		"anthropic*|claude*",
		"openai*|gpt*",
		// Case block (same as Hermes — shared via providerCaseBlock).
		`case "$PROVIDER" in`,
		"PROVIDER_LABEL='Anthropic'",
		"PROVIDER_LABEL='OpenAI'",
		"MODEL_ID='claude-sonnet-4-6'",
		"MODEL_ID='gpt-5.4'",
		"CONTEXT_WINDOW=200000",
		"CONTEXT_WINDOW=1000000",
		// Step 3: ensure vaulted key (provider-agnostic title).
		"## 3. Ensure a vaulted upstream key exists",
		"/api/runtime/llm-credentials",
		`### 3.a. Vault a $PROVIDER_LABEL API key`,
		// Step 4-5: probe + preflight.
		"## 4. Probe the OpenClaw deployment",
		"$OPENCLAW_MODE",
		"## 5. Preflight: confirm OpenClaw can reach Clawvisor",
		"-H \"X-Clawvisor-Agent-Token: $CLAWVISOR_TOKEN\"",
		"host.docker.internal:",
		"/api/skill/catalog",
		// Step 6: configure — onboard uses $PROVIDER / $MODEL_ID / $BASE_PATH.
		"## 6. Point OpenClaw at Clawvisor",
		"openclaw-cli onboard --non-interactive",
		"--auth-choice custom-api-key",
		`--custom-base-url "$CLAWVISOR_LLM_URL$BASE_PATH"`,
		`--custom-model-id "$MODEL_ID"`,
		"--custom-api-key \"$TOKEN\"",
		`--custom-compatibility "$PROVIDER" --accept-risk`,
		`docker exec "$OPENCLAW_CONTAINER" openclaw-cli onboard`,
		"OPENCLAW_MAX_TOKENS=8192",
		`1M beta context`,
		"models.json",
		`jq --arg model "$MODEL_ID"`,
		`--argjson contextWindow "$CONTEXT_WINDOW"`,
		// Remote mode rendered alongside host/docker.
		"ssh \"$OPENCLAW_REMOTE\" \"openclaw-cli onboard",
		"REMOTE_OPENCLAW_PATCH",
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
		// Provider-baked literals (must come from $PROVIDER / $MODEL_ID).
		"## 2. Ensure a Anthropic key is vaulted",
		"## 2. Ensure a OpenAI key is vaulted",
		"--custom-compatibility anthropic --accept-risk",
		"--custom-compatibility openai --accept-risk",
		`--custom-model-id "claude-sonnet-4-6"`,
		`--custom-model-id "gpt-5.4"`,
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
	// $BASE_PATH shell var instead of a hardcoded /api or /api/v1 path.
	h := NewInstallerHandler("", "", true, "", "")
	body := installerGet(t, h, "openclaw", "CLAIMOPEN12")

	assertContainsAll(t, body,
		// Host: bare openclaw-cli onboard.
		"openclaw-cli onboard --non-interactive",
		// Docker: docker exec into the already-running container (probe
		// captures its name as $OPENCLAW_CONTAINER). $BASE_PATH gets
		// substituted from the case block at install time.
		`docker exec "$OPENCLAW_CONTAINER" openclaw-cli onboard`,
		"host.docker.internal:",
		// Remote: ssh-wrapped onboard + remote models.json patch.
		"ssh \"$OPENCLAW_REMOTE\" \"openclaw-cli onboard",
		"export OPENCLAW_CLAWVISOR_URL",
		"$OPENCLAW_CLAWVISOR_URL$BASE_PATH",
		"REMOTE_OPENCLAW_PATCH",
	)
}

// TestInstallerAllTargetsHaveFrontmatter — Codex rejects skills without YAML
// frontmatter at load time; we caught this in the field after a real install,
// so guard against regression by asserting the exact shape on every target.
// All four targets now use the `clawvisor-setup` slash command — the
// one-paste shape (auto-approve claim → in-skill mint, configure, smoke
// test, self-uninstall) is uniform across Claude Code, Codex, Hermes, and
// OpenClaw.
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
		// awk strips any [model_providers.clawvisor*] block; sed regex catches
		// all three env-derived slugs (clawvisor, clawvisor-staging, clawvisor-dev).
		`awk 'BEGIN{skip=0} /^\[model_providers\.clawvisor/{skip=1; next}`,
		`sed -i.bak -E '/^model_provider = "clawvisor(-staging|-dev)?"$/d' ~/.codex/config.toml`,
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
		"hermes":      "clawvisor-setup",
		"openclaw":    "clawvisor-setup",
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

func TestInstallerSplitsAppAndLLMURLs(t *testing.T) {
	// Control plane (registration, dashboard, credentials, skill catalog)
	// lives on the app host; the LLM proxy is a separate host. The install
	// script exports both — CLAWVISOR_APP_URL for control-plane curls and
	// CLAWVISOR_LLM_URL for what gets baked into ANTHROPIC_BASE_URL etc.
	// Conflating the two (using the proxy URL for /api/agents/connect)
	// 404s in split deployments — that's the regression this test guards.
	h := NewInstallerHandler("", "", false, "https://llm.example.com", "https://app.example.com")
	body := installerGet(t, h, "claude-code", "TESTCLAIM0")
	if !strings.Contains(body, `export CLAWVISOR_APP_URL="https://app.example.com"`) {
		t.Errorf("expected CLAWVISOR_APP_URL export to use server public URL; body excerpt:\n%s",
			body[:min(len(body), 800)])
	}
	if !strings.Contains(body, `export CLAWVISOR_LLM_URL="https://llm.example.com"`) {
		t.Errorf("expected CLAWVISOR_LLM_URL export to use LLM proxy URL; body excerpt:\n%s",
			body[:min(len(body), 800)])
	}
	// Registration MUST hit the app host. Hitting it on the LLM host is
	// what caused the original bug — guard explicitly.
	if !strings.Contains(body, `"$CLAWVISOR_APP_URL/api/agents/connect`) {
		t.Errorf("agent registration must target $CLAWVISOR_APP_URL, not the LLM proxy")
	}
	if strings.Contains(body, `"$CLAWVISOR_LLM_URL/api/agents/connect`) {
		t.Errorf("agent registration must NOT target $CLAWVISOR_LLM_URL")
	}
	// Model traffic must hit the LLM host.
	if !strings.Contains(body, `ANTHROPIC_BASE_URL="$CLAWVISOR_LLM_URL/api"`) {
		t.Errorf("ANTHROPIC_BASE_URL must target $CLAWVISOR_LLM_URL")
	}
	if strings.Contains(body, "http://127.0.0.1:") {
		t.Errorf("embedded URLs should not fall back to request host when both PublicURLs are configured")
	}
}

func TestInstallerFallsBackToServerPublicURL(t *testing.T) {
	// If there is no dedicated lite-proxy URL, both AppURL and LLMURL fall
	// back to Server.PublicURL (single-host deployment).
	h := NewInstallerHandler("", "", false, "", "https://app.example.com")
	body := installerGet(t, h, "codex", "")
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
	body := installerGet(t, h, "claude-code", "")
	if !strings.Contains(body, "ANTHROPIC_BASE_URL") {
		t.Fatalf("rendered body missing ANTHROPIC_BASE_URL: %s", body)
	}
	if !strings.Contains(body, "/api/runtime/llm-credentials/anthropic") {
		t.Fatalf("rendered body missing llm-credentials endpoint: %s", body)
	}
	// httptest binds an ephemeral 127.0.0.1 host; both exports should embed it.
	if !strings.Contains(body, `export CLAWVISOR_APP_URL="http://127.0.0.1:`) {
		t.Errorf("expected request host to be embedded as CLAWVISOR_APP_URL export, body excerpt:\n%s", body[:min(len(body), 800)])
	}
	if !strings.Contains(body, `export CLAWVISOR_LLM_URL="http://127.0.0.1:`) {
		t.Errorf("expected request host to be embedded as CLAWVISOR_LLM_URL export, body excerpt:\n%s", body[:min(len(body), 800)])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
