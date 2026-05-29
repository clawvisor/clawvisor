package handlers

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/relay"
)

// InstallerTarget identifies which harness the installer skill is for.
type InstallerTarget string

const (
	InstallerClaudeCode InstallerTarget = "claude-code"
	InstallerCodex      InstallerTarget = "codex"
	InstallerHermes     InstallerTarget = "hermes"
	InstallerOpenClaw   InstallerTarget = "openclaw"
)

// InstallerHandler serves per-harness installer skills at
// GET /skill/install/{target}.md. Each target's markdown is rendered with a
// pre-filled Clawvisor URL and (optionally) a claim code so the installed
// skill can mint a connection request on the user's behalf without ever
// seeing the user's ID.
type InstallerHandler struct {
	relayHost string
	daemonID  string
	isLocal   bool
	// llmProxyURL is the externally reachable lite-proxy endpoint configured
	// via cfg.ProxyLite.PublicURL. It wins for installer-rendered CLAWVISOR_URL
	// because LLM harnesses need to route model calls through the proxy host.
	llmProxyURL string
	// publicURL is cfg.Server.PublicURL. It is the next-best user-configured
	// externally reachable host when a dedicated lite-proxy URL is not set.
	publicURL string
}

func NewInstallerHandler(relayHost, daemonID string, isLocal bool, llmProxyURL, publicURL string) *InstallerHandler {
	return &InstallerHandler{
		relayHost:   relayHost,
		daemonID:    daemonID,
		isLocal:     isLocal,
		llmProxyURL: strings.TrimRight(strings.TrimSpace(llmProxyURL), "/"),
		publicURL:   strings.TrimRight(strings.TrimSpace(publicURL), "/"),
	}
}

type installerCtx struct {
	ClawvisorURL    string
	UserID          string // optional; rendered into the install context fallback path
	Claim           string // optional; rendered into the mint URL
	IsLocal         bool
	LLMProvider     string
	ClaudeScope     string
	ClaudeCurlAllow string
	AliasMode       string
	HermesConfig    string
	OpenClawMode    string
	TaskApproval    string
}

