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
	ClaudeScope     string
	ClaudeCurlAllow string
	AliasMode       string
	HermesConfig    string
	OpenClawMode    string
	PolicySetup     string
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
	ctx.PolicySetup = queryChoice(r, "policy_setup", "after", "after", "later")
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
// at startup); Hermes and the openclaw clawhub format use the same shape; Claude
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
// Every installer skill follows the same outline: probe → reuse-check → mint →
// persist → configure → alias → smoke test → uninstall reference →
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
	if ctx.PolicySetup == "after" {
		fmt.Fprintf(&b, "- After installation, remind the user to configure restrictions in the Clawvisor dashboard.\n")
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

func sectionReuseCheck(harness, clawvisorURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 2. Check for an existing token (skip Step 3 if you reuse)\n\n")
	fmt.Fprintf(&b, "If the user already minted a token for this harness previously, reuse it\n")
	fmt.Fprintf(&b, "rather than minting a fresh one — it avoids cluttering Clawvisor with\n")
	fmt.Fprintf(&b, "duplicate agent records.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "ls ~/.clawvisor/agents/*.json 2>/dev/null\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "For each file, peek at the `harness` field. A candidate matches when:\n\n")
	fmt.Fprintf(&b, "- the file's `harness` value equals `%s`, OR\n", harness)
	fmt.Fprintf(&b, "- the filename starts with `%s` and the file has no `harness` field (older format).\n\n", harness)
	fmt.Fprintf(&b, "Verify the candidate token is still live:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "TOK=$(jq -r .token ~/.clawvisor/agents/<name>.json)\n")
	fmt.Fprintf(&b, "curl -sf -H \"X-Clawvisor-Agent-Token: $TOK\" \\\n")
	fmt.Fprintf(&b, "  \"%s/api/skill/catalog\" -o /dev/null && echo OK || echo REVOKED\n", clawvisorURL)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If OK: ask the user whether to reuse this token or mint a new one. On reuse,\n")
	fmt.Fprintf(&b, "set `TOKEN=$TOK`, set `reuse: true` in `install_context`, and jump to Step 4.\n")
	fmt.Fprintf(&b, "If REVOKED: fall through and mint fresh.\n\n")
	return b.String()
}

func sectionMint(harness, clawvisorURL, claim, userID string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 3. Mint a connection request\n\n")
	fmt.Fprintf(&b, "Pick a short, kebab-case name. The default `%s` is fine; suffix with a\n", harness)
	fmt.Fprintf(&b, "number (e.g. `%s-2`) if the user already has one with that name.\n\n", harness)
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
	fmt.Fprintf(&b, "## 4. Persist the token\n\n")
	fmt.Fprintf(&b, "Store the token on disk so the configure step (and future re-runs of this\n")
	fmt.Fprintf(&b, "skill) can read it back without re-minting. The file is `chmod 600`.\n\n")
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
	fmt.Fprintf(&b, "Reuse path (Step 2): you already have `$TOKEN` and the file — skip this step.\n\n")
	return b.String()
}

func sectionSmokeTest(clawvisorURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 7. Connectivity smoke test\n\n")
	fmt.Fprintf(&b, "Verify the token works. This is a *connectivity* check only — the policy-\n")
	fmt.Fprintf(&b, "enforcement demo (try an out-of-scope action and watch Clawvisor deny it)\n")
	fmt.Fprintf(&b, "lives in the agent's *first real use*, not in this skill, because **this\n")
	fmt.Fprintf(&b, "skill isn't running through Clawvisor**. The agent you just configured is.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "curl -sf -H \"X-Clawvisor-Agent-Token: $TOKEN\" \\\n")
	fmt.Fprintf(&b, "  \"%s/api/skill/catalog\" | jq '.services[0:3]'\n", clawvisorURL)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If you get a JSON service catalog back, the connection works. If you get a\n")
	fmt.Fprintf(&b, "401, the token is wrong — re-check Step 4 wrote the right value.\n\n")
	return b.String()
}

func sectionUninstallDoc(harness, uninstallSteps string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 8. Save an uninstall reference\n\n")
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

func sectionSelfUninstall(harness, skillRemovePath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 9. Self-uninstall automatically\n\n")
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
	b.WriteString(sectionReuseCheck("claude-code", ctx.ClawvisorURL))
	b.WriteString(sectionMint("claude-code", ctx.ClawvisorURL, ctx.Claim, ctx.UserID))
	b.WriteString(sectionPersistToken("claude-code", "claude-code"))

	fmt.Fprintf(&b, "## 5. Configure Claude Code\n\n")
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
		fmt.Fprintf(&b, "configure the `claude-cv` alias in Step 6 instead.\n\n")
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

	fmt.Fprintf(&b, "## 6. Offer a shell alias\n\n")
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

	b.WriteString(sectionSmokeTest(ctx.ClawvisorURL))

	b.WriteString(sectionUninstallDoc("claude-code", `1. If you chose global routing, remove env vars from `+"`~/.claude/settings.json`"+` (delete the four ANTHROPIC_* keys we added).
2. If you added it, remove the permission allow rule for `+"`Bash(curl *<clawvisor-url>/*)`"+`.
3. Remove the alias from your shell rc file if you added one: search for `+"`claude-cv()`"+` and delete that block.
4. Delete the token file: `+"`rm ~/.clawvisor/agents/claude-code.json`"+`.
5. Revoke the agent in the Clawvisor dashboard under Agents → claude-code → Delete.
`))

	b.WriteString(sectionSelfUninstall("claude-code", helperInstallerCleanupCommands()))

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
	b.WriteString(sectionReuseCheck("codex", ctx.ClawvisorURL))
	b.WriteString(sectionMint("codex", ctx.ClawvisorURL, ctx.Claim, ctx.UserID))
	b.WriteString(sectionPersistToken("codex", "codex"))

	fmt.Fprintf(&b, "## 5. Configure Codex\n\n")
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

	fmt.Fprintf(&b, "## 6. Offer a shell alias\n\n")
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

	b.WriteString(sectionSmokeTest(ctx.ClawvisorURL))

	b.WriteString(sectionUninstallDoc("codex", `1. Remove the `+"`[model_providers.clawvisor]`"+` block from `+"`~/.codex/config.toml`"+`.
2. Remove the alias from your shell rc file if you added one: search for `+"`codex-cv()`"+` and delete.
3. Delete the token file: `+"`rm ~/.clawvisor/agents/codex.json`"+`.
4. Revoke the agent in the Clawvisor dashboard under Agents → codex → Delete.
`))

	b.WriteString(sectionSelfUninstall("codex", helperInstallerCleanupCommands()))

	return b.String()
}

func renderHermesInstaller(ctx installerCtx) string {
	var b strings.Builder
	b.WriteString(installerFrontmatter("Hermes"))
	fmt.Fprintf(&b, "# Connect Hermes to Clawvisor\n\n")
	fmt.Fprintf(&b, "You are walking the user through connecting Hermes (Nous Research) to a\n")
	fmt.Fprintf(&b, "running Clawvisor instance at `%s`. One-shot — do, verify, offer to\n", ctx.ClawvisorURL)
	fmt.Fprintf(&b, "remove yourself.\n\n")
	fmt.Fprintf(&b, "Hermes runs in **swap mode**: Hermes is OpenAI-compatible and presents the\n")
	fmt.Fprintf(&b, "Clawvisor agent token as `OPENAI_API_KEY`; Clawvisor swaps in the user's\n")
	fmt.Fprintf(&b, "*vaulted upstream key* on each call. **The user must vault their upstream\n")
	fmt.Fprintf(&b, "OpenAI key in the Clawvisor dashboard before Hermes can make any calls.**\n\n")
	fmt.Fprintf(&b, "Set the endpoint:\n\n```bash\nexport CLAWVISOR_URL=%s\n```\n\n", ctx.ClawvisorURL)

	b.WriteString(sectionProbe("hermes", nil))
	b.WriteString(sectionReuseCheck("hermes", ctx.ClawvisorURL))
	b.WriteString(sectionMint("hermes", ctx.ClawvisorURL, ctx.Claim, ctx.UserID))
	b.WriteString(sectionPersistToken("hermes", "hermes"))

	fmt.Fprintf(&b, "## 5. Ask the user to vault their upstream key\n\n")
	fmt.Fprintf(&b, "Hermes can't pass through to OpenAI without a vaulted upstream key. Direct\n")
	fmt.Fprintf(&b, "the user to the dashboard:\n\n")
	fmt.Fprintf(&b, "    %s/dashboard/agents (find the just-approved `hermes` agent → \"Vault upstream key\")\n\n", ctx.ClawvisorURL)
	fmt.Fprintf(&b, "Poll the agent's credential status until it reports at least one stored key:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "AGENT_ID=$(echo \"$RESPONSE\" | jq -r .agent_id)  # captured at mint time\n")
	fmt.Fprintf(&b, "while :; do\n")
	fmt.Fprintf(&b, "  STORED=$(curl -sf -H \"X-Clawvisor-Agent-Token: $TOKEN\" \\\n")
	fmt.Fprintf(&b, "    \"%s/api/agents/$AGENT_ID/llm-credentials\" | jq '[.credentials[] | select(.stored or .agent_stored)] | length')\n", ctx.ClawvisorURL)
	fmt.Fprintf(&b, "  [ \"${STORED:-0}\" -gt 0 ] && break\n")
	fmt.Fprintf(&b, "  sleep 3\n")
	fmt.Fprintf(&b, "done\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If the user wants to skip vaulting (e.g. they'll do it later or use a\n")
	fmt.Fprintf(&b, "user-level credential), let them — just warn that calls will fail until\n")
	fmt.Fprintf(&b, "a key is stored.\n\n")
	b.WriteString(sectionDashboardAnswers(ctx, "Hermes configuration mode: "+ctx.HermesConfig))

	fmt.Fprintf(&b, "## 6. Configure Hermes\n\n")
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
	fmt.Fprintf(&b, "OPENAI_BASE_URL=%s/api/v1 \\\n", ctx.ClawvisorURL)
	fmt.Fprintf(&b, "OPENAI_API_KEY=$(jq -r .token ~/.clawvisor/agents/hermes.json) \\\n")
	fmt.Fprintf(&b, "hermes chat\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**Config file (persistent):**\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "mkdir -p ~/.hermes && cat > ~/.hermes/config.yaml <<EOF\n")
	fmt.Fprintf(&b, "model:\n")
	fmt.Fprintf(&b, "  provider: custom\n")
	fmt.Fprintf(&b, "  base_url: \"%s/api/v1\"\n", ctx.ClawvisorURL)
	fmt.Fprintf(&b, "  api_key: \"$(jq -r .token ~/.clawvisor/agents/hermes.json)\"\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "The config-file path bakes the current token into the file; re-bootstrapping\n")
	fmt.Fprintf(&b, "the same agent rotates the token and the user must re-run this snippet.\n\n")

	fmt.Fprintf(&b, "## 6.5. Offer a shell alias\n\n")
	fmt.Fprintf(&b, "If they went the env-var route, a shell function keeps it ergonomic:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "cat >> ~/.zshrc <<'EOF'\n")
	fmt.Fprintf(&b, "hermes-cv() {\n")
	fmt.Fprintf(&b, "  OPENAI_BASE_URL=%s/api/v1 \\\n", ctx.ClawvisorURL)
	fmt.Fprintf(&b, "  OPENAI_API_KEY=$(jq -r .token ~/.clawvisor/agents/hermes.json) \\\n")
	fmt.Fprintf(&b, "  hermes \"$@\"\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Hermes doesn't ship a documented bypass-prompts flag — skip the YOLO\n")
	fmt.Fprintf(&b, "question unless the user volunteers one they know about.\n\n")

	b.WriteString(sectionSmokeTest(ctx.ClawvisorURL))

	b.WriteString(sectionUninstallDoc("hermes", `1. Remove the `+"`model:`"+` block from `+"`~/.hermes/config.yaml`"+` (or unset OPENAI_BASE_URL/OPENAI_API_KEY if you used env vars).
2. Remove the alias from your shell rc file if you added one.
3. Delete the token file: `+"`rm ~/.clawvisor/agents/hermes.json`"+`.
4. Revoke the agent in the Clawvisor dashboard under Agents → hermes → Delete.
5. Optional: remove the vaulted upstream key from the agent's credentials panel.
`))

	b.WriteString(sectionSelfUninstall("hermes", helperInstallerCleanupCommands()))

	return b.String()
}

func renderOpenClawInstaller(ctx installerCtx) string {
	var b strings.Builder
	b.WriteString(installerFrontmatter("OpenClaw"))
	fmt.Fprintf(&b, "# Connect OpenClaw to Clawvisor\n\n")
	fmt.Fprintf(&b, "You are walking the user through connecting an OpenClaw instance to a\n")
	fmt.Fprintf(&b, "running Clawvisor at `%s`. OpenClaw uses Clawvisor as a **tool gateway**\n", ctx.ClawvisorURL)
	fmt.Fprintf(&b, "(not an LLM proxy) — it calls Clawvisor for authorized tool actions and\n")
	fmt.Fprintf(&b, "receives async approvals via a webhook callback. This skill is one-shot.\n\n")
	fmt.Fprintf(&b, "**Prerequisite:** the dedicated guide at `docs/INTEGRATE_OPENCLAW.md` in\n")
	fmt.Fprintf(&b, "the Clawvisor repo is the canonical source for the configure-side steps.\n")
	fmt.Fprintf(&b, "If you want the full reasoning behind any decision below, read it.\n\n")
	fmt.Fprintf(&b, "Set the endpoint:\n\n```bash\nexport CLAWVISOR_URL=%s\n```\n\n", ctx.ClawvisorURL)
	b.WriteString(sectionDashboardAnswers(ctx, "OpenClaw running mode: "+ctx.OpenClawMode))

	if ctx.OpenClawMode == "remote" {
		b.WriteString(sectionOpenClawRemoteProbe())
	} else {
		b.WriteString(sectionProbe("openclaw", []string{
			"**Container vs host** — `docker ps --format '{{.Names}}\\t{{.Image}}' | grep -i openclaw`. Capture the container name if found; otherwise check `~/.openclaw/` on the host.",
			"**Gateway port** — read `~/.openclaw/openclaw.json` (or `<container>:/.openclaw/openclaw.json`); the gateway's `port` defaults to 18789 but the user may have changed it.",
		}))
	}

	fmt.Fprintf(&b, "## 2. Check for an existing agent\n\n")
	fmt.Fprintf(&b, "OpenClaw needs a *callback secret* in addition to the agent token (so\n")
	fmt.Fprintf(&b, "the webhook plugin can verify Clawvisor callbacks). The standard reuse\n")
	fmt.Fprintf(&b, "check (Step 2 of the other harnesses) doesn't apply cleanly — the\n")
	fmt.Fprintf(&b, "callback secret isn't stored in `~/.clawvisor/agents/*.json`.\n\n")
	if ctx.OpenClawMode == "remote" {
		fmt.Fprintf(&b, "Do not check the local filesystem for an existing OpenClaw integration.\n")
		fmt.Fprintf(&b, "Use the remote access details from Step 1 and check the remote workspace:\n\n")
		fmt.Fprintf(&b, "```bash\n")
		fmt.Fprintf(&b, "ssh \"$OPENCLAW_REMOTE\" 'test -f ~/.openclaw/workspace/.env && grep -n \"^CLAWVISOR_AGENT_TOKEN=\" ~/.openclaw/workspace/.env || true'\n")
		fmt.Fprintf(&b, "```\n\n")
		fmt.Fprintf(&b, "If the remote workspace already has `CLAWVISOR_AGENT_TOKEN` set, ask the\n")
		fmt.Fprintf(&b, "user whether to reuse the existing OpenClaw integration or rotate it.\n")
		fmt.Fprintf(&b, "Reuse path: skip to Step 7 to verify the integration still works.\n\n")
	} else {
		fmt.Fprintf(&b, "If `~/.openclaw/workspace/.env` already has `CLAWVISOR_AGENT_TOKEN` set,\n")
		fmt.Fprintf(&b, "ask the user whether to reuse the existing OpenClaw integration or\n")
		fmt.Fprintf(&b, "rotate. Reuse path: skip to Step 7 to verify the integration still works.\n\n")
	}

	b.WriteString(sectionMint("openclaw", ctx.ClawvisorURL, ctx.Claim, ctx.UserID))

	fmt.Fprintf(&b, "Capture the callback secret from the approval response too — it's emitted\n")
	fmt.Fprintf(&b, "alongside `token` for OpenClaw-style agents:\n\n")
	fmt.Fprintf(&b, "```bash\nCALLBACK_SECRET=$(echo \"$RESPONSE\" | jq -r .callback_secret)\n```\n\n")
	fmt.Fprintf(&b, "If the secret is empty, the dashboard didn't tag the agent as OpenClaw —\n")
	fmt.Fprintf(&b, "ask the user to delete the just-created agent and use the dashboard's\n")
	fmt.Fprintf(&b, "OpenClaw tab instead, which mints with `--with-callback-secret`.\n\n")

	b.WriteString(sectionPersistToken("openclaw", "openclaw"))

	if ctx.OpenClawMode == "remote" {
		b.WriteString(sectionOpenClawRemoteConfigure(ctx.ClawvisorURL))
	} else {
		b.WriteString(sectionOpenClawLocalConfigure(ctx.ClawvisorURL))
	}

	b.WriteString(sectionSmokeTest(ctx.ClawvisorURL))

	b.WriteString(sectionUninstallDoc("openclaw", `1. Strip Clawvisor lines from `+"`~/.openclaw/workspace/.env`"+`: `+"`grep -v '^CLAWVISOR_\\|^OPENCLAW_HOOKS_URL=' ~/.openclaw/workspace/.env`"+`.
2. Disable the webhook plugin: edit `+"`~/.openclaw/openclaw.json`"+` and set `+"`plugins.entries[\"clawvisor-webhook\"].enabled = false`"+` (or delete the entry).
3. Optional: remove the extension directory: `+"`rm -rf ~/.openclaw/extensions/clawvisor-webhook`"+`.
4. Optional: uninstall the workspace skill: `+"`npx clawhub uninstall clawvisor`"+`.
5. Delete the token file: `+"`rm ~/.clawvisor/agents/openclaw.json`"+`.
6. Revoke the agent in the Clawvisor dashboard under Agents → openclaw → Delete.
7. Restart OpenClaw.
`))

	b.WriteString(sectionSelfUninstall("openclaw", helperInstallerCleanupCommands()))

	return b.String()
}

func sectionOpenClawRemoteProbe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 1. Confirm remote OpenClaw access\n\n")
	fmt.Fprintf(&b, "The user selected **remote host** in the dashboard. Do **not** probe the\n")
	fmt.Fprintf(&b, "local machine for OpenClaw files, Docker containers, or gateway ports;\n")
	fmt.Fprintf(&b, "that would inspect the helper agent's machine, not the OpenClaw host.\n\n")
	fmt.Fprintf(&b, "Ask the user for the remote access details you need, then keep them in\n")
	fmt.Fprintf(&b, "shell variables for the commands below:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "export OPENCLAW_REMOTE='<ssh host, for example user@example.com>'\n")
	fmt.Fprintf(&b, "export OPENCLAW_WORKSPACE='~/.openclaw/workspace'\n")
	fmt.Fprintf(&b, "export OPENCLAW_GATEWAY_WS='ws://127.0.0.1:18789'\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If SSH is unavailable, do not invent local commands. Give the user the\n")
	fmt.Fprintf(&b, "remote commands from later steps to run on the OpenClaw host and ask them\n")
	fmt.Fprintf(&b, "to paste back any output or errors.\n\n")
	fmt.Fprintf(&b, "Verify only the remote host:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "ssh \"$OPENCLAW_REMOTE\" 'uname -s; test -d ~/.openclaw && echo openclaw-dir-present || true; test -f ~/.openclaw/openclaw.json && cat ~/.openclaw/openclaw.json || true'\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Keep what you learned in a JSON object for `install_context`:\n\n")
	fmt.Fprintf(&b, "```json\n")
	fmt.Fprintf(&b, "{\n")
	fmt.Fprintf(&b, "  \"harness\": \"openclaw\",\n")
	fmt.Fprintf(&b, "  \"install_mode\": \"remote\",\n")
	fmt.Fprintf(&b, "  \"remote_host\": \"<hostname or description>\",\n")
	fmt.Fprintf(&b, "  \"host_os\": \"darwin | linux | windows\",\n")
	fmt.Fprintf(&b, "  \"gateway_ws_url\": \"<ws://...>\",\n")
	fmt.Fprintf(&b, "  \"auth_mode\": \"swap\"\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "```\n\n")
	return b.String()
}

func sectionOpenClawLocalConfigure(clawvisorURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 5. Install the clawvisor skill in the OpenClaw workspace\n\n")
	fmt.Fprintf(&b, "Use clawhub from the host (or inside the OpenClaw container if that's where\n")
	fmt.Fprintf(&b, "OpenClaw runs):\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "# Host install:\n")
	fmt.Fprintf(&b, "npx clawhub install clawvisor --force\n\n")
	fmt.Fprintf(&b, "# Docker install:\n")
	fmt.Fprintf(&b, "docker exec <OPENCLAW_CONTAINER> npx clawhub install clawvisor --force \\\n")
	fmt.Fprintf(&b, "  --workdir /home/node/.openclaw/workspace\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Verify: `ls ~/.openclaw/workspace/skills/clawvisor/SKILL.md`.\n\n")

	fmt.Fprintf(&b, "## 5.5. Install and enable the webhook plugin\n\n")
	fmt.Fprintf(&b, "Install the webhook extension OpenClaw uses to receive async approval\n")
	fmt.Fprintf(&b, "callbacks from Clawvisor:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "EXT_DST=~/.openclaw/extensions/clawvisor-webhook\n")
	fmt.Fprintf(&b, "mkdir -p \"$EXT_DST\"\n")
	fmt.Fprintf(&b, "cd \"$EXT_DST\" && npm init -y && npm install clawvisor-webhook --production\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Enable it in `~/.openclaw/openclaw.json` — **read, merge, write back;\n")
	fmt.Fprintf(&b, "don't overwrite the file**:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "jq '.plugins.entries[\"clawvisor-webhook\"] = {\"enabled\": true}' \\\n")
	fmt.Fprintf(&b, "  ~/.openclaw/openclaw.json > /tmp/openclaw.json.tmp \\\n")
	fmt.Fprintf(&b, "  && mv /tmp/openclaw.json.tmp ~/.openclaw/openclaw.json\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If the user's gateway runs on a non-default port, also set\n")
	fmt.Fprintf(&b, "`.plugins.entries[\"clawvisor-webhook\"].config.gatewayWsUrl` to\n")
	fmt.Fprintf(&b, "`ws://127.0.0.1:<port>`. Default is fine for most installs.\n\n")

	fmt.Fprintf(&b, "## 6. Write environment variables\n\n")
	fmt.Fprintf(&b, "OpenClaw reads `~/.openclaw/workspace/.env` (non-overriding semantics —\n")
	fmt.Fprintf(&b, "shell env wins). Strip prior Clawvisor lines first so re-runs are\n")
	fmt.Fprintf(&b, "idempotent, then append fresh values.\n\n")
	fmt.Fprintf(&b, "**Pick the Clawvisor URL OpenClaw can actually reach:**\n\n")
	fmt.Fprintf(&b, "- Both OpenClaw and Clawvisor in Docker on same host: `http://host.docker.internal:25297`\n")
	fmt.Fprintf(&b, "- OpenClaw in Docker, Clawvisor on host: `http://host.docker.internal:25297`\n")
	fmt.Fprintf(&b, "- Both on host: `%s` (or whatever `$CLAWVISOR_URL` is)\n\n", clawvisorURL)
	fmt.Fprintf(&b, "Same logic for `OPENCLAW_HOOKS_URL` (gateway port on the same axis).\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "grep -v '^CLAWVISOR_\\|^OPENCLAW_HOOKS_URL=' ~/.openclaw/workspace/.env > /tmp/env.tmp 2>/dev/null || true\n")
	fmt.Fprintf(&b, "mv /tmp/env.tmp ~/.openclaw/workspace/.env 2>/dev/null || true\n")
	fmt.Fprintf(&b, "cat >> ~/.openclaw/workspace/.env <<EOF\n")
	fmt.Fprintf(&b, "CLAWVISOR_URL=<resolved URL>\n")
	fmt.Fprintf(&b, "CLAWVISOR_AGENT_TOKEN=$TOKEN\n")
	fmt.Fprintf(&b, "CLAWVISOR_CALLBACK_SECRET=$CALLBACK_SECRET\n")
	fmt.Fprintf(&b, "OPENCLAW_HOOKS_URL=<resolved URL>\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, "chmod 600 ~/.openclaw/workspace/.env\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "OpenClaw must be restarted to pick up the new `.env` and webhook plugin —\n")
	fmt.Fprintf(&b, "tell the user.\n\n")
	return b.String()
}

func sectionOpenClawRemoteConfigure(clawvisorURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 5. Install the clawvisor skill on the remote OpenClaw host\n\n")
	fmt.Fprintf(&b, "Run the install on the remote host, not locally. If OpenClaw runs inside\n")
	fmt.Fprintf(&b, "a container on the remote host, run the Docker command through SSH on\n")
	fmt.Fprintf(&b, "that host.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "# Remote host install:\n")
	fmt.Fprintf(&b, "ssh \"$OPENCLAW_REMOTE\" 'npx clawhub install clawvisor --force'\n\n")
	fmt.Fprintf(&b, "# Remote Docker install, only if OpenClaw is containerized on that host:\n")
	fmt.Fprintf(&b, "ssh \"$OPENCLAW_REMOTE\" 'docker exec <OPENCLAW_CONTAINER> npx clawhub install clawvisor --force --workdir /home/node/.openclaw/workspace'\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Verify on the remote host: `ssh \"$OPENCLAW_REMOTE\" 'ls ~/.openclaw/workspace/skills/clawvisor/SKILL.md'`.\n\n")

	fmt.Fprintf(&b, "## 5.5. Install and enable the webhook plugin on the remote host\n\n")
	fmt.Fprintf(&b, "Install the webhook extension on the same remote host where OpenClaw runs:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "ssh \"$OPENCLAW_REMOTE\" '\n")
	fmt.Fprintf(&b, "set -e\n")
	fmt.Fprintf(&b, "EXT_DST=~/.openclaw/extensions/clawvisor-webhook\n")
	fmt.Fprintf(&b, "mkdir -p \"$EXT_DST\"\n")
	fmt.Fprintf(&b, "cd \"$EXT_DST\"\n")
	fmt.Fprintf(&b, "npm init -y >/dev/null\n")
	fmt.Fprintf(&b, "npm install clawvisor-webhook --production\n")
	fmt.Fprintf(&b, "'\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Enable it in the remote `~/.openclaw/openclaw.json` — **read, merge,\n")
	fmt.Fprintf(&b, "write back; don't overwrite the file**:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "ssh \"$OPENCLAW_REMOTE\" '\n")
	fmt.Fprintf(&b, "jq --arg ws \"$OPENCLAW_GATEWAY_WS\" '\\''\n")
	fmt.Fprintf(&b, ".plugins.entries[\"clawvisor-webhook\"] = {\n")
	fmt.Fprintf(&b, "  \"enabled\": true,\n")
	fmt.Fprintf(&b, "  \"config\": {\"gatewayWsUrl\": $ws}\n")
	fmt.Fprintf(&b, "}'\\'' ~/.openclaw/openclaw.json > /tmp/openclaw.json.tmp\n")
	fmt.Fprintf(&b, "mv /tmp/openclaw.json.tmp ~/.openclaw/openclaw.json\n")
	fmt.Fprintf(&b, "'\n")
	fmt.Fprintf(&b, "```\n\n")

	fmt.Fprintf(&b, "## 6. Write environment variables on the remote OpenClaw host\n\n")
	fmt.Fprintf(&b, "Because OpenClaw is remote, `localhost` on that host is not this helper\n")
	fmt.Fprintf(&b, "machine. Pick a Clawvisor URL the remote host can actually reach. The\n")
	fmt.Fprintf(&b, "dashboard rendered `%s`; if that is a localhost URL, replace it with a\n", clawvisorURL)
	fmt.Fprintf(&b, "relay, public, VPN, or LAN URL reachable from `$OPENCLAW_REMOTE`.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "export OPENCLAW_CLAWVISOR_URL='<remote-reachable Clawvisor URL>'\n")
	fmt.Fprintf(&b, "ssh \"$OPENCLAW_REMOTE\" \"curl -sf -H 'X-Clawvisor-Agent-Token: $TOKEN' '$OPENCLAW_CLAWVISOR_URL/api/skill/catalog' >/dev/null\"\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Then write the remote workspace `.env`:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "ssh \"$OPENCLAW_REMOTE\" \"\n")
	fmt.Fprintf(&b, "set -e\n")
	fmt.Fprintf(&b, "mkdir -p ~/.openclaw/workspace\n")
	fmt.Fprintf(&b, "grep -v '^CLAWVISOR_\\|^OPENCLAW_HOOKS_URL=' ~/.openclaw/workspace/.env > /tmp/env.tmp 2>/dev/null || true\n")
	fmt.Fprintf(&b, "mv /tmp/env.tmp ~/.openclaw/workspace/.env 2>/dev/null || true\n")
	fmt.Fprintf(&b, "cat >> ~/.openclaw/workspace/.env <<EOF\n")
	fmt.Fprintf(&b, "CLAWVISOR_URL=$OPENCLAW_CLAWVISOR_URL\n")
	fmt.Fprintf(&b, "CLAWVISOR_AGENT_TOKEN=$TOKEN\n")
	fmt.Fprintf(&b, "CLAWVISOR_CALLBACK_SECRET=$CALLBACK_SECRET\n")
	fmt.Fprintf(&b, "OPENCLAW_HOOKS_URL=$OPENCLAW_GATEWAY_WS\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, "chmod 600 ~/.openclaw/workspace/.env\n")
	fmt.Fprintf(&b, "\"\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Restart OpenClaw on the remote host after writing `.env` and enabling the\n")
	fmt.Fprintf(&b, "webhook plugin.\n\n")
	return b.String()
}