// Setup handles GET /skill/install/{target}. The route captures the whole
// segment (Go's ServeMux doesn't allow `{target}.md`), so we trim a trailing
// `.md` here — the dashboard renders the public URL with the extension for
// content-sniffing on the agent side.
func (h *InstallerHandler) Setup(w http.ResponseWriter, r *http.Request) {
	target := InstallerTarget(strings.TrimSuffix(r.PathValue("target"), ".md"))

	ctx := installerCtx{
		ClawvisorURL: h.resolveURL(r),
		IsLocal:      h.isLocal,
	}
	if uid := r.URL.Query().Get("user_id"); uid != "" && validUserID.MatchString(uid) {
		ctx.UserID = uid
	}
	if claim := r.URL.Query().Get("claim"); claim != "" {
		ctx.Claim = claim
	}
	ctx.ClaudeScope = queryChoice(r, "claude_scope", "alias", "alias", "global")
	ctx.ClaudeCurlAllow = queryChoice(r, "claude_curl_allow", "no", "no", "yes")
	ctx.AliasMode = queryChoice(r, "alias_mode", "safe", "none", "safe", "yolo")
	ctx.HermesConfig = queryChoice(r, "hermes_config", "env", "env", "file")
	ctx.OpenClawMode = queryChoice(r, "openclaw_mode", "host", "host", "docker", "remote")
	defaultProvider := "anthropic"
	if target == InstallerHermes {
		defaultProvider = "openai"
	}
	ctx.LLMProvider = queryChoice(r, "llm_provider", defaultProvider, "anthropic", "openai")
	ctx.TaskApproval = queryChoice(r, "task_approval", "manual", "manual", "low", "medium")

	var body string
	switch target {
	case InstallerClaudeCode:
		body = renderClaudeCodeInstaller(ctx)
	case InstallerCodex:
		body = renderCodexInstaller(ctx)
	case InstallerHermes:
		body = renderHermesInstaller(ctx)
	case InstallerOpenClaw:
		body = renderOpenClawInstaller(ctx)
	default:
		http.Error(w, "unknown installer target", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

func queryChoice(r *http.Request, key, fallback string, allowed ...string) string {
	got := r.URL.Query().Get(key)
	for _, v := range allowed {
		if got == v {
			return got
		}
	}
	return fallback
}

func installerProviderDisplayName(provider string) string {
	if provider == "openai" {
		return "OpenAI"
	}
	return "Anthropic"
}

func providerBasePath(provider string) string {
	if provider == "openai" {
		return "/api/v1"
	}
	return "/api"
}

func providerDefaultModel(provider string) string {
	if provider == "openai" {
		return "gpt-5.4"
	}
	return "claude-sonnet-4-6"
}

func providerDefaultContextWindow(provider string) int {
	return 1000000
}

func openClawDefaultMaxTokens() int {
	return 8192
}

func providerBaseEnv(provider string) string {
	if provider == "openai" {
		return "OPENAI_BASE_URL"
	}
	return "ANTHROPIC_BASE_URL"
}

func providerKeyEnv(provider string) string {
	if provider == "openai" {
		return "OPENAI_API_KEY"
	}
	return "ANTHROPIC_API_KEY"
}

func (h *InstallerHandler) resolveURL(r *http.Request) string {
	// URL precedence for the agent-side installer:
	// 1. Dedicated LLM proxy public URL, when configured.
	// 2. General server public URL, when configured.
	// 3. The actual request/relay/local server URL.
	//
	// This keeps CLAWVISOR_URL pointed at the endpoint the next agent can use
	// for both registration curls and LLM proxy traffic.
	if h.llmProxyURL != "" {
		return h.llmProxyURL
	}
	if h.publicURL != "" {
		return h.publicURL
	}
	if !relay.ViaRelay(r.Context()) {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if fp := r.Header.Get("X-Forwarded-Proto"); fp != "" {
			scheme = fp
		}
		return scheme + "://" + r.Host
	}
	if h.daemonID != "" && h.relayHost != "" {
		return fmt.Sprintf("https://%s/d/%s", h.relayHost, h.daemonID)
	}
	return "http://localhost:25297"
}

// installerFrontmatter emits the YAML frontmatter every target's skill loader
// expects. Codex *requires* `name` + `description` (rejects skills without it
// at startup); Hermes/OpenClaw skills use the same shape; Claude
// Code slash commands accept a `description` (shown in the slash-command
// picker). One shared block keeps the four renders in sync.
func installerFrontmatter(harness string) string {
	return fmt.Sprintf(`---
name: clawvisor-install
description: Install Clawvisor into %s — probe the environment, mint and approve a connection request, configure %s, optionally add an alias, run a connectivity smoke test, and remove itself when done.
---

`, harness, harness)
}

// ── Shared markdown helpers ──────────────────────────────────────────────────
//
// Every installer skill follows the same outline: probe → mint → persist →
// configure → alias → smoke test → uninstall reference →
// self-uninstall. The shared helpers render the steps that don't vary; the
// per-target functions slot in their own configure/alias/self-uninstall.

func sectionProbe(harness string, extra []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 1. Probe the environment\n\n")
	fmt.Fprintf(&b, "Before doing anything, learn enough about the user's setup that the install\n")
	fmt.Fprintf(&b, "decisions below have answers. Use shell commands when the machine knows;\n")
	fmt.Fprintf(&b, "ask the user when it doesn't. Do not guess silently.\n\n")
	fmt.Fprintf(&b, "Determine:\n\n")
	fmt.Fprintf(&b, "- **Harness running mode** — local, docker, or cloud (`docker ps`, `uname -s`,\n")
	fmt.Fprintf(&b, "  filesystem checks). If docker, capture the container ID.\n")
	fmt.Fprintf(&b, "- **%s install state** — installed? which version? auth mode?\n", harness)
	fmt.Fprintf(&b, "- **Shell** — zsh (default on macOS), bash, fish — needed for the alias step.\n")
	for _, e := range extra {
		fmt.Fprintf(&b, "- %s\n", e)
	}
	fmt.Fprintf(&b, "\nKeep what you learned in a JSON object — you'll send it as `install_context`\n")
	fmt.Fprintf(&b, "on the mint request below so the user sees install details on the approval\n")
	fmt.Fprintf(&b, "card. Fields are all optional; send what you know.\n\n")
	fmt.Fprintf(&b, "```json\n")
	fmt.Fprintf(&b, "{\n")
	fmt.Fprintf(&b, "  \"harness\": %q,\n", harness)
	fmt.Fprintf(&b, "  \"harness_version\": \"<x.y.z or omit>\",\n")
	fmt.Fprintf(&b, "  \"install_mode\": \"local | docker | cloud\",\n")
	fmt.Fprintf(&b, "  \"host_os\": \"darwin | linux | windows\",\n")
	fmt.Fprintf(&b, "  \"container_id\": \"<docker only>\",\n")
	fmt.Fprintf(&b, "  \"auth_mode\": \"passthrough | swap\",\n")
	fmt.Fprintf(&b, "  \"alias_intent\": \"none | safe | yolo\"\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "```\n\n")
	return b.String()
}

func sectionLocalCLIProbe(harness string, versionCommand string, authCheck string, extra []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 1. Check the local CLI\n\n")
	fmt.Fprintf(&b, "This path assumes %s is installed on the user's local machine. Keep the\n", harness)
	fmt.Fprintf(&b, "setup simple: verify the CLI exists, verify auth is present, identify the\n")
	fmt.Fprintf(&b, "user's shell for the alias step, and ask only if something is missing.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "%s\n", versionCommand)
	if authCheck != "" {
		fmt.Fprintf(&b, "%s\n", authCheck)
	}
	fmt.Fprintf(&b, "echo \"$SHELL\"\n")
	fmt.Fprintf(&b, "```\n\n")
	for _, e := range extra {
		fmt.Fprintf(&b, "- %s\n", e)
	}
	if len(extra) > 0 {
		fmt.Fprintf(&b, "\n")
	}
	fmt.Fprintf(&b, "Keep what you learned in a small JSON object for `install_context`:\n\n")
	fmt.Fprintf(&b, "```json\n")
	fmt.Fprintf(&b, "{\n")
	fmt.Fprintf(&b, "  \"harness\": %q,\n", harness)
	fmt.Fprintf(&b, "  \"harness_version\": \"<x.y.z or omit>\",\n")
	fmt.Fprintf(&b, "  \"install_mode\": \"local\",\n")
	fmt.Fprintf(&b, "  \"host_os\": \"darwin | linux | windows\",\n")
	fmt.Fprintf(&b, "  \"auth_mode\": \"passthrough\",\n")
	fmt.Fprintf(&b, "  \"alias_intent\": \"none | safe | yolo\"\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "```\n\n")
	return b.String()
}

func sectionDashboardAnswers(ctx installerCtx, lines ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Dashboard answers\n\n")
	fmt.Fprintf(&b, "The user answered setup questions in the Clawvisor dashboard before launching this skill. Follow these choices; don't ask them again unless a command fails.\n\n")
	for _, line := range lines {
		if line != "" {
			fmt.Fprintf(&b, "- %s\n", line)
		}
	}
	switch ctx.TaskApproval {
	case "low":
		fmt.Fprintf(&b, "- Preferred task auto-approval default: low-risk conversation updates.\n")
	case "medium":
		fmt.Fprintf(&b, "- Preferred task auto-approval default: low- and medium-risk conversation updates.\n")
	default:
		fmt.Fprintf(&b, "- Preferred task auto-approval default: manual approval.\n")
	}
	fmt.Fprintf(&b, "\n")
	return b.String()
}

func sectionMint(harness, clawvisorURL, claim, userID string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 2. Mint a connection request\n\n")
	fmt.Fprintf(&b, "Pick a short, kebab-case name. The default `%s` is fine; suffix with a\n", harness)
	fmt.Fprintf(&b, "number (e.g. `%s-2`) if the user already has one with that name.\n\n", harness)
	fmt.Fprintf(&b, "Always mint a fresh connection request for this setup. Do not reuse a token\n")
	fmt.Fprintf(&b, "found on disk; the user is approving a new agent connection in the dashboard.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	url := clawvisorURL + "/api/agents/connect?wait=true&timeout=120"
	switch {
	case claim != "":
		url += "&claim=" + claim
	case userID != "":
		// User-ID fallback when no claim was minted (skill installed directly
		// via curl without a dashboard session). Still single-tenant-safe.
		url += "&user_id=" + userID
	}
	fmt.Fprintf(&b, "RESPONSE=$(curl -s -X POST %q \\\n", url)
	fmt.Fprintf(&b, "  -H \"Content-Type: application/json\" \\\n")
	fmt.Fprintf(&b, "  -d @- <<'JSON'\n")
	fmt.Fprintf(&b, "{\n")
	fmt.Fprintf(&b, "  \"name\": \"<picked name>\",\n")
	fmt.Fprintf(&b, "  \"description\": \"%s on <host_os>\",\n", harness)
	fmt.Fprintf(&b, "  \"install_context\": { ... fill in from Step 1 ... }\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "JSON\n")
	fmt.Fprintf(&b, ")\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Tell the user to look at their Clawvisor dashboard — the request appears\n")
	fmt.Fprintf(&b, "with the install context attached so they can see what you're connecting.\n")
	fmt.Fprintf(&b, "The curl blocks until they approve (or 120s elapses).\n\n")
	fmt.Fprintf(&b, "On approval, the response includes a `token` field:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "TOKEN=$(echo \"$RESPONSE\" | jq -r .token)\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If `$TOKEN` is `null` or empty, the request was denied or timed out. Surface\n")
	fmt.Fprintf(&b, "the response to the user and stop — don't retry without their go-ahead.\n\n")
	return b.String()
}

func sectionPersistToken(harness, name string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 3. Persist the token\n\n")
	fmt.Fprintf(&b, "Store the token on disk so the configure step (and future re-runs of this\n")
	fmt.Fprintf(&b, "target agent) can read it. The file is `chmod 600`.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "mkdir -p ~/.clawvisor/agents\n")
	fmt.Fprintf(&b, "AGENT_JSON=~/.clawvisor/agents/%s.json    # use the picked name\n", name)
	fmt.Fprintf(&b, "cat > \"$AGENT_JSON\" <<EOF\n")
	fmt.Fprintf(&b, "{\n")
	fmt.Fprintf(&b, "  \"name\": \"%s\",\n", name)
	fmt.Fprintf(&b, "  \"harness\": \"%s\",\n", harness)
	fmt.Fprintf(&b, "  \"installed_at\": \"$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)\",\n")
	fmt.Fprintf(&b, "  \"token\": \"$TOKEN\"\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, "chmod 600 \"$AGENT_JSON\"\n")
	fmt.Fprintf(&b, "```\n\n")
	return b.String()
}

func sectionSmokeTest(clawvisorURL string, step int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %d. Connectivity smoke test\n\n", step)
	fmt.Fprintf(&b, "Verify the token works. This is a *connectivity* check only — the policy-\n")
	fmt.Fprintf(&b, "enforcement demo (try an out-of-scope action and watch Clawvisor deny it)\n")
	fmt.Fprintf(&b, "lives in the agent's *first real use*, not in this skill, because **this\n")
	fmt.Fprintf(&b, "skill isn't running through Clawvisor**. The agent you just configured is.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "AGENT_JSON=${AGENT_JSON:-$HOME/.clawvisor/agents/<picked name>.json}\n")
	fmt.Fprintf(&b, "TOK=$(jq -r .token \"$AGENT_JSON\") && \\\n")
	fmt.Fprintf(&b, "  curl -sf -H \"X-Clawvisor-Agent-Token: $TOK\" \\\n")
	fmt.Fprintf(&b, "    \"%s/api/skill/catalog\" -o /dev/null && echo OK || echo REVOKED\n", clawvisorURL)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If you get `OK`, the connection works. If you get `REVOKED`, the token is\n")
	fmt.Fprintf(&b, "wrong or no longer active — re-check Step 4 wrote the right file and token.\n\n")
	return b.String()
}

func sectionUninstallDoc(harness, uninstallSteps string, step int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %d. Save an uninstall reference\n\n", step)
	fmt.Fprintf(&b, "Write a short doc the user can refer back to if they want to turn Clawvisor\n")
	fmt.Fprintf(&b, "off. Trust is built by making the exit easy.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "cat > ~/.clawvisor/uninstall-%s.md <<'EOF'\n", harness)
	fmt.Fprintf(&b, "# How to disconnect %s from Clawvisor\n\n", harness)
	fmt.Fprintf(&b, "%s", uninstallSteps)
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, "```\n\n")
	return b.String()
}

func sectionSelfUninstall(harness, skillRemovePath string, step int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %d. Self-uninstall automatically\n\n", step)
	fmt.Fprintf(&b, "Setup is done. Remove this installer skill now; it is one-shot setup\n")
	fmt.Fprintf(&b, "scaffolding and is not needed after the target agent is configured.\n")
	fmt.Fprintf(&b, "Run the command that matches the helper agent currently executing this\n")
	fmt.Fprintf(&b, "skill; ignore paths that do not exist.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "%s\n", skillRemovePath)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Tell the user:\n\n")
	fmt.Fprintf(&b, "- %s is now routed through Clawvisor.\n", harness)
	fmt.Fprintf(&b, "- Their first real interaction is where they'll see the policy-enforcement demo.\n")
	fmt.Fprintf(&b, "- The uninstall guide is at `~/.clawvisor/uninstall-%s.md` if they need to back out.\n", harness)
	return b.String()
}

func helperInstallerCleanupCommands() string {
	return `rm -f ~/.claude/commands/clawvisor-install.md
rm -rf ~/.codex/skills/clawvisor-install`
}

// ── Per-target renders ───────────────────────────────────────────────────────

func renderClaudeCodeInstaller(ctx installerCtx) string {
	var b strings.Builder
	b.WriteString(installerFrontmatter("Claude Code"))
	fmt.Fprintf(&b, "# Connect Claude Code to Clawvisor\n\n")
	fmt.Fprintf(&b, "You are walking the user through connecting Claude Code to a running\n")
	fmt.Fprintf(&b, "Clawvisor instance at `%s`. This is a one-shot skill: do the work,\n", ctx.ClawvisorURL)
	fmt.Fprintf(&b, "verify it, then remove yourself.\n\n")
	fmt.Fprintf(&b, "Claude Code runs in **passthrough mode**: the user's existing Anthropic\n")
	fmt.Fprintf(&b, "login (OAuth subscription or API key) authenticates upstream; Clawvisor\n")
	fmt.Fprintf(&b, "only identifies which agent is making the call. There's no upstream key\n")
	fmt.Fprintf(&b, "to vault.\n\n")
	fmt.Fprintf(&b, "Set the endpoint once for convenience:\n\n```bash\nexport CLAWVISOR_URL=%s\n```\n\n", ctx.ClawvisorURL)
	b.WriteString(sectionDashboardAnswers(ctx,
		"Claude Code routing scope: "+ctx.ClaudeScope,
		"Claude Code curl allow rule: "+ctx.ClaudeCurlAllow,
		"Alias mode: "+ctx.AliasMode,
	))

	b.WriteString(sectionLocalCLIProbe("claude-code", "claude --version", "", []string{
		"**Existing alias state** — does `~/.zshrc`/`~/.bashrc` already have a `claude-cv` function from a prior install?",
	}))
	b.WriteString(sectionMint("claude-code", ctx.ClawvisorURL, ctx.Claim, ctx.UserID))
	b.WriteString(sectionPersistToken("claude-code", "claude-code"))

	fmt.Fprintf(&b, "## 4. Configure Claude Code\n\n")
	fmt.Fprintf(&b, "Claude Code reads `ANTHROPIC_BASE_URL`, `ANTHROPIC_CUSTOM_HEADERS`, and\n")
	fmt.Fprintf(&b, "`ANTHROPIC_AUTH_TOKEN`/`ANTHROPIC_API_KEY` from the environment. We point\n")
	fmt.Fprintf(&b, "the base URL at Clawvisor and forward the agent token in a custom header;\n")
	fmt.Fprintf(&b, "the user's upstream auth flows through unchanged.\n\n")
	if ctx.ClaudeScope == "global" {
		fmt.Fprintf(&b, "The user chose **global routing**. Read `~/.claude/settings.json` (create\n")
		fmt.Fprintf(&b, "`{}` if it doesn't exist), merge the following into `env`, and write it\n")
		fmt.Fprintf(&b, "back. **Preserve every other key.**\n\n")
		fmt.Fprintf(&b, "```json\n")
		fmt.Fprintf(&b, "{\n")
		fmt.Fprintf(&b, "  \"env\": {\n")
		fmt.Fprintf(&b, "    \"ANTHROPIC_BASE_URL\": \"%s/api\",\n", ctx.ClawvisorURL)
		fmt.Fprintf(&b, "    \"ANTHROPIC_CUSTOM_HEADERS\": \"X-Clawvisor-Agent-Token: $TOKEN\",\n")
		fmt.Fprintf(&b, "    \"ANTHROPIC_AUTH_TOKEN\": \"\",\n")
		fmt.Fprintf(&b, "    \"ANTHROPIC_API_KEY\": \"\"\n")
		fmt.Fprintf(&b, "  }\n")
		fmt.Fprintf(&b, "}\n")
		fmt.Fprintf(&b, "```\n\n")
		fmt.Fprintf(&b, "Substitute `$TOKEN` with the actual value. The current Claude Code session\n")
		fmt.Fprintf(&b, "won't pick up changes until restarted — say so.\n\n")
	} else {
		fmt.Fprintf(&b, "The user chose **scoped routing**. Do not edit `~/.claude/settings.json`;\n")
		fmt.Fprintf(&b, "configure the `claude-cv` alias in Step 5 instead.\n\n")
	}
	if ctx.ClaudeCurlAllow == "yes" {
		fmt.Fprintf(&b, "The user chose to add a Clawvisor curl allow rule. Merge this into\n")
		fmt.Fprintf(&b, "`permissions.allow`:\n\n")
		fmt.Fprintf(&b, "```json\n")
		fmt.Fprintf(&b, "{\n")
		fmt.Fprintf(&b, "  \"permissions\": {\n")
		fmt.Fprintf(&b, "    \"allow\": [\n")
		fmt.Fprintf(&b, "      \"Bash(curl *%s/*)\"\n", ctx.ClawvisorURL)
		fmt.Fprintf(&b, "    ]\n")
		fmt.Fprintf(&b, "  }\n")
		fmt.Fprintf(&b, "}\n")
		fmt.Fprintf(&b, "```\n\n")
	} else {
		fmt.Fprintf(&b, "The user chose not to add a Claude Code curl allow rule. Leave permissions unchanged.\n\n")
	}

	fmt.Fprintf(&b, "## 5. Offer a shell alias\n\n")
	if ctx.AliasMode == "none" {
		fmt.Fprintf(&b, "The user chose not to create an alias. Skip this step.\n\n")
	} else {
		fmt.Fprintf(&b, "Create a shell function that is clearly Clawvisor-routed and leaves bare\n")
		fmt.Fprintf(&b, "`claude` untouched:\n\n")
		fmt.Fprintf(&b, "```bash\n")
		fmt.Fprintf(&b, "cat >> ~/.zshrc <<'EOF'\n")
		fmt.Fprintf(&b, "claude-cv() {\n")
		fmt.Fprintf(&b, "  ANTHROPIC_BASE_URL=%s/api \\\n", ctx.ClawvisorURL)
		fmt.Fprintf(&b, "  ANTHROPIC_CUSTOM_HEADERS=\"X-Clawvisor-Agent-Token: $(jq -r .token ~/.clawvisor/agents/claude-code.json)\" \\\n")
		fmt.Fprintf(&b, "  ANTHROPIC_AUTH_TOKEN= ANTHROPIC_API_KEY= \\\n")
		if ctx.AliasMode == "yolo" {
			fmt.Fprintf(&b, "  claude --dangerously-skip-permissions \"$@\"\n")
		} else {
			fmt.Fprintf(&b, "  claude \"$@\"\n")
		}
		fmt.Fprintf(&b, "}\n")
		fmt.Fprintf(&b, "EOF\n")
		fmt.Fprintf(&b, "```\n\n")
		fmt.Fprintf(&b, "Use `~/.bashrc` if the user is on bash; `~/.config/fish/config.fish` for\n")
		fmt.Fprintf(&b, "fish (the function syntax is different — translate).\n\n")
	}

	b.WriteString(sectionSmokeTest(ctx.ClawvisorURL, 6))

	b.WriteString(sectionUninstallDoc("claude-code", `1. If you chose global routing, remove env vars from `+"`~/.claude/settings.json`"+` (delete the four ANTHROPIC_* keys we added).
2. If you added it, remove the permission allow rule for `+"`Bash(curl *<clawvisor-url>/*)`"+`.
3. Remove the alias from your shell rc file if you added one: search for `+"`claude-cv()`"+` and delete that block.
4. Delete the token file: `+"`rm ~/.clawvisor/agents/claude-code.json`"+`.
5. Revoke the agent in the Clawvisor dashboard under Agents → claude-code → Delete.
`, 7))

	b.WriteString(sectionSelfUninstall("claude-code", helperInstallerCleanupCommands(), 8))

	return b.String()
}

func renderCodexInstaller(ctx installerCtx) string {
	var b strings.Builder
	b.WriteString(installerFrontmatter("Codex"))
	fmt.Fprintf(&b, "# Connect Codex to Clawvisor\n\n")
	fmt.Fprintf(&b, "You are walking the user through connecting OpenAI Codex CLI to a running\n")
	fmt.Fprintf(&b, "Clawvisor instance at `%s`. One-shot skill — do the work, verify, then\n", ctx.ClawvisorURL)
	fmt.Fprintf(&b, "remove yourself.\n\n")
	fmt.Fprintf(&b, "Codex runs in **passthrough mode**: the user's `codex login` (ChatGPT\n")
	fmt.Fprintf(&b, "subscription or API key) authenticates upstream; Clawvisor identifies\n")
	fmt.Fprintf(&b, "the agent via a header. No upstream key vaulting required.\n\n")
	fmt.Fprintf(&b, "Set the endpoint:\n\n```bash\nexport CLAWVISOR_URL=%s\n```\n\n", ctx.ClawvisorURL)
	b.WriteString(sectionDashboardAnswers(ctx, "Alias mode: "+ctx.AliasMode))
	fmt.Fprintf(&b, "**Prerequisite:** the user must have run `codex login` at least once.\n")
	fmt.Fprintf(&b, "Verify before proceeding:\n\n```bash\ncodex --version && ls ~/.codex/auth.json 2>/dev/null\n```\n\n")
	fmt.Fprintf(&b, "If `auth.json` is missing, stop and ask the user to run `codex login`.\n\n")

	b.WriteString(sectionLocalCLIProbe("codex", "codex --version", "test -f ~/.codex/auth.json", nil))
	b.WriteString(sectionMint("codex", ctx.ClawvisorURL, ctx.Claim, ctx.UserID))
	b.WriteString(sectionPersistToken("codex", "codex"))

	fmt.Fprintf(&b, "## 4. Configure Codex\n\n")
	fmt.Fprintf(&b, "Codex reads `~/.codex/config.toml`. We add a `[model_providers.clawvisor]`\n")
	fmt.Fprintf(&b, "block that points at Clawvisor, asks Codex to keep using the user's\n")
	fmt.Fprintf(&b, "existing OpenAI auth (`requires_openai_auth = true`), and forwards the\n")
	fmt.Fprintf(&b, "Clawvisor token via a custom header.\n\n")
	fmt.Fprintf(&b, "**Idempotency:** grep first; the block is a table, and Codex rejects\n")
	fmt.Fprintf(&b, "duplicate `[model_providers.<name>]` entries on startup.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "mkdir -p ~/.codex\n")
	fmt.Fprintf(&b, "grep -q '^\\[model_providers\\.clawvisor\\]' ~/.codex/config.toml 2>/dev/null \\\n")
	fmt.Fprintf(&b, "  || cat >> ~/.codex/config.toml <<'EOF'\n\n")
	fmt.Fprintf(&b, "[model_providers.clawvisor]\n")
	fmt.Fprintf(&b, "name = \"Clawvisor\"\n")
	fmt.Fprintf(&b, "base_url = \"%s/api/v1\"\n", ctx.ClawvisorURL)
	fmt.Fprintf(&b, "wire_api = \"responses\"\n")
	fmt.Fprintf(&b, "requires_openai_auth = true\n\n")
	fmt.Fprintf(&b, "[model_providers.clawvisor.env_http_headers]\n")
	fmt.Fprintf(&b, "X-Clawvisor-Agent-Token = \"CLAWVISOR_AGENT_TOKEN\"\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Codex picks this up on next launch. To invoke Codex through Clawvisor,\n")
	fmt.Fprintf(&b, "set `CLAWVISOR_AGENT_TOKEN` and pass `-c model_provider=clawvisor`:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "CLAWVISOR_AGENT_TOKEN=$(jq -r .token ~/.clawvisor/agents/codex.json) \\\n")
	fmt.Fprintf(&b, "  codex -c model_provider=clawvisor\n")
	fmt.Fprintf(&b, "```\n\n")

	fmt.Fprintf(&b, "## 5. Offer a shell alias\n\n")
	if ctx.AliasMode == "none" {
		fmt.Fprintf(&b, "The user chose not to create an alias. Skip this step.\n\n")
	} else {
		fmt.Fprintf(&b, "Create the requested shell function:\n\n")
		fmt.Fprintf(&b, "```bash\n")
		fmt.Fprintf(&b, "cat >> ~/.zshrc <<'EOF'\n")
		fmt.Fprintf(&b, "codex-cv() {\n")
		fmt.Fprintf(&b, "  CLAWVISOR_AGENT_TOKEN=$(jq -r .token ~/.clawvisor/agents/codex.json) \\\n")
		if ctx.AliasMode == "yolo" {
			fmt.Fprintf(&b, "  codex --dangerously-bypass-approvals-and-sandbox -c model_provider=clawvisor \"$@\"\n")
		} else {
			fmt.Fprintf(&b, "  codex -c model_provider=clawvisor \"$@\"\n")
		}
		fmt.Fprintf(&b, "}\n")
		fmt.Fprintf(&b, "EOF\n")
		fmt.Fprintf(&b, "```\n\n")
		fmt.Fprintf(&b, "Translate for bash/fish as needed.\n\n")
	}

	b.WriteString(sectionSmokeTest(ctx.ClawvisorURL, 6))

	b.WriteString(sectionUninstallDoc("codex", `1. Remove the `+"`[model_providers.clawvisor]`"+` block from `+"`~/.codex/config.toml`"+`.
2. Remove the alias from your shell rc file if you added one: search for `+"`codex-cv()`"+` and delete.
3. Delete the token file: `+"`rm ~/.clawvisor/agents/codex.json`"+`.
4. Revoke the agent in the Clawvisor dashboard under Agents → codex → Delete.
`, 7))

	b.WriteString(sectionSelfUninstall("codex", helperInstallerCleanupCommands(), 8))

	return b.String()
}

func renderHermesInstaller(ctx installerCtx) string {
	var b strings.Builder
	providerName := installerProviderDisplayName(ctx.LLMProvider)
	basePath := providerBasePath(ctx.LLMProvider)
	baseEnv := providerBaseEnv(ctx.LLMProvider)
	keyEnv := providerKeyEnv(ctx.LLMProvider)
	b.WriteString(installerFrontmatter("Hermes"))
	fmt.Fprintf(&b, "# Connect Hermes to Clawvisor\n\n")
	fmt.Fprintf(&b, "You are walking the user through connecting Hermes (Nous Research) to a\n")
	fmt.Fprintf(&b, "running Clawvisor instance at `%s`. One-shot — do, verify, offer to\n", ctx.ClawvisorURL)
	fmt.Fprintf(&b, "remove yourself.\n\n")
	fmt.Fprintf(&b, "Hermes runs in **swap mode**: Hermes presents the Clawvisor agent token as\n")
	fmt.Fprintf(&b, "`%s`; Clawvisor swaps in the user's\n", keyEnv)
	fmt.Fprintf(&b, "*vaulted upstream key* on each call. The dashboard step before this skill\n")
	fmt.Fprintf(&b, "collects the user's upstream %s API key.\n\n", providerName)
	fmt.Fprintf(&b, "Set the endpoint:\n\n```bash\nexport CLAWVISOR_URL=%s\n```\n\n", ctx.ClawvisorURL)

	b.WriteString(sectionProbe("hermes", nil))
	b.WriteString(sectionMint("hermes", ctx.ClawvisorURL, ctx.Claim, ctx.UserID))
	b.WriteString(sectionPersistToken("hermes", "hermes"))

	b.WriteString(sectionDashboardAnswers(ctx, "LLM provider: "+providerName, "Hermes configuration mode: "+ctx.HermesConfig))

	fmt.Fprintf(&b, "## 4. Configure Hermes\n\n")
	fmt.Fprintf(&b, "Hermes reads `~/.hermes/config.yaml`. Use the env-var run pattern for\n")
	fmt.Fprintf(&b, "dynamic token rotation, or persist the config for set-and-forget. Offer\n")
	fmt.Fprintf(&b, "both — the user picks.\n\n")
	if ctx.HermesConfig == "file" {
		fmt.Fprintf(&b, "The user chose the persistent config-file path. Prefer that snippet below.\n\n")
	} else {
		fmt.Fprintf(&b, "The user chose the env-var launch path. Prefer that snippet below.\n\n")
	}
	fmt.Fprintf(&b, "**Env-var (dynamic, recommended):**\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "%s=%s%s \\\n", baseEnv, ctx.ClawvisorURL, basePath)
	fmt.Fprintf(&b, "%s=$(jq -r .token ~/.clawvisor/agents/hermes.json) \\\n", keyEnv)
	fmt.Fprintf(&b, "hermes chat\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**Config file (persistent):**\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "mkdir -p ~/.hermes && cat > ~/.hermes/config.yaml <<EOF\n")
	fmt.Fprintf(&b, "model:\n")
	fmt.Fprintf(&b, "  provider: custom\n")
	fmt.Fprintf(&b, "  base_url: \"%s%s\"\n", ctx.ClawvisorURL, basePath)
	fmt.Fprintf(&b, "  api_key: \"$(jq -r .token ~/.clawvisor/agents/hermes.json)\"\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "The config-file path bakes the current token into the file; re-bootstrapping\n")
	fmt.Fprintf(&b, "the same agent rotates the token and the user must re-run this snippet.\n\n")

	fmt.Fprintf(&b, "## 5. Offer a shell alias\n\n")
	fmt.Fprintf(&b, "If they went the env-var route, a shell function keeps it ergonomic:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "cat >> ~/.zshrc <<'EOF'\n")
	fmt.Fprintf(&b, "hermes-cv() {\n")
	fmt.Fprintf(&b, "  %s=%s%s \\\n", baseEnv, ctx.ClawvisorURL, basePath)
	fmt.Fprintf(&b, "  %s=$(jq -r .token ~/.clawvisor/agents/hermes.json) \\\n", keyEnv)
	fmt.Fprintf(&b, "  hermes \"$@\"\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Hermes doesn't ship a documented bypass-prompts flag — skip the YOLO\n")
	fmt.Fprintf(&b, "question unless the user volunteers one they know about.\n\n")

	b.WriteString(sectionSmokeTest(ctx.ClawvisorURL, 6))

	b.WriteString(sectionUninstallDoc("hermes", `1. Remove the `+"`model:`"+` block from `+"`~/.hermes/config.yaml`"+` (or unset `+"`"+baseEnv+"`"+`/`+"`"+keyEnv+"`"+` if you used env vars).
2. Remove the alias from your shell rc file if you added one.
3. Delete the token file: `+"`rm ~/.clawvisor/agents/hermes.json`"+`.
4. Revoke the agent in the Clawvisor dashboard under Agents → hermes → Delete.
5. Optional: remove the user-level `+providerName+` key from Clawvisor credentials if no other agents use it.
`, 7))

	b.WriteString(sectionSelfUninstall("hermes", helperInstallerCleanupCommands(), 8))

	return b.String()
}

func renderOpenClawInstaller(ctx installerCtx) string {
	var b strings.Builder
	providerName := installerProviderDisplayName(ctx.LLMProvider)
	b.WriteString(installerFrontmatter("OpenClaw"))
	fmt.Fprintf(&b, "# Connect OpenClaw to Clawvisor\n\n")
	fmt.Fprintf(&b, "You are walking the user through connecting an OpenClaw instance to a\n")
	fmt.Fprintf(&b, "running Clawvisor at `%s`. The setup is intentionally simple: point\n", ctx.ClawvisorURL)
	fmt.Fprintf(&b, "OpenClaw's LLM base URL at Clawvisor's %s-compatible endpoint and\n", providerName)
	fmt.Fprintf(&b, "use the minted Clawvisor agent token as the custom API key. This skill is\n")
	fmt.Fprintf(&b, "one-shot. The dashboard step before this skill collects the user's upstream\n")
	fmt.Fprintf(&b, "%s API key so Clawvisor can forward OpenClaw's model calls.\n\n", providerName)
	fmt.Fprintf(&b, "Set the endpoint:\n\n```bash\nexport CLAWVISOR_URL=%s\n```\n\n", ctx.ClawvisorURL)
	b.WriteString(sectionDashboardAnswers(ctx, "LLM provider: "+providerName, "OpenClaw running mode: "+ctx.OpenClawMode))

	if ctx.OpenClawMode == "remote" {
		b.WriteString(sectionOpenClawRemoteProbe(ctx.LLMProvider))
	} else {
		b.WriteString(sectionOpenClawLocalProbe(ctx.OpenClawMode, ctx.LLMProvider))
	}

	b.WriteString(sectionMint("openclaw", ctx.ClawvisorURL, ctx.Claim, ctx.UserID))

	b.WriteString(sectionPersistToken("openclaw", "openclaw"))

	if ctx.OpenClawMode == "remote" {
		b.WriteString(sectionOpenClawRemoteConfigure(ctx.ClawvisorURL, ctx.LLMProvider))
	} else {
		b.WriteString(sectionOpenClawLocalConfigure(ctx.ClawvisorURL, ctx.LLMProvider))
	}

	b.WriteString(sectionSmokeTest(ctx.ClawvisorURL, 5))

	b.WriteString(sectionUninstallDoc("openclaw", `1. Re-run OpenClaw onboarding and choose your previous non-Clawvisor provider/base URL.
2. Delete the token file: `+"`rm ~/.clawvisor/agents/openclaw.json`"+`.
3. Revoke the agent in the Clawvisor dashboard under Agents → openclaw → Delete.
`, 6))

	b.WriteString(sectionSelfUninstall("openclaw", helperInstallerCleanupCommands(), 7))

	return b.String()
}

func sectionOpenClawLocalProbe(mode, provider string) string {
	var b strings.Builder
	model := providerDefaultModel(provider)
	providerName := installerProviderDisplayName(provider)
	fmt.Fprintf(&b, "## 1. Confirm how to run OpenClaw onboarding\n\n")
	fmt.Fprintf(&b, "Do not install extra OpenClaw components. Only determine how the user runs\n")
	fmt.Fprintf(&b, "OpenClaw's existing onboarding command.\n\n")
	fmt.Fprintf(&b, "Determine:\n\n")
	if mode == "docker" {
		fmt.Fprintf(&b, "- **Docker command** — confirm the compose service/working directory for `openclaw-cli`.\n")
	} else {
		fmt.Fprintf(&b, "- **Host command** — confirm `openclaw-cli` is available on this machine.\n")
		fmt.Fprintf(&b, "- **Docker fallback** — if OpenClaw is actually containerized, use the Docker command in Step 4 instead.\n")
	}
	fmt.Fprintf(&b, "- **Model id** — default to `%s` unless the user prefers another Clawvisor-routed %s model.\n", model, providerName)
	fmt.Fprintf(&b, "- **Shell** — zsh, bash, or fish, only if you need to save a convenience command.\n\n")
	fmt.Fprintf(&b, "Keep what you learned in a JSON object for `install_context`:\n\n")
	fmt.Fprintf(&b, "```json\n")
	fmt.Fprintf(&b, "{\n")
	fmt.Fprintf(&b, "  \"harness\": \"openclaw\",\n")
	if mode == "docker" {
		fmt.Fprintf(&b, "  \"install_mode\": \"docker\",\n")
	} else {
		fmt.Fprintf(&b, "  \"install_mode\": \"host\",\n")
	}
	fmt.Fprintf(&b, "  \"model_id\": %q,\n", model)
	fmt.Fprintf(&b, "  \"provider\": %q,\n", provider)
	fmt.Fprintf(&b, "  \"auth_mode\": \"custom-api-key\"\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "```\n\n")
	return b.String()
}

func sectionOpenClawRemoteProbe(provider string) string {
	var b strings.Builder
	model := providerDefaultModel(provider)
	fmt.Fprintf(&b, "## 1. Confirm remote OpenClaw access\n\n")
	fmt.Fprintf(&b, "The user selected **remote host** in the dashboard. Do **not** probe the\n")
	fmt.Fprintf(&b, "local machine for OpenClaw files or Docker containers;\n")
	fmt.Fprintf(&b, "that would inspect the helper agent's machine, not the OpenClaw host.\n\n")
	fmt.Fprintf(&b, "Ask the user for the remote access details you need, then keep them in\n")
	fmt.Fprintf(&b, "shell variables for the commands below:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "export OPENCLAW_REMOTE='<ssh host, for example user@example.com>'\n")
	fmt.Fprintf(&b, "export OPENCLAW_WORKSPACE='~/.openclaw/workspace'\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If SSH is unavailable, do not invent local commands. Give the user the\n")
	fmt.Fprintf(&b, "remote commands from later steps to run on the OpenClaw host and ask them\n")
	fmt.Fprintf(&b, "to paste back any output or errors.\n\n")
	fmt.Fprintf(&b, "Verify only how onboarding is run on the remote host:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "ssh \"$OPENCLAW_REMOTE\" 'uname -s; command -v openclaw-cli || true; docker compose ps 2>/dev/null || true'\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Keep what you learned in a JSON object for `install_context`:\n\n")
	fmt.Fprintf(&b, "```json\n")
	fmt.Fprintf(&b, "{\n")
	fmt.Fprintf(&b, "  \"harness\": \"openclaw\",\n")
	fmt.Fprintf(&b, "  \"install_mode\": \"remote\",\n")
	fmt.Fprintf(&b, "  \"remote_host\": \"<hostname or description>\",\n")
	fmt.Fprintf(&b, "  \"host_os\": \"darwin | linux | windows\",\n")
	fmt.Fprintf(&b, "  \"model_id\": %q,\n", model)
	fmt.Fprintf(&b, "  \"provider\": %q,\n", provider)
	fmt.Fprintf(&b, "  \"auth_mode\": \"custom-api-key\"\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "```\n\n")
	return b.String()
}

func sectionOpenClawLocalConfigure(clawvisorURL, provider string) string {
	var b strings.Builder
	basePath := "/api/v1"
	model := providerDefaultModel(provider)
	contextWindow := providerDefaultContextWindow(provider)
	maxTokens := openClawDefaultMaxTokens()
	fmt.Fprintf(&b, "## 4. Point OpenClaw at Clawvisor\n\n")
	fmt.Fprintf(&b, "Run OpenClaw's onboarding command and select a custom API key provider.\n")
	fmt.Fprintf(&b, "Use Clawvisor's %s-compatible base URL and the minted `cvis_...`\n", installerProviderDisplayName(provider))
	fmt.Fprintf(&b, "agent token. For Docker, use a host-reachable URL such as\n")
	fmt.Fprintf(&b, "`http://host.docker.internal:25297%s` when Clawvisor is on the host.\n\n", basePath)
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "# Host OpenClaw:\n")
	fmt.Fprintf(&b, "openclaw-cli onboard --non-interactive \\\n")
	fmt.Fprintf(&b, "  --auth-choice custom-api-key \\\n")
	fmt.Fprintf(&b, "  --custom-base-url \"%s%s\" \\\n", clawvisorURL, basePath)
	fmt.Fprintf(&b, "  --custom-model-id \"%s\" \\\n", model)
	fmt.Fprintf(&b, "  --custom-api-key \"$TOKEN\" \\\n")
	fmt.Fprintf(&b, "  --custom-compatibility %s --accept-risk\n\n", provider)
	fmt.Fprintf(&b, "# Docker OpenClaw, when Clawvisor is running on the host:\n")
	fmt.Fprintf(&b, "docker compose run --rm openclaw-cli onboard --non-interactive \\\n")
	fmt.Fprintf(&b, "  --auth-choice custom-api-key \\\n")
	fmt.Fprintf(&b, "  --custom-base-url \"http://host.docker.internal:25297%s\" \\\n", basePath)
	fmt.Fprintf(&b, "  --custom-model-id \"%s\" \\\n", model)
	fmt.Fprintf(&b, "  --custom-api-key \"$TOKEN\" \\\n")
	fmt.Fprintf(&b, "  --custom-compatibility %s --accept-risk\n", provider)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Then patch OpenClaw's custom-provider model metadata so it does not keep\n")
	fmt.Fprintf(&b, "the low fallback context window written by some OpenClaw versions. Run the\n")
	fmt.Fprintf(&b, "patch in the same environment that owns OpenClaw's `models.json` (host for\n")
	fmt.Fprintf(&b, "host installs; the OpenClaw container/volume for Docker installs). If you\n")
	fmt.Fprintf(&b, "changed the model ID above, set `OPENCLAW_MODEL_CONTEXT_WINDOW` to that\n")
	fmt.Fprintf(&b, "model's native maximum before running the patch.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "OPENCLAW_MODEL_ID=%q\n", model)
	fmt.Fprintf(&b, "OPENCLAW_MODEL_CONTEXT_WINDOW=%d\n", contextWindow)
	fmt.Fprintf(&b, "OPENCLAW_MAX_TOKENS=%d\n", maxTokens)
	fmt.Fprintf(&b, "OPENCLAW_MODELS_JSON=${OPENCLAW_MODELS_JSON:-$(find \"${OPENCLAW_STATE_DIR:-$HOME/.openclaw}/agents\" -path '*/agent/models.json' -print | sort | tail -n 1)}\n")
	fmt.Fprintf(&b, "test -n \"$OPENCLAW_MODELS_JSON\" && test -f \"$OPENCLAW_MODELS_JSON\"\n")
	fmt.Fprintf(&b, "tmp=$(mktemp)\n")
	fmt.Fprintf(&b, "jq --arg model \"$OPENCLAW_MODEL_ID\" \\\n")
	fmt.Fprintf(&b, "  --argjson contextWindow \"$OPENCLAW_MODEL_CONTEXT_WINDOW\" \\\n")
	fmt.Fprintf(&b, "  --argjson maxTokens \"$OPENCLAW_MAX_TOKENS\" '\n")
	fmt.Fprintf(&b, "  def patchProvider:\n")
	fmt.Fprintf(&b, "    .models |= ((. // []) | map(if .id == $model then . + {\n")
	fmt.Fprintf(&b, "      contextWindow: $contextWindow,\n")
	fmt.Fprintf(&b, "      maxTokens: $maxTokens\n")
	fmt.Fprintf(&b, "    } else . end));\n")
	fmt.Fprintf(&b, "  if .models.providers then\n")
	fmt.Fprintf(&b, "    .models.providers |= with_entries(.value |= patchProvider)\n")
	fmt.Fprintf(&b, "  elif .providers then\n")
	fmt.Fprintf(&b, "    .providers |= with_entries(.value |= patchProvider)\n")
	fmt.Fprintf(&b, "  else\n")
	fmt.Fprintf(&b, "    error(\"No OpenClaw provider registry found\")\n")
	fmt.Fprintf(&b, "  end\n")
	fmt.Fprintf(&b, "' \"$OPENCLAW_MODELS_JSON\" > \"$tmp\" && mv \"$tmp\" \"$OPENCLAW_MODELS_JSON\"\n")
	fmt.Fprintf(&b, "jq -e --arg model \"$OPENCLAW_MODEL_ID\" \\\n")
	fmt.Fprintf(&b, "  --argjson contextWindow \"$OPENCLAW_MODEL_CONTEXT_WINDOW\" \\\n")
	fmt.Fprintf(&b, "  --argjson maxTokens \"$OPENCLAW_MAX_TOKENS\" '\n")
	fmt.Fprintf(&b, "  (if .models.providers then .models.providers elif .providers then .providers else {} end)\n")
	fmt.Fprintf(&b, "  | to_entries\n")
	fmt.Fprintf(&b, "  | any(.[]; any(.value.models[]?; .id == $model and .contextWindow == $contextWindow and .maxTokens == $maxTokens))\n")
	fmt.Fprintf(&b, "' \"$OPENCLAW_MODELS_JSON\" >/dev/null\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If Clawvisor is not on the host, replace the base URL with the URL that\n")
	fmt.Fprintf(&b, "the OpenClaw process can reach. The important part is `%s`.\n\n", basePath)
	return b.String()
}

func sectionOpenClawRemoteConfigure(clawvisorURL, provider string) string {
	var b strings.Builder
	basePath := "/api/v1"
	model := providerDefaultModel(provider)
	contextWindow := providerDefaultContextWindow(provider)
	maxTokens := openClawDefaultMaxTokens()
	fmt.Fprintf(&b, "## 4. Point remote OpenClaw at Clawvisor\n\n")
	fmt.Fprintf(&b, "Because OpenClaw is remote, `localhost` on that host is not this helper\n")
	fmt.Fprintf(&b, "machine. Pick a Clawvisor URL the remote host can actually reach. The\n")
	fmt.Fprintf(&b, "dashboard rendered `%s`; if that is a localhost URL, replace it with a\n", clawvisorURL)
	fmt.Fprintf(&b, "relay, public, VPN, or LAN URL reachable from `$OPENCLAW_REMOTE`.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "export OPENCLAW_CLAWVISOR_BASE_URL='<remote-reachable Clawvisor URL>%s'\n\n", basePath)
	fmt.Fprintf(&b, "# Remote host OpenClaw:\n")
	fmt.Fprintf(&b, "ssh \"$OPENCLAW_REMOTE\" \"openclaw-cli onboard --non-interactive \\\n")
	fmt.Fprintf(&b, "  --auth-choice custom-api-key \\\n")
	fmt.Fprintf(&b, "  --custom-base-url '$OPENCLAW_CLAWVISOR_BASE_URL' \\\n")
	fmt.Fprintf(&b, "  --custom-model-id '%s' \\\n", model)
	fmt.Fprintf(&b, "  --custom-api-key '$TOKEN' \\\n")
	fmt.Fprintf(&b, "  --custom-compatibility %s --accept-risk\"\n\n", provider)
	fmt.Fprintf(&b, "# Remote Docker OpenClaw, if OpenClaw is containerized on that host:\n")
	fmt.Fprintf(&b, "ssh \"$OPENCLAW_REMOTE\" \"docker compose run --rm openclaw-cli onboard --non-interactive \\\n")
	fmt.Fprintf(&b, "  --auth-choice custom-api-key \\\n")
	fmt.Fprintf(&b, "  --custom-base-url '$OPENCLAW_CLAWVISOR_BASE_URL' \\\n")
	fmt.Fprintf(&b, "  --custom-model-id '%s' \\\n", model)
	fmt.Fprintf(&b, "  --custom-api-key '$TOKEN' \\\n")
	fmt.Fprintf(&b, "  --custom-compatibility %s --accept-risk\"\n", provider)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Then patch the remote OpenClaw custom-provider model metadata so it does\n")
	fmt.Fprintf(&b, "not keep the low fallback context window written by some OpenClaw versions.\n")
	fmt.Fprintf(&b, "If you changed the model ID above, set `OPENCLAW_MODEL_CONTEXT_WINDOW` to\n")
	fmt.Fprintf(&b, "that model's native maximum before running the patch.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "ssh \"$OPENCLAW_REMOTE\" 'OPENCLAW_MODEL_ID=%q OPENCLAW_MODEL_CONTEXT_WINDOW=%d OPENCLAW_MAX_TOKENS=%d sh -s' <<'REMOTE_OPENCLAW_PATCH'\n", model, contextWindow, maxTokens)
	fmt.Fprintf(&b, "set -eu\n")
	fmt.Fprintf(&b, "OPENCLAW_MODELS_JSON=${OPENCLAW_MODELS_JSON:-$(find \"${OPENCLAW_STATE_DIR:-$HOME/.openclaw}/agents\" -path '*/agent/models.json' -print | sort | tail -n 1)}\n")
	fmt.Fprintf(&b, "test -n \"$OPENCLAW_MODELS_JSON\" && test -f \"$OPENCLAW_MODELS_JSON\"\n")
	fmt.Fprintf(&b, "tmp=$(mktemp)\n")
	fmt.Fprintf(&b, "jq --arg model \"$OPENCLAW_MODEL_ID\" \\\n")
	fmt.Fprintf(&b, "  --argjson contextWindow \"$OPENCLAW_MODEL_CONTEXT_WINDOW\" \\\n")
	fmt.Fprintf(&b, "  --argjson maxTokens \"$OPENCLAW_MAX_TOKENS\" '\n")
	fmt.Fprintf(&b, "  def patchProvider:\n")
	fmt.Fprintf(&b, "    .models |= ((. // []) | map(if .id == $model then . + {\n")
	fmt.Fprintf(&b, "      contextWindow: $contextWindow,\n")
	fmt.Fprintf(&b, "      maxTokens: $maxTokens\n")
	fmt.Fprintf(&b, "    } else . end));\n")
	fmt.Fprintf(&b, "  if .models.providers then\n")
	fmt.Fprintf(&b, "    .models.providers |= with_entries(.value |= patchProvider)\n")
	fmt.Fprintf(&b, "  elif .providers then\n")
	fmt.Fprintf(&b, "    .providers |= with_entries(.value |= patchProvider)\n")
	fmt.Fprintf(&b, "  else\n")
	fmt.Fprintf(&b, "    error(\"No OpenClaw provider registry found\")\n")
	fmt.Fprintf(&b, "  end\n")
	fmt.Fprintf(&b, "' \"$OPENCLAW_MODELS_JSON\" > \"$tmp\" && mv \"$tmp\" \"$OPENCLAW_MODELS_JSON\"\n")
	fmt.Fprintf(&b, "jq -e --arg model \"$OPENCLAW_MODEL_ID\" \\\n")
	fmt.Fprintf(&b, "  --argjson contextWindow \"$OPENCLAW_MODEL_CONTEXT_WINDOW\" \\\n")
	fmt.Fprintf(&b, "  --argjson maxTokens \"$OPENCLAW_MAX_TOKENS\" '\n")
	fmt.Fprintf(&b, "  (if .models.providers then .models.providers elif .providers then .providers else {} end)\n")
	fmt.Fprintf(&b, "  | to_entries\n")
	fmt.Fprintf(&b, "  | any(.[]; any(.value.models[]?; .id == $model and .contextWindow == $contextWindow and .maxTokens == $maxTokens))\n")
	fmt.Fprintf(&b, "' \"$OPENCLAW_MODELS_JSON\" >/dev/null\n")
	fmt.Fprintf(&b, "REMOTE_OPENCLAW_PATCH\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "The important invariant is that OpenClaw's model requests go to Clawvisor\n")
	fmt.Fprintf(&b, "and use the minted `cvis_...` token.\n\n")
	return b.String()
}
