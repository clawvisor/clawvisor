package handlers

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/clawvisor/clawvisor/internal/relay"
)

// dockerHostURL adapts a Clawvisor URL for use from inside a container running
// on the helper's host. If the URL points at `localhost` / `127.0.0.1`
// (typically because no proxy or public URL is configured and resolveURL
// fell through to the request host), swap the host to `host.docker.internal`
// so the container can reach Clawvisor on the host. URLs that already point
// at a real hostname (lite-proxy public URL, server public URL, relay URL)
// are returned unchanged.
func dockerHostURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	host, port, splitErr := net.SplitHostPort(u.Host)
	if splitErr != nil {
		host = u.Host
		port = ""
	}
	if host != "localhost" && host != "127.0.0.1" {
		return raw
	}
	if port == "" {
		u.Host = "host.docker.internal"
	} else {
		u.Host = "host.docker.internal:" + port
	}
	return u.String()
}

// validAgentName guards the `agent_name` query param. Same shape as agent
// names accepted elsewhere — kebab/underscore alphanum, capped at 64 chars
// so a malicious URL can't shove a shell metacharacter into the rendered
// `~/.clawvisor/agents/<name>.json` path inside the skill markdown.
var validAgentName = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)

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
	HermesMode      string
	OpenClawMode    string
	// AgentName is the on-disk filename slug for ~/.clawvisor/agents/<name>.json.
	// Defaults to the harness name; the dashboard overrides via ?agent_name=
	// when it picks a non-colliding variant (openclaw-1, openclaw-2, …).
	AgentName string
}

// Setup handles GET /skill/install/{target}. The route captures the whole
// segment (Go's ServeMux doesn't allow `{target}.md`), so we trim a trailing
// `.md` here — the dashboard renders the public URL with the extension for
// content-sniffing on the agent side. To keep the URL shape unambiguous
// (browsers that hit the no-extension form would otherwise see inline
// markdown), redirect a no-suffix request to the canonical `.md` form,
// preserving any query string. Skips the redirect when the segment is
// already `.md` or when there's no obvious harness slug at all.
func (h *InstallerHandler) Setup(w http.ResponseWriter, r *http.Request) {
	rawTarget := r.PathValue("target")
	if rawTarget != "" && !strings.HasSuffix(rawTarget, ".md") {
		redirectURL := r.URL.Path + ".md"
		if raw := r.URL.RawQuery; raw != "" {
			redirectURL += "?" + raw
		}
		http.Redirect(w, r, redirectURL, http.StatusMovedPermanently)
		return
	}
	target := InstallerTarget(strings.TrimSuffix(rawTarget, ".md"))

	ctx := installerCtx{
		ClawvisorURL: h.resolveURL(r),
		IsLocal:      h.isLocal,
	}
	// `validUserID` (defined in onboarding.go) is `^[a-zA-Z0-9_-]+$` with no
	// length bound — so a `?user_id=<10MB>` query param would pass the regex
	// and get embedded verbatim into the rendered markdown. The body is
	// already gated upstream, but a per-field cap keeps a single noisy query
	// from inflating the response. 64 matches the agent-name cap elsewhere.
	const maxUserIDLen = 64
	if uid := r.URL.Query().Get("user_id"); uid != "" && len(uid) <= maxUserIDLen && validUserID.MatchString(uid) {
		ctx.UserID = uid
	}
	// Same length defense for `claim`. Claim codes are minted as 10-char
	// base64 (see MintClaim in connections.go), so 64 is a generous cap that
	// still rejects abuse without rejecting any legitimate value.
	const maxClaimLen = 64
	if claim := r.URL.Query().Get("claim"); claim != "" && len(claim) <= maxClaimLen {
		ctx.Claim = claim
	}
	ctx.ClaudeScope = queryChoice(r, "claude_scope", "alias", "alias", "global")
	ctx.ClaudeCurlAllow = queryChoice(r, "claude_curl_allow", "no", "no", "yes")
	ctx.AliasMode = queryChoice(r, "alias_mode", "safe", "none", "safe", "yolo")
	ctx.HermesConfig = queryChoice(r, "hermes_config", "env", "env", "file")
	ctx.HermesMode = queryChoice(r, "hermes_mode", "host", "host", "docker", "remote")
	ctx.OpenClawMode = queryChoice(r, "openclaw_mode", "host", "host", "docker", "remote")
	defaultProvider := "anthropic"
	if target == InstallerHermes {
		defaultProvider = "openai"
	}
	ctx.LLMProvider = queryChoice(r, "llm_provider", defaultProvider, "anthropic", "openai")
	ctx.AgentName = string(target) // default
	if n := r.URL.Query().Get("agent_name"); n != "" && validAgentName.MatchString(n) {
		ctx.AgentName = n
	}

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

// Uninstall handles GET /skill/uninstall/{target}.md. Renders the companion
// uninstall skill the install flow writes to disk in its final step so the
// user has a one-command revert path (/clawvisor-uninstall) without going
// back to the dashboard. Only Claude Code and Codex have one — Hermes /
// OpenClaw uninstall paths are different (revert harness binary install,
// not just config) and ship inline `uninstall-<harness>.md` reference docs
// from the existing installer flow.
func (h *InstallerHandler) Uninstall(w http.ResponseWriter, r *http.Request) {
	rawTarget := r.PathValue("target")
	if rawTarget != "" && !strings.HasSuffix(rawTarget, ".md") {
		redirectURL := r.URL.Path + ".md"
		if raw := r.URL.RawQuery; raw != "" {
			redirectURL += "?" + raw
		}
		http.Redirect(w, r, redirectURL, http.StatusMovedPermanently)
		return
	}
	target := InstallerTarget(strings.TrimSuffix(rawTarget, ".md"))

	ctx := installerCtx{
		ClawvisorURL: h.resolveURL(r),
		IsLocal:      h.isLocal,
	}
	ctx.AgentName = string(target)
	if n := r.URL.Query().Get("agent_name"); n != "" && validAgentName.MatchString(n) {
		ctx.AgentName = n
	}

	var body string
	switch target {
	case InstallerClaudeCode:
		body = renderClaudeCodeUninstaller(ctx)
	case InstallerCodex:
		body = renderCodexUninstaller(ctx)
	default:
		http.Error(w, "unknown uninstall target", http.StatusNotFound)
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
	return modelContextWindow(providerDefaultModel(provider))
}

func modelContextWindow(model string) int {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "gpt-5.4":
		return 1000000
	default:
		// Use 200K as the conservative floor for modern Clawvisor-routed
		// models. Add known larger model IDs above as we validate them.
		return 200000
	}
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
//
// `harness` is spliced into the YAML `description:` line unescaped. Every
// caller today passes a hard-coded literal ("Claude Code", "Codex",
// "Hermes", "OpenClaw"), so that's safe. If you ever wire user-controlled
// or per-request data into this argument (an agent name, harness version,
// etc.), escape characters that would break YAML — `:`, `\n`, `"`, leading
// dashes — first, or the skill loaders will reject the file at startup.
func installerFrontmatter(harness string) string {
	return fmt.Sprintf(`---
name: clawvisor-install
description: Install Clawvisor into %s — probe the environment, mint and approve a connection request, configure %s, optionally add an alias, run a connectivity smoke test, and remove itself when done.
---

`, harness, harness)
}

// setupFrontmatter is the YAML header for the one-paste Claude Code / Codex
// setup skill. Distinct from installerFrontmatter because (a) the slash
// command name is `clawvisor-setup` (vs. `clawvisor-install` for harness
// installs), (b) the description reflects the new flow — no dashboard
// approval, optional default-everywhere routing, subprocess smoke test.
func setupFrontmatter(harness string) string {
	return fmt.Sprintf(`---
name: clawvisor-setup
description: One-paste connect %s to Clawvisor — register, install the skill, optionally route every session through Clawvisor, and remove this command file.
---

`, harness)
}

// uninstallFrontmatter is the YAML header for the companion uninstall
// skill that the install skill drops to disk as its final action. The
// user invokes it with /clawvisor-uninstall (or the Codex equivalent)
// to revert everything the install changed.
func uninstallFrontmatter(harness string) string {
	return fmt.Sprintf(`---
name: clawvisor-uninstall
description: Revert the Clawvisor setup for %s — remove env / config entries, delete the local token file, and remove this command file. Use this when you want to back out cleanly.
---

`, harness)
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
	fmt.Fprintf(&b, "- **Harness running mode** — host, docker, or remote (`docker ps`, `uname -s`,\n")
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
	fmt.Fprintf(&b, "  \"install_mode\": \"host | docker | remote\",\n")
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
	fmt.Fprintf(&b, "  \"install_mode\": \"host\",\n")
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

// ── Shared helpers for the one-paste setup skill (Claude Code, Codex) ────────

// sectionClaimedConnect renders the connect-with-claim curl + token-file
// write. The claim is the user's pre-authorization from the dashboard;
// the connect endpoint consumes it and auto-approves in one round-trip,
// so the curl returns the agent token directly (no waiting, no second
// dashboard click).
func sectionClaimedConnect(harness, clawvisorURL, claim, agentName string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 1. Register and persist the token\n\n")
	fmt.Fprintf(&b, "The claim code below is the user's pre-authorization — the connect endpoint\n")
	fmt.Fprintf(&b, "consumes it and returns the agent token immediately. No second dashboard\n")
	fmt.Fprintf(&b, "click required.\n\n")
	fmt.Fprintf(&b, "Set the variables this skill uses (already filled in):\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "export CLAWVISOR_URL=%q\n", clawvisorURL)
	fmt.Fprintf(&b, "export AGENT_NAME=%q\n", agentName)
	fmt.Fprintf(&b, "export TOKEN_FILE=~/.clawvisor/agents/$AGENT_NAME.json\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**Pre-flight: detect an existing install.** If `$TOKEN_FILE` already\n")
	fmt.Fprintf(&b, "exists, this is a re-install over a prior setup. Ask the user before\n")
	fmt.Fprintf(&b, "continuing — otherwise the connect call will fail with `AGENT_NAME_EXISTS`\n")
	fmt.Fprintf(&b, "and the user won't know why.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "if [ -f \"$TOKEN_FILE\" ]; then\n")
	fmt.Fprintf(&b, "  echo \"existing install detected\"\n")
	fmt.Fprintf(&b, "  ls -l \"$TOKEN_FILE\"\n")
	fmt.Fprintf(&b, "fi\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If the file exists, ask the user EXACTLY one question (verbatim or close):\n\n")
	fmt.Fprintf(&b, "> A Clawvisor install for `%s` already exists at `$TOKEN_FILE`.\n", harness)
	fmt.Fprintf(&b, "> Overwrite it with a fresh install?\n")
	fmt.Fprintf(&b, "> \n")
	fmt.Fprintf(&b, "> **Yes** — register a new agent and rewrite the local token file. The old\n")
	fmt.Fprintf(&b, "> agent's token still exists in the Clawvisor dashboard; revoke it from\n")
	fmt.Fprintf(&b, "> `$CLAWVISOR_URL/dashboard/agents` when you're ready. The previous install's\n")
	fmt.Fprintf(&b, "> diff records under `~/.clawvisor/diffs/$AGENT_NAME/` are still there —\n")
	fmt.Fprintf(&b, "> `/clawvisor-uninstall` can still cleanly reverse the original install.\n")
	fmt.Fprintf(&b, "> \n")
	fmt.Fprintf(&b, "> **No** — exit without changes.\n\n")
	fmt.Fprintf(&b, "If **yes**, delete the existing token file so the connect call below\n")
	fmt.Fprintf(&b, "writes a fresh one. (You'll also hit `AGENT_NAME_EXISTS` on the connect\n")
	fmt.Fprintf(&b, "call — the dashboard's bootstrap link picks a non-colliding `$AGENT_NAME`\n")
	fmt.Fprintf(&b, "for re-installs, but if the user pasted an older link, ask them to refresh\n")
	fmt.Fprintf(&b, "the dashboard and re-paste.)\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "rm -f \"$TOKEN_FILE\"\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If **no**, stop here and tell the user the existing install is unchanged.\n\n")
	fmt.Fprintf(&b, "Now register the agent:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "mkdir -p ~/.clawvisor/agents\n")
	if claim != "" {
		fmt.Fprintf(&b, "curl -sf --remove-on-error -X POST \\\n")
		fmt.Fprintf(&b, "  \"$CLAWVISOR_URL/api/agents/connect?claim=%s&name=$AGENT_NAME&harness=%s\" \\\n", claim, harness)
		fmt.Fprintf(&b, "  -H \"Content-Type: application/json\" \\\n")
		fmt.Fprintf(&b, "  -d '{\"description\":\"%s\"}' \\\n", harness)
		fmt.Fprintf(&b, "  -o \"$TOKEN_FILE\"\n")
	} else {
		fmt.Fprintf(&b, "# (no claim baked in — you'll need to re-paste from the dashboard;\n")
		fmt.Fprintf(&b, "# the claim is short-lived and the dashboard refreshes it on revisit.)\n")
		fmt.Fprintf(&b, "echo 'no claim code — refresh the dashboard and re-paste the one-liner'\n")
		fmt.Fprintf(&b, "exit 1\n")
	}
	fmt.Fprintf(&b, "chmod 600 \"$TOKEN_FILE\"\n")
	fmt.Fprintf(&b, "TOKEN=$(jq -r .token \"$TOKEN_FILE\")\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If `curl` exits non-zero or `$TOKEN` is empty after this block, surface the\n")
	fmt.Fprintf(&b, "response to the user and STOP — do not retry. Common causes:\n\n")
	fmt.Fprintf(&b, "- **INVALID_CLAIM** — the claim expired (5 min TTL) or was already consumed.\n")
	fmt.Fprintf(&b, "  Ask the user to refresh `$CLAWVISOR_URL/dashboard/agents` and re-paste the\n")
	fmt.Fprintf(&b, "  one-liner from the Connect-an-Agent panel.\n")
	fmt.Fprintf(&b, "- **AGENT_NAME_EXISTS** — an agent with this name already exists. The user\n")
	fmt.Fprintf(&b, "  can delete the old one from the dashboard, or you can pick a fresh name\n")
	fmt.Fprintf(&b, "  (e.g. `%s-2`) by re-running with `AGENT_NAME` set differently.\n", agentName)
	fmt.Fprintf(&b, "- **HTTP 5xx** — Clawvisor is unhealthy. Ask the user to check the daemon.\n\n")
	return b.String()
}

// sectionVaultUpstreamKey is the no-leak upstream-API-key vault step.
// Detects $PROVIDER_API_KEY in the live shell with a prefix+length probe
// (never the value), asks the user to confirm, then pipes the value
// shell→jq→curl→server via stdin so the materialized key never crosses
// argv or the agent's tool-output context. Falls back to the dedicated
// /dashboard/keys/<provider> page if env is empty or the user declines.
//
// This is a RECOVERY step — only invoked when the passthrough-mode smoke
// test fails with an upstream-auth error. Users with `claude login` /
// `codex login` or an env API key get a passing smoke test on the first
// try and never see this section.
//
// `heading` is the full markdown header (e.g. "### 3.a. Vault the user's
// upstream Anthropic API key") so the caller can place this as a sub-step.
//
// HARD CONSTRAINTS at the top of the rendered step are non-negotiable —
// they're the difference between "key stays in env" and "key lands in
// transcript." Helpful-by-default agents will grep ~/.zshrc if not told
// otherwise; the explicit DO NOT list closes that hole.
func sectionVaultUpstreamKey(heading, provider, providerLabel, envVar, keyPrefix, dashboardPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", heading)
	fmt.Fprintf(&b, "The passthrough smoke test failed with an upstream auth error — the user\n")
	fmt.Fprintf(&b, "has no working login session and no `%s` in env. Either they fix that\n", envVar)
	fmt.Fprintf(&b, "and re-run the install, or we vault a %s key here for the proxy to\n", providerLabel)
	fmt.Fprintf(&b, "substitute on every call. This step does the second.\n\n")
	fmt.Fprintf(&b, "We try to detect a key in the live shell environment first; if present,\n")
	fmt.Fprintf(&b, "the value pipes directly from the shell into Clawvisor's vault without\n")
	fmt.Fprintf(&b, "ever materializing in your conversation context.\n\n")
	fmt.Fprintf(&b, "**HARD CONSTRAINTS — read carefully, these are non-negotiable:**\n\n")
	fmt.Fprintf(&b, "- DO NOT `grep`, `cat`, `head`, `tail`, or otherwise read `~/.zshrc`,\n")
	fmt.Fprintf(&b, "  `~/.bashrc`, `~/.zshenv`, `~/.profile`, `.env`, `.envrc`,\n")
	fmt.Fprintf(&b, "  `~/.config/fish/config.fish`, or any file that might contain\n")
	fmt.Fprintf(&b, "  `%s=…`. Those files hold the literal value in plaintext and\n", envVar)
	fmt.Fprintf(&b, "  reading them would put the secret in your conversation context —\n")
	fmt.Fprintf(&b, "  defeating the point of this whole step.\n")
	fmt.Fprintf(&b, "- DO NOT `echo \"$%s\"`, `printenv %s`, or print the value\n", envVar, envVar)
	fmt.Fprintf(&b, "  any other way.\n")
	fmt.Fprintf(&b, "- DO NOT use `set -x`, `bash -x`, or any trace mode.\n")
	fmt.Fprintf(&b, "- DO NOT pass the value through argv (`jq --arg`, `curl -d \"key=$VAR\"`).\n")
	fmt.Fprintf(&b, "  Argv shows up in `/proc` and process listings. Use stdin pipes only.\n")
	fmt.Fprintf(&b, "- Use ONLY the live environment of the shell you're running in right now.\n\n")
	fmt.Fprintf(&b, "Detect (this reveals only a %d-char prefix and the length — zero entropy):\n\n", len(keyPrefix))
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "if [ -n \"$%s\" ]; then\n", envVar)
	fmt.Fprintf(&b, "  printf 'present prefix=%%s length=%%d\\n' \\\n")
	fmt.Fprintf(&b, "    \"$(printf '%%s' \"$%s\" | head -c %d)\" \\\n", envVar, len(keyPrefix))
	fmt.Fprintf(&b, "    \"${#%s}\"\n", envVar)
	fmt.Fprintf(&b, "else\n")
	fmt.Fprintf(&b, "  echo absent\n")
	fmt.Fprintf(&b, "fi\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**If the output is `present prefix=%s length=<N>`**, ask the user:\n\n", keyPrefix)
	fmt.Fprintf(&b, "> I see a %s API key in your environment (prefix `%s`, N chars).\n", providerLabel, keyPrefix)
	fmt.Fprintf(&b, "> Want me to vault it in Clawvisor so this agent can route through proxy-lite?\n")
	fmt.Fprintf(&b, "> I won't read the key itself — it'll pipe straight from your shell into\n")
	fmt.Fprintf(&b, "> Clawvisor's vault.\n\n")
	fmt.Fprintf(&b, "If they say yes, vault via stdin pipe (value never enters argv):\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "printf '%%s' \"$%s\" | jq -Rs '{api_key:.}' | \\\n", envVar)
	fmt.Fprintf(&b, "  curl -sS -X PUT \"$CLAWVISOR_URL/api/runtime/llm-credentials/%s\" \\\n", provider)
	fmt.Fprintf(&b, "    -H \"Authorization: Bearer $TOKEN\" \\\n")
	fmt.Fprintf(&b, "    -H \"Content-Type: application/json\" \\\n")
	fmt.Fprintf(&b, "    --data-binary @-\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Expected response: `{\"provider\":\"%s\",\"service_id\":\"…\",\"status\":\"stored\"}`\n", provider)
	fmt.Fprintf(&b, "(or `\"rotated\"` / `\"unchanged\"`). No key is echoed back. Any of those\n")
	fmt.Fprintf(&b, "status values means the key is vaulted.\n\n")
	fmt.Fprintf(&b, "**If the env variable was `absent` OR the user declined to vault from env**,\n")
	fmt.Fprintf(&b, "fall through to the dashboard page. The page's `?for=<agent_id>` param\n")
	fmt.Fprintf(&b, "scopes the saved key to this specific agent. The id is in the token file\n")
	fmt.Fprintf(&b, "we wrote in step 1:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "AGENT_ID=$(jq -r .agent_id \"$TOKEN_FILE\")\n")
	fmt.Fprintf(&b, "echo \"$CLAWVISOR_URL%s?for=$AGENT_ID\"\n", dashboardPath)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Tell the user:\n\n")
	fmt.Fprintf(&b, "> Open the URL above to add your %s key. I'll wait — once you save it,\n", providerLabel)
	fmt.Fprintf(&b, "> I'll continue automatically.\n\n")
	fmt.Fprintf(&b, "Then poll the credentials endpoint (up to ~3 min). **Pass `?agent_id=`**\n")
	fmt.Fprintf(&b, "and accept EITHER user-scope OR agent-scope as success — the dashboard\n")
	fmt.Fprintf(&b, "page lets the user pick either. Without `?agent_id=`, the server only\n")
	fmt.Fprintf(&b, "reports user-scope, and a user who saved with \"Only this agent\" would\n")
	fmt.Fprintf(&b, "leave us polling forever.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "for i in $(seq 1 90); do\n")
	fmt.Fprintf(&b, "  RESP=$(curl -sS -H \"Authorization: Bearer $TOKEN\" \\\n")
	fmt.Fprintf(&b, "    \"$CLAWVISOR_URL/api/runtime/llm-credentials?agent_id=$AGENT_ID\")\n")
	fmt.Fprintf(&b, "  if echo \"$RESP\" | jq -e '.credentials[] | select(.provider==\"%s\" and (.stored==true or .agent_stored==true))' >/dev/null 2>&1; then\n", provider)
	fmt.Fprintf(&b, "    echo \"key vaulted\"; break\n")
	fmt.Fprintf(&b, "  fi\n")
	fmt.Fprintf(&b, "  sleep 2\n")
	fmt.Fprintf(&b, "done\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If the loop completes without `key vaulted` (user closed the tab or never\n")
	fmt.Fprintf(&b, "saved), ask the user if they want to keep waiting or fall back to the\n")
	fmt.Fprintf(&b, "alias-only path (jump ahead to the alias step).\n\n")
	return b.String()
}

// recordTextDiff renders the shell snippet that captures an appended text
// block into ~/.clawvisor/diffs/$AGENT_NAME/<id>.json, alongside appending
// the same content to `targetFile`. The diff record is what the uninstall
// uses to find and remove the block later — the user's file stays free of
// any clawvisor-related markers.
//
// `id` is a stable per-modification slug (e.g. "claude-cv", "provider_block")
// so multi-step installs don't overwrite each other's records.
//
// `contentHeredoc` is the heredoc body emitted verbatim — callers control
// expansion via the heredoc delimiter form they use upstream of this
// helper. We assume the content has already been generated into a shell
// `CONTENT` variable and the rendered block emitted by this helper writes
// both targets from that variable.
func recordTextDiff(id, targetFile string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mkdir -p ~/.clawvisor/diffs/$AGENT_NAME\n")
	fmt.Fprintf(&b, "printf '\\n%%s\\n' \"$CONTENT\" >> %s\n", targetFile)
	fmt.Fprintf(&b, "jq -n --arg file %s --arg content \"$CONTENT\" \\\n", targetFile)
	fmt.Fprintf(&b, "  '{file: $file, type: \"text_append\", content: $content}' \\\n")
	fmt.Fprintf(&b, "  > ~/.clawvisor/diffs/$AGENT_NAME/%s.json\n", id)
	return b.String()
}

// recordJSONKeyDiff renders the shell snippet that records which dotted JSON
// paths the install added to `targetFile`. Uninstall uses jq to walk the
// list and delete each path.
//
// `paths` is the literal JSON array body (e.g. `"env.X","env.Y"`).
func recordJSONKeyDiff(id, targetFile, paths string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mkdir -p ~/.clawvisor/diffs/$AGENT_NAME\n")
	fmt.Fprintf(&b, "jq -n --arg file %s \\\n", targetFile)
	fmt.Fprintf(&b, "  '{file: $file, type: \"json_keys\", paths: [%s]}' \\\n", paths)
	fmt.Fprintf(&b, "  > ~/.clawvisor/diffs/$AGENT_NAME/%s.json\n", id)
	return b.String()
}

// classifySmokeFailure renders the shared "how to decide what to do when
// the smoke test failed" guidance — separates the upstream-auth-error case
// (which is the trigger for the swap-mode/vault fallback) from other
// failures (which are install-environment problems requiring user fix).
func classifySmokeFailure(authFailureNextStep string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Pass criteria:** exit code 0 AND stdout contains `OK` (case-insensitive).\n\n")
	fmt.Fprintf(&b, "**On fail**, surface stdout AND stderr to the user — don't hide the error.\n")
	fmt.Fprintf(&b, "Then classify by the visible failure mode:\n\n")
	fmt.Fprintf(&b, "- **HTTP 401 from upstream / \"no API key\" / \"not logged in\"** — the user\n")
	fmt.Fprintf(&b, "  has no working upstream auth. %s\n", authFailureNextStep)
	fmt.Fprintf(&b, "- **HTTP 404** — the Clawvisor URL is wrong, or `proxy_lite.enabled` is\n")
	fmt.Fprintf(&b, "  not set in the daemon config. Surface and STOP — the user fixes it.\n")
	fmt.Fprintf(&b, "- **Connection refused** — Clawvisor daemon is not running. Surface and STOP.\n")
	fmt.Fprintf(&b, "- **Timeout** — Clawvisor is unreachable or hung. Surface and STOP.\n")
	fmt.Fprintf(&b, "- **Anything else** — surface and STOP. Don't write any config; don't\n")
	fmt.Fprintf(&b, "  guess at a fix. The user can re-run the install after debugging.\n\n")
	return b.String()
}

// sectionSelfUninstallSetup is the one-paste skill's final step. Three jobs:
//
//   1. Download the companion uninstall skill so the user has a
//      `/clawvisor-uninstall` (or Codex equivalent) revert path. The
//      uninstall skill is rendered server-side with the same agent name
//      baked in so it knows which token file / settings entries to undo.
//   2. Self-delete (the setup skill is one-shot scaffolding).
//   3. Tell the user what happened + how to revert.
//
// `installerTarget` is the URL slug used in /skill/uninstall/<target>.md
// (e.g. "claude-code"). `uninstallSkillPath` is the on-disk path where
// the agent should write the downloaded uninstall skill. `removeSetupCmd`
// removes the just-completed setup skill itself.
func sectionSelfUninstallSetup(stepNum int, harness, installerTarget, uninstallSkillPath, removeSetupCmd string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %d. Drop the uninstall skill, then self-uninstall\n\n", stepNum)
	fmt.Fprintf(&b, "Setup is done. Two things to do before exiting:\n\n")
	fmt.Fprintf(&b, "**Download the companion uninstall skill.** The user gets a one-command\n")
	fmt.Fprintf(&b, "revert path (`/clawvisor-uninstall` or the Codex equivalent) without going\n")
	fmt.Fprintf(&b, "back to the dashboard:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	// Use --create-dirs so the codex skill subdirectory is created if needed.
	fmt.Fprintf(&b, "curl -sf \"$CLAWVISOR_URL/skill/uninstall/%s.md?agent_name=$AGENT_NAME\" \\\n", installerTarget)
	fmt.Fprintf(&b, "  --create-dirs -o %s\n", uninstallSkillPath)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**Remove this setup skill.** It's one-shot scaffolding, not needed once\n")
	fmt.Fprintf(&b, "%s is connected:\n\n", harness)
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "%s\n", removeSetupCmd)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Tell the user:\n\n")
	fmt.Fprintf(&b, "- %s is now connected to Clawvisor as `$AGENT_NAME`.\n", harness)
	fmt.Fprintf(&b, "- Manage it from `$CLAWVISOR_URL/dashboard/agents`.\n")
	fmt.Fprintf(&b, "- **To revert at any time**, run `/clawvisor-uninstall` (Claude Code) or\n")
	fmt.Fprintf(&b, "  invoke the `clawvisor-uninstall` skill (Codex). It cleans up the local\n")
	fmt.Fprintf(&b, "  config, deletes the token file, and points you at the dashboard for\n")
	fmt.Fprintf(&b, "  agent + vault key cleanup.\n")
	fmt.Fprintf(&b, "- Tool calls will start triggering Clawvisor approval prompts — that's\n")
	fmt.Fprintf(&b, "  Clawvisor working as expected. Edit the runtime policy in the dashboard\n")
	fmt.Fprintf(&b, "  to auto-approve trusted tools.\n")
	return b.String()
}

// ── Per-target renders ───────────────────────────────────────────────────────

func renderClaudeCodeInstaller(ctx installerCtx) string {
	var b strings.Builder
	b.WriteString(setupFrontmatter("Claude Code"))
	fmt.Fprintf(&b, "# Connect Claude Code to Clawvisor\n\n")
	fmt.Fprintf(&b, "You are running a one-shot setup skill. The dashboard pre-baked everything\n")
	fmt.Fprintf(&b, "you need into this file: the Clawvisor URL, a single-use claim code, and\n")
	fmt.Fprintf(&b, "the agent name. The dashboard already approved the connection — no second\n")
	fmt.Fprintf(&b, "click is needed.\n\n")
	fmt.Fprintf(&b, "**Two modes; the smoke test picks one.** Clawvisor's lite-proxy can run in\n")
	fmt.Fprintf(&b, "**passthrough** (the user's existing `claude login` OAuth token or env\n")
	fmt.Fprintf(&b, "`ANTHROPIC_API_KEY` flows through Clawvisor to Anthropic — keeps their\n")
	fmt.Fprintf(&b, "subscription billing intact) or **swap** (Clawvisor substitutes a vaulted\n")
	fmt.Fprintf(&b, "API key on every call). Passthrough is the default; swap is the fallback\n")
	fmt.Fprintf(&b, "for users with no working upstream auth.\n\n")

	b.WriteString(sectionClaimedConnect("claude-code", ctx.ClawvisorURL, ctx.Claim, ctx.AgentName))

	// Step 2: passthrough smoke test. Don't clear ANTHROPIC_AUTH_TOKEN /
	// ANTHROPIC_API_KEY — let the user's existing auth (claude login OAuth
	// or env-set API key) flow through. The X-Clawvisor-Agent-Token custom
	// header rides alongside for policy ID.
	fmt.Fprintf(&b, "## 2. Smoke-test Clawvisor routing in **passthrough mode**\n\n")
	fmt.Fprintf(&b, "Run a fresh `claude` in a child process pointed at Clawvisor. We do NOT\n")
	fmt.Fprintf(&b, "clear `ANTHROPIC_AUTH_TOKEN` or `ANTHROPIC_API_KEY` here — the user's\n")
	fmt.Fprintf(&b, "existing auth needs to flow through for passthrough mode to work.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "timeout 30 env \\\n")
	fmt.Fprintf(&b, "  ANTHROPIC_BASE_URL=\"$CLAWVISOR_URL/api\" \\\n")
	fmt.Fprintf(&b, "  ANTHROPIC_CUSTOM_HEADERS=\"X-Clawvisor-Agent-Token: $TOKEN\" \\\n")
	fmt.Fprintf(&b, "  claude -p \"respond with the word OK\"\n")
	fmt.Fprintf(&b, "```\n\n")
	b.WriteString(classifySmokeFailure("Continue to step 3 to vault a key and retry in swap mode."))
	fmt.Fprintf(&b, "**On pass**, the user has working upstream auth. Set `MODE=passthrough`\n")
	fmt.Fprintf(&b, "in your head and skip to step 4.\n\n")

	// Step 3: swap-mode fallback. Vault the user's key (with the no-leak
	// guards) then re-run the smoke test with cvis_ riding as
	// ANTHROPIC_AUTH_TOKEN (which Clawvisor treats as swap mode and
	// substitutes the vaulted key).
	fmt.Fprintf(&b, "## 3. Fall back to **swap mode** (only if step 2 failed with upstream auth)\n\n")
	fmt.Fprintf(&b, "Step 2 failed because the user has no working upstream auth. We vault a\n")
	fmt.Fprintf(&b, "key here and re-run the smoke test in swap mode (the proxy will substitute\n")
	fmt.Fprintf(&b, "the vaulted key on every call).\n\n")
	b.WriteString(sectionVaultUpstreamKey("### 3.a. Vault an Anthropic API key", "anthropic", "Anthropic", "ANTHROPIC_API_KEY", "sk-ant-", "/dashboard/keys/anthropic"))
	fmt.Fprintf(&b, "### 3.b. Re-run the smoke test in swap mode\n\n")
	fmt.Fprintf(&b, "In swap mode, the agent's `cvis_…` token rides as `ANTHROPIC_AUTH_TOKEN`.\n")
	fmt.Fprintf(&b, "Clawvisor sees a `cvis_…` in the Authorization slot, recognizes the swap\n")
	fmt.Fprintf(&b, "intent, and substitutes the vaulted upstream key before forwarding to\n")
	fmt.Fprintf(&b, "Anthropic. `ANTHROPIC_API_KEY` is cleared so it can't accidentally take\n")
	fmt.Fprintf(&b, "precedence.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "timeout 30 env \\\n")
	fmt.Fprintf(&b, "  ANTHROPIC_BASE_URL=\"$CLAWVISOR_URL/api\" \\\n")
	fmt.Fprintf(&b, "  ANTHROPIC_AUTH_TOKEN=\"$TOKEN\" \\\n")
	fmt.Fprintf(&b, "  ANTHROPIC_API_KEY= \\\n")
	fmt.Fprintf(&b, "  claude -p \"respond with the word OK\"\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**Pass criteria:** exit code 0 AND stdout contains `OK`.\n\n")
	fmt.Fprintf(&b, "**On pass**, the vaulted key works. Set `MODE=swap` in your head and\n")
	fmt.Fprintf(&b, "continue to step 4.\n\n")
	fmt.Fprintf(&b, "**On fail**, the vaulted key is probably wrong (or someone revoked it).\n")
	fmt.Fprintf(&b, "Surface the error and STOP — don't loop back to vault again from this\n")
	fmt.Fprintf(&b, "session.\n\n")

	// Step 4: the one user question.
	fmt.Fprintf(&b, "## 4. Ask the user: make Clawvisor the default?\n\n")
	fmt.Fprintf(&b, "Smoke test passed (in either passthrough or swap mode — `$MODE` is set).\n")
	fmt.Fprintf(&b, "Now ask exactly one question — verbatim or close to it:\n\n")
	fmt.Fprintf(&b, "> Make Clawvisor the default for every Claude Code session? I'll write\n")
	fmt.Fprintf(&b, "> `ANTHROPIC_BASE_URL` etc. into `~/.claude/settings.json` so all future\n")
	fmt.Fprintf(&b, "> `claude` invocations route through Clawvisor automatically.\n")
	fmt.Fprintf(&b, "> \n")
	fmt.Fprintf(&b, "> The alternative is a `claude-cv` shell function — your regular `claude`\n")
	fmt.Fprintf(&b, "> stays exactly as it is, and you opt into Clawvisor routing by typing\n")
	fmt.Fprintf(&b, "> `claude-cv` instead.\n\n")
	fmt.Fprintf(&b, "- **YES (default-everywhere)** → do step 5.a.\n")
	fmt.Fprintf(&b, "- **NO (alias-only)** → do step 5.b.\n\n")

	// Step 5: apply the choice. The env we commit differs by mode.
	fmt.Fprintf(&b, "## 5. Apply the user's choice\n\n")
	fmt.Fprintf(&b, "### 5.a. Default-everywhere — commit env to `~/.claude/settings.json`\n\n")
	fmt.Fprintf(&b, "Read `~/.claude/settings.json` (if it doesn't exist, treat as `{}`).\n")
	fmt.Fprintf(&b, "Merge the entries below into the `env` object, **preserving every other\n")
	fmt.Fprintf(&b, "top-level key and every other entry in `env`.** Substitute the actual\n")
	fmt.Fprintf(&b, "values for `$CLAWVISOR_URL` and `$TOKEN`. Then record what you added in\n")
	fmt.Fprintf(&b, "an external diff file so the uninstall skill can reverse it — the user's\n")
	fmt.Fprintf(&b, "settings.json stays clean of any Clawvisor-related metadata.\n\n")
	fmt.Fprintf(&b, "**If MODE=passthrough** — keep the user's upstream auth flowing through.\n")
	fmt.Fprintf(&b, "Merge this into settings.json:\n\n")
	fmt.Fprintf(&b, "```json\n")
	fmt.Fprintf(&b, "{\n")
	fmt.Fprintf(&b, "  \"env\": {\n")
	fmt.Fprintf(&b, "    \"ANTHROPIC_BASE_URL\": \"$CLAWVISOR_URL/api\",\n")
	fmt.Fprintf(&b, "    \"ANTHROPIC_CUSTOM_HEADERS\": \"X-Clawvisor-Agent-Token: $TOKEN\"\n")
	fmt.Fprintf(&b, "  }\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Do NOT add `ANTHROPIC_AUTH_TOKEN` or `ANTHROPIC_API_KEY` keys — that would\n")
	fmt.Fprintf(&b, "blank the user's `claude login` / env key.\n\n")
	fmt.Fprintf(&b, "Then record the diff:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	b.WriteString(recordJSONKeyDiff("settings", `"$HOME/.claude/settings.json"`, `"env.ANTHROPIC_BASE_URL", "env.ANTHROPIC_CUSTOM_HEADERS"`))
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**If MODE=swap** — put the `cvis_…` token in the auth slot so Clawvisor\n")
	fmt.Fprintf(&b, "swaps it for the vaulted upstream key:\n\n")
	fmt.Fprintf(&b, "```json\n")
	fmt.Fprintf(&b, "{\n")
	fmt.Fprintf(&b, "  \"env\": {\n")
	fmt.Fprintf(&b, "    \"ANTHROPIC_BASE_URL\": \"$CLAWVISOR_URL/api\",\n")
	fmt.Fprintf(&b, "    \"ANTHROPIC_AUTH_TOKEN\": \"$TOKEN\",\n")
	fmt.Fprintf(&b, "    \"ANTHROPIC_API_KEY\": \"\"\n")
	fmt.Fprintf(&b, "  }\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Then record the diff:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	b.WriteString(recordJSONKeyDiff("settings", `"$HOME/.claude/settings.json"`, `"env.ANTHROPIC_BASE_URL", "env.ANTHROPIC_AUTH_TOKEN", "env.ANTHROPIC_API_KEY"`))
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Write the file back. **The currently-running Claude Code session keeps\n")
	fmt.Fprintf(&b, "its old config until restart** — tell the user the new routing takes\n")
	fmt.Fprintf(&b, "effect on their next `claude` invocation. Then jump to step 6\n")
	fmt.Fprintf(&b, "(self-uninstall).\n\n")

	fmt.Fprintf(&b, "### 5.b. Alias-only — append `claude-cv` to the shell rc\n\n")
	fmt.Fprintf(&b, "**Ask the user one question first** — do they want the alias to also pass\n")
	fmt.Fprintf(&b, "`--dangerously-skip-permissions`? Phrase it exactly like this so the user\n")
	fmt.Fprintf(&b, "understands the tradeoff:\n\n")
	fmt.Fprintf(&b, "> Should `claude-cv` skip Claude Code's permission prompts (the\n")
	fmt.Fprintf(&b, "> `--dangerously-skip-permissions` flag)? This means every tool call runs\n")
	fmt.Fprintf(&b, "> immediately without asking you for confirmation — speed at the cost of\n")
	fmt.Fprintf(&b, "> safety. Clawvisor's own gating still applies, but Claude Code's local\n")
	fmt.Fprintf(&b, "> prompts won't. Default is **no**.\n\n")
	fmt.Fprintf(&b, "Remember the answer as `$YOLO` (yes/no). If yes, the rendered function\n")
	fmt.Fprintf(&b, "below adds ` --dangerously-skip-permissions` between `claude` and `\"$@\"`.\n\n")
	fmt.Fprintf(&b, "Detect the user's shell:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "case \"$SHELL\" in\n")
	fmt.Fprintf(&b, "  */zsh)  RC=~/.zshrc ;;\n")
	fmt.Fprintf(&b, "  */bash) RC=~/.bashrc ;;\n")
	fmt.Fprintf(&b, "  */fish) RC=~/.config/fish/config.fish ;;\n")
	fmt.Fprintf(&b, "  *)      RC=\"\"; echo \"unknown shell: $SHELL — append the function manually\" ;;\n")
	fmt.Fprintf(&b, "esac\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Append a `claude-cv` function (leaves bare `claude` untouched). The user's\n")
	fmt.Fprintf(&b, "rc file gets ONLY the function — no marker comments, no Clawvisor-related\n")
	fmt.Fprintf(&b, "annotations. We separately record what we appended to\n")
	fmt.Fprintf(&b, "`~/.clawvisor/diffs/$AGENT_NAME/claude_cv.json` so the uninstall skill can\n")
	fmt.Fprintf(&b, "find and remove the same block by exact-string match.\n\n")
	fmt.Fprintf(&b, "Use the form matching the mode the smoke test passed in. If `$YOLO=yes`,\n")
	fmt.Fprintf(&b, "substitute `claude --dangerously-skip-permissions` everywhere the snippets\n")
	fmt.Fprintf(&b, "below spell `claude`.\n\n")
	fmt.Fprintf(&b, "**If MODE=passthrough** (zsh/bash):\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "CONTENT=$(cat <<EOF\n")
	fmt.Fprintf(&b, "claude-cv() {\n")
	fmt.Fprintf(&b, "  ANTHROPIC_BASE_URL=\"$CLAWVISOR_URL/api\" \\\\\n")
	fmt.Fprintf(&b, "  ANTHROPIC_CUSTOM_HEADERS=\"X-Clawvisor-Agent-Token: \\$(jq -r .token \\$HOME/.clawvisor/agents/$AGENT_NAME.json)\" \\\\\n")
	fmt.Fprintf(&b, "  claude \"\\$@\"\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, ")\n")
	b.WriteString(recordTextDiff("claude_cv", `"$RC"`))
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**If MODE=swap** (zsh/bash):\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "CONTENT=$(cat <<EOF\n")
	fmt.Fprintf(&b, "claude-cv() {\n")
	fmt.Fprintf(&b, "  ANTHROPIC_BASE_URL=\"$CLAWVISOR_URL/api\" \\\\\n")
	fmt.Fprintf(&b, "  ANTHROPIC_AUTH_TOKEN=\"\\$(jq -r .token \\$HOME/.clawvisor/agents/$AGENT_NAME.json)\" \\\\\n")
	fmt.Fprintf(&b, "  ANTHROPIC_API_KEY= \\\\\n")
	fmt.Fprintf(&b, "  claude \"\\$@\"\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, ")\n")
	b.WriteString(recordTextDiff("claude_cv", `"$RC"`))
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "For fish, translate the function syntax accordingly — the same\n")
	fmt.Fprintf(&b, "CONTENT-then-record pattern applies.\n\n")
	fmt.Fprintf(&b, "Tell the user to `source \"$RC\"` (or restart their shell), then run\n")
	fmt.Fprintf(&b, "`claude-cv` instead of `claude` when they want Clawvisor routing.\n\n")

	b.WriteString(sectionSelfUninstallSetup(6, "Claude Code", "claude-code", "~/.claude/commands/clawvisor-uninstall.md", "rm -f ~/.claude/commands/clawvisor-setup.md"))

	return b.String()
}

func renderCodexInstaller(ctx installerCtx) string {
	var b strings.Builder
	b.WriteString(setupFrontmatter("Codex"))
	fmt.Fprintf(&b, "# Connect Codex to Clawvisor\n\n")
	fmt.Fprintf(&b, "You are running a one-shot setup skill. The dashboard pre-baked everything\n")
	fmt.Fprintf(&b, "you need into this file: the Clawvisor URL, a single-use claim code, and\n")
	fmt.Fprintf(&b, "the agent name. The dashboard already approved the connection — no second\n")
	fmt.Fprintf(&b, "click is needed.\n\n")
	fmt.Fprintf(&b, "**Two modes; the smoke test picks one.** Clawvisor's lite-proxy can run in\n")
	fmt.Fprintf(&b, "**passthrough** (the user's existing `codex login` subscription or env\n")
	fmt.Fprintf(&b, "`OPENAI_API_KEY` flows through Clawvisor to OpenAI — keeps their\n")
	fmt.Fprintf(&b, "subscription billing intact) or **swap** (Clawvisor substitutes a vaulted\n")
	fmt.Fprintf(&b, "API key on every call). Passthrough is the default; swap is the fallback\n")
	fmt.Fprintf(&b, "for users with no working upstream auth.\n\n")

	b.WriteString(sectionClaimedConnect("codex", ctx.ClawvisorURL, ctx.Claim, ctx.AgentName))

	// Step 2: write the provider block in passthrough form. `requires_openai_auth
	// = true` makes Codex send its OAuth/env auth as Authorization upstream;
	// the X-Clawvisor-Agent-Token custom header rides alongside for policy ID.
	fmt.Fprintf(&b, "## 2. Write the Clawvisor provider block (passthrough form)\n\n")
	fmt.Fprintf(&b, "Codex reads `~/.codex/config.toml`. We add a `[model_providers.clawvisor]`\n")
	fmt.Fprintf(&b, "block so `codex -c model_provider=clawvisor` (and the smoke test below)\n")
	fmt.Fprintf(&b, "can target it. `requires_openai_auth = true` keeps Codex's normal\n")
	fmt.Fprintf(&b, "subscription / env-key auth flowing through; the cvis_ token rides in a\n")
	fmt.Fprintf(&b, "custom header for policy ID only. (If the smoke test fails because the\n")
	fmt.Fprintf(&b, "user has no working upstream auth, step 3 rewrites this block to swap\n")
	fmt.Fprintf(&b, "form.)\n\n")
	fmt.Fprintf(&b, "**Idempotent — grep first.** Codex rejects duplicate `[model_providers.<n>]`\n")
	fmt.Fprintf(&b, "entries on startup. We append only the block itself; the uninstall trail\n")
	fmt.Fprintf(&b, "lives outside the file in `~/.clawvisor/diffs/$AGENT_NAME/`.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "mkdir -p ~/.codex ~/.clawvisor/diffs/$AGENT_NAME\n")
	fmt.Fprintf(&b, "if ! grep -q '^\\[model_providers\\.clawvisor\\]' ~/.codex/config.toml 2>/dev/null; then\n")
	fmt.Fprintf(&b, "  CONTENT=$(cat <<EOF\n")
	fmt.Fprintf(&b, "[model_providers.clawvisor]\n")
	fmt.Fprintf(&b, "name = \"Clawvisor\"\n")
	fmt.Fprintf(&b, "base_url = \"$CLAWVISOR_URL/api/v1\"\n")
	fmt.Fprintf(&b, "wire_api = \"responses\"\n")
	fmt.Fprintf(&b, "requires_openai_auth = true\n\n")
	fmt.Fprintf(&b, "[model_providers.clawvisor.env_http_headers]\n")
	fmt.Fprintf(&b, "X-Clawvisor-Agent-Token = \"CLAWVISOR_AGENT_TOKEN\"\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, "  )\n")
	fmt.Fprintf(&b, "  printf '\\n%%s\\n' \"$CONTENT\" >> ~/.codex/config.toml\n")
	fmt.Fprintf(&b, "  jq -n --arg file \"$HOME/.codex/config.toml\" --arg content \"$CONTENT\" \\\n")
	fmt.Fprintf(&b, "    '{file: $file, type: \"text_append\", content: $content}' \\\n")
	fmt.Fprintf(&b, "    > ~/.clawvisor/diffs/$AGENT_NAME/provider_block.json\n")
	fmt.Fprintf(&b, "fi\n")
	fmt.Fprintf(&b, "```\n\n")

	// Step 3: smoke test in passthrough mode (block as written in step 2).
	fmt.Fprintf(&b, "## 3. Smoke-test Clawvisor routing in **passthrough mode**\n\n")
	fmt.Fprintf(&b, "Run a fresh `codex` in a child process targeting the block from step 2.\n")
	fmt.Fprintf(&b, "The user's existing `codex login` or env `OPENAI_API_KEY` flows through.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "CLAWVISOR_AGENT_TOKEN=\"$TOKEN\" timeout 30 codex \\\n")
	fmt.Fprintf(&b, "  -c model_provider=clawvisor \\\n")
	fmt.Fprintf(&b, "  exec \"respond with the word OK\"\n")
	fmt.Fprintf(&b, "```\n\n")
	b.WriteString(classifySmokeFailure("Continue to step 4 to vault a key, rewrite the provider block to swap form, and retry."))
	fmt.Fprintf(&b, "**On pass**, the user has working upstream auth. Set `MODE=passthrough` in\n")
	fmt.Fprintf(&b, "your head and skip to step 5.\n\n")

	// Step 4: swap-mode fallback. Vault key, rewrite provider block, retry.
	fmt.Fprintf(&b, "## 4. Fall back to **swap mode** (only if step 3 failed with upstream auth)\n\n")
	fmt.Fprintf(&b, "Step 3 failed because the user has no working upstream auth. We vault a\n")
	fmt.Fprintf(&b, "key, rewrite the provider block so the cvis_ token rides as Authorization\n")
	fmt.Fprintf(&b, "(triggering Clawvisor's swap mode), and re-run the smoke test.\n\n")
	b.WriteString(sectionVaultUpstreamKey("### 4.a. Vault an OpenAI API key", "openai", "OpenAI", "OPENAI_API_KEY", "sk-", "/dashboard/keys/openai"))
	fmt.Fprintf(&b, "### 4.b. Rewrite the provider block to swap form\n\n")
	fmt.Fprintf(&b, "Replace the block written in step 2 with the swap form: `requires_openai_auth\n")
	fmt.Fprintf(&b, "= false` (so Codex doesn't try to send its own Authorization), and an\n")
	fmt.Fprintf(&b, "`env_http_headers.Authorization` that puts the cvis_ token as a Bearer\n")
	fmt.Fprintf(&b, "Authorization header. Clawvisor sees the cvis_ in Authorization and\n")
	fmt.Fprintf(&b, "substitutes the vaulted upstream key.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "# Strip the existing passthrough-form provider_block by exact-string\n")
	fmt.Fprintf(&b, "# match against the diff record from step 2, then append the swap-form\n")
	fmt.Fprintf(&b, "# block and overwrite the diff record.\n")
	fmt.Fprintf(&b, "python3 - <<'PY'\n")
	fmt.Fprintf(&b, "import json, os\n")
	fmt.Fprintf(&b, "agent = os.environ['AGENT_NAME']\n")
	fmt.Fprintf(&b, "diff_path = os.path.expanduser(f'~/.clawvisor/diffs/{agent}/provider_block.json')\n")
	fmt.Fprintf(&b, "with open(diff_path) as f: rec = json.load(f)\n")
	fmt.Fprintf(&b, "target = os.path.expanduser('~/.codex/config.toml')\n")
	fmt.Fprintf(&b, "with open(target) as f: body = f.read()\n")
	fmt.Fprintf(&b, "needle = '\\n' + rec['content'] + '\\n'\n")
	fmt.Fprintf(&b, "if needle in body:\n")
	fmt.Fprintf(&b, "    body = body.replace(needle, '', 1)\n")
	fmt.Fprintf(&b, "    with open(target, 'w') as f: f.write(body)\n")
	fmt.Fprintf(&b, "PY\n")
	fmt.Fprintf(&b, "CONTENT=$(cat <<EOF\n")
	fmt.Fprintf(&b, "[model_providers.clawvisor]\n")
	fmt.Fprintf(&b, "name = \"Clawvisor\"\n")
	fmt.Fprintf(&b, "base_url = \"$CLAWVISOR_URL/api/v1\"\n")
	fmt.Fprintf(&b, "wire_api = \"responses\"\n")
	fmt.Fprintf(&b, "requires_openai_auth = false\n\n")
	fmt.Fprintf(&b, "[model_providers.clawvisor.env_http_headers]\n")
	fmt.Fprintf(&b, "Authorization = \"CLAWVISOR_AGENT_BEARER\"\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, ")\n")
	fmt.Fprintf(&b, "printf '\\n%%s\\n' \"$CONTENT\" >> ~/.codex/config.toml\n")
	fmt.Fprintf(&b, "jq -n --arg file \"$HOME/.codex/config.toml\" --arg content \"$CONTENT\" \\\n")
	fmt.Fprintf(&b, "  '{file: $file, type: \"text_append\", content: $content}' \\\n")
	fmt.Fprintf(&b, "  > ~/.clawvisor/diffs/$AGENT_NAME/provider_block.json\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "### 4.c. Re-run the smoke test in swap mode\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "CLAWVISOR_AGENT_BEARER=\"Bearer $TOKEN\" timeout 30 codex \\\n")
	fmt.Fprintf(&b, "  -c model_provider=clawvisor \\\n")
	fmt.Fprintf(&b, "  exec \"respond with the word OK\"\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**Pass criteria:** exit code 0 AND stdout contains `OK`.\n\n")
	fmt.Fprintf(&b, "**On pass**, the vaulted key works. Set `MODE=swap` in your head and\n")
	fmt.Fprintf(&b, "continue to step 5.\n\n")
	fmt.Fprintf(&b, "**On fail**, the vaulted key is probably wrong (or someone revoked it).\n")
	fmt.Fprintf(&b, "Surface the error and STOP — don't loop back to vault again from this\n")
	fmt.Fprintf(&b, "session.\n\n")

	// Step 5: the make-default question.
	fmt.Fprintf(&b, "## 5. Ask the user: make Clawvisor the default?\n\n")
	fmt.Fprintf(&b, "Smoke test passed (in either passthrough or swap mode — `$MODE` is set).\n")
	fmt.Fprintf(&b, "Now ask exactly one question — verbatim or close to it:\n\n")
	fmt.Fprintf(&b, "> Make Clawvisor the default for every Codex session? I'll set\n")
	fmt.Fprintf(&b, "> `model_provider = \"clawvisor\"` at the top of `~/.codex/config.toml` so\n")
	fmt.Fprintf(&b, "> all future `codex` invocations route through Clawvisor automatically.\n")
	fmt.Fprintf(&b, "> \n")
	fmt.Fprintf(&b, "> The alternative is a `codex-cv` shell function — your regular `codex`\n")
	fmt.Fprintf(&b, "> stays exactly as it is, and you opt into Clawvisor routing by typing\n")
	fmt.Fprintf(&b, "> `codex-cv` instead.\n\n")
	fmt.Fprintf(&b, "- **YES (default-everywhere)** → do step 6.a.\n")
	fmt.Fprintf(&b, "- **NO (alias-only)** → do step 6.b.\n\n")

	// Step 6: apply. Both branches need a small shell-rc export of the right
	// env var (CLAWVISOR_AGENT_TOKEN for passthrough, CLAWVISOR_AGENT_BEARER
	// for swap). The provider block is already in the right form.
	fmt.Fprintf(&b, "## 6. Apply the user's choice\n\n")
	fmt.Fprintf(&b, "### 6.a. Default-everywhere — set `model_provider = \"clawvisor\"` as the default\n\n")
	fmt.Fprintf(&b, "Prepend a top-level `model_provider = \"clawvisor\"` line to\n")
	fmt.Fprintf(&b, "`~/.codex/config.toml` (outside any `[…]` section). Record the diff so\n")
	fmt.Fprintf(&b, "the uninstall can find and remove this exact line:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "if ! grep -q '^model_provider = \"clawvisor\"$' ~/.codex/config.toml 2>/dev/null; then\n")
	fmt.Fprintf(&b, "  CONTENT='model_provider = \"clawvisor\"'\n")
	fmt.Fprintf(&b, "  { printf '%%s\\n\\n' \"$CONTENT\"; cat ~/.codex/config.toml; } > ~/.codex/config.toml.new && \\\n")
	fmt.Fprintf(&b, "    mv ~/.codex/config.toml.new ~/.codex/config.toml\n")
	fmt.Fprintf(&b, "  jq -n --arg file \"$HOME/.codex/config.toml\" --arg content \"$CONTENT\" \\\n")
	fmt.Fprintf(&b, "    '{file: $file, type: \"text_prepend\", content: $content}' \\\n")
	fmt.Fprintf(&b, "    > ~/.clawvisor/diffs/$AGENT_NAME/default_provider.json\n")
	fmt.Fprintf(&b, "fi\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Then make sure the right env var is exported for every `codex` invocation.\n")
	fmt.Fprintf(&b, "The export line is appended without marker comments; the uninstall finds\n")
	fmt.Fprintf(&b, "it via the recorded diff.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "case \"$SHELL\" in\n")
	fmt.Fprintf(&b, "  */zsh)  RC=~/.zshrc ;;\n")
	fmt.Fprintf(&b, "  */bash) RC=~/.bashrc ;;\n")
	fmt.Fprintf(&b, "  *)      RC=\"\"; echo \"unknown shell: $SHELL — export manually\" ;;\n")
	fmt.Fprintf(&b, "esac\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**If MODE=passthrough**, export `CLAWVISOR_AGENT_TOKEN`:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "if [ -n \"$RC\" ]; then\n")
	fmt.Fprintf(&b, "  CONTENT=\"export CLAWVISOR_AGENT_TOKEN=\\$(jq -r .token \\$HOME/.clawvisor/agents/$AGENT_NAME.json)\"\n")
	fmt.Fprintf(&b, "  printf '\\n%%s\\n' \"$CONTENT\" >> \"$RC\"\n")
	fmt.Fprintf(&b, "  jq -n --arg file \"$RC\" --arg content \"$CONTENT\" \\\n")
	fmt.Fprintf(&b, "    '{file: $file, type: \"text_append\", content: $content}' \\\n")
	fmt.Fprintf(&b, "    > ~/.clawvisor/diffs/$AGENT_NAME/rc_export.json\n")
	fmt.Fprintf(&b, "fi\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**If MODE=swap**, export `CLAWVISOR_AGENT_BEARER` (the `Bearer …` form Codex\n")
	fmt.Fprintf(&b, "sends as Authorization):\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "if [ -n \"$RC\" ]; then\n")
	fmt.Fprintf(&b, "  CONTENT=\"export CLAWVISOR_AGENT_BEARER=\\\"Bearer \\$(jq -r .token \\$HOME/.clawvisor/agents/$AGENT_NAME.json)\\\"\"\n")
	fmt.Fprintf(&b, "  printf '\\n%%s\\n' \"$CONTENT\" >> \"$RC\"\n")
	fmt.Fprintf(&b, "  jq -n --arg file \"$RC\" --arg content \"$CONTENT\" \\\n")
	fmt.Fprintf(&b, "    '{file: $file, type: \"text_append\", content: $content}' \\\n")
	fmt.Fprintf(&b, "    > ~/.clawvisor/diffs/$AGENT_NAME/rc_export.json\n")
	fmt.Fprintf(&b, "fi\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Tell the user the new routing takes effect on their next shell or next\n")
	fmt.Fprintf(&b, "`codex` invocation. Then jump to step 7 (self-uninstall).\n\n")

	fmt.Fprintf(&b, "### 6.b. Alias-only — append `codex-cv` to the shell rc\n\n")
	fmt.Fprintf(&b, "**Ask the user one question first** — do they want the alias to also pass\n")
	fmt.Fprintf(&b, "`--dangerously-bypass-approvals-and-sandbox` (Codex's equivalent of\n")
	fmt.Fprintf(&b, "`--dangerously-skip-permissions`)? Phrase it exactly like this:\n\n")
	fmt.Fprintf(&b, "> Should `codex-cv` skip Codex's approval prompts and sandbox restrictions\n")
	fmt.Fprintf(&b, "> (the `--dangerously-bypass-approvals-and-sandbox` flag)? This means every\n")
	fmt.Fprintf(&b, "> tool call runs immediately without asking you for confirmation —\n")
	fmt.Fprintf(&b, "> speed at the cost of safety. Clawvisor's own gating still applies, but\n")
	fmt.Fprintf(&b, "> Codex's local prompts and sandbox won't. Default is **no**.\n\n")
	fmt.Fprintf(&b, "Remember the answer as `$YOLO` (yes/no). If yes, the rendered function\n")
	fmt.Fprintf(&b, "below adds ` --dangerously-bypass-approvals-and-sandbox` between `codex`\n")
	fmt.Fprintf(&b, "and the `-c model_provider=clawvisor` flag.\n\n")
	fmt.Fprintf(&b, "Append a `codex-cv` function (leaves bare `codex` untouched). The rc file\n")
	fmt.Fprintf(&b, "gets only the function — the uninstall trail lives in\n")
	fmt.Fprintf(&b, "`~/.clawvisor/diffs/$AGENT_NAME/codex_cv.json`.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "case \"$SHELL\" in\n")
	fmt.Fprintf(&b, "  */zsh)  RC=~/.zshrc ;;\n")
	fmt.Fprintf(&b, "  */bash) RC=~/.bashrc ;;\n")
	fmt.Fprintf(&b, "  */fish) RC=~/.config/fish/config.fish ;;\n")
	fmt.Fprintf(&b, "  *)      RC=\"\"; echo \"unknown shell: $SHELL — append the function manually\" ;;\n")
	fmt.Fprintf(&b, "esac\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**If MODE=passthrough** (zsh/bash):\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "CONTENT=$(cat <<EOF\n")
	fmt.Fprintf(&b, "codex-cv() {\n")
	fmt.Fprintf(&b, "  CLAWVISOR_AGENT_TOKEN=\\$(jq -r .token \\$HOME/.clawvisor/agents/$AGENT_NAME.json) \\\\\n")
	fmt.Fprintf(&b, "  codex -c model_provider=clawvisor \"\\$@\"\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, ")\n")
	b.WriteString(recordTextDiff("codex_cv", `"$RC"`))
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**If MODE=swap** (zsh/bash):\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "CONTENT=$(cat <<EOF\n")
	fmt.Fprintf(&b, "codex-cv() {\n")
	fmt.Fprintf(&b, "  CLAWVISOR_AGENT_BEARER=\"Bearer \\$(jq -r .token \\$HOME/.clawvisor/agents/$AGENT_NAME.json)\" \\\\\n")
	fmt.Fprintf(&b, "  codex -c model_provider=clawvisor \"\\$@\"\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, ")\n")
	b.WriteString(recordTextDiff("codex_cv", `"$RC"`))
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Tell the user to `source \"$RC\"` (or restart their shell), then run\n")
	fmt.Fprintf(&b, "`codex-cv` instead of `codex` when they want Clawvisor routing.\n\n")

	b.WriteString(sectionSelfUninstallSetup(7, "Codex", "codex", "~/.codex/skills/clawvisor-uninstall/SKILL.md", "rm -rf ~/.codex/skills/clawvisor-setup"))

	return b.String()
}

// ── Uninstall skill renderers ────────────────────────────────────────────────
//
// The install skill drops these to disk as its last action so the user has a
// one-command revert path. Each is mode-detecting: it reads the current
// config to figure out whether the user installed in default-everywhere or
// alias mode (and passthrough vs swap submode) and reverses the right changes.

func renderClaudeCodeUninstaller(ctx installerCtx) string {
	var b strings.Builder
	b.WriteString(uninstallFrontmatter("Claude Code"))
	fmt.Fprintf(&b, "# Uninstall Clawvisor from Claude Code\n\n")
	fmt.Fprintf(&b, "You are reverting the Clawvisor setup. The install skill wrote this file\n")
	fmt.Fprintf(&b, "so the user has a one-command revert path. Walk the user through each step\n")
	fmt.Fprintf(&b, "and confirm before destructive actions.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "export AGENT_NAME=%q\n", ctx.AgentName)
	fmt.Fprintf(&b, "export TOKEN_FILE=~/.clawvisor/agents/$AGENT_NAME.json\n")
	fmt.Fprintf(&b, "```\n\n")

	fmt.Fprintf(&b, "## 1. Detect the install mode\n\n")
	fmt.Fprintf(&b, "Read the current Claude Code config to figure out what was installed:\n\n")
	fmt.Fprintf(&b, "- **Default-everywhere (settings.json)** — `~/.claude/settings.json` has an\n")
	fmt.Fprintf(&b, "  `env` block containing `ANTHROPIC_BASE_URL` that points at a Clawvisor URL.\n")
	fmt.Fprintf(&b, "  If `env.ANTHROPIC_AUTH_TOKEN` is set to a `cvis_…` value it's swap-submode;\n")
	fmt.Fprintf(&b, "  if `env.ANTHROPIC_CUSTOM_HEADERS` contains `X-Clawvisor-Agent-Token` it's\n")
	fmt.Fprintf(&b, "  passthrough-submode.\n")
	fmt.Fprintf(&b, "- **Alias-only (shell rc)** — `~/.zshrc` / `~/.bashrc` / fish config has a\n")
	fmt.Fprintf(&b, "  `claude-cv()` function (zsh/bash) or `function claude-cv` (fish).\n")
	fmt.Fprintf(&b, "- **Both** — possible if the user ran the install twice in different modes.\n")
	fmt.Fprintf(&b, "  Revert each.\n")
	fmt.Fprintf(&b, "- **Neither** — nothing to revert; jump to step 3.\n\n")
	fmt.Fprintf(&b, "Tell the user what you found and confirm before changing anything.\n\n")

	fmt.Fprintf(&b, "## 2. Reverse the config from the diff records\n\n")
	fmt.Fprintf(&b, "The install left a precise trail of every modification it made under\n")
	fmt.Fprintf(&b, "`~/.clawvisor/diffs/$AGENT_NAME/` — one tiny JSON file per modification.\n")
	fmt.Fprintf(&b, "Each record names a target file, a diff type, and either the JSON paths\n")
	fmt.Fprintf(&b, "added (for JSON files) or the literal text content appended/prepended\n")
	fmt.Fprintf(&b, "(for text files). User files were modified without any marker comments\n")
	fmt.Fprintf(&b, "or sentinel keys, so they stay clean either way.\n\n")
	fmt.Fprintf(&b, "List the records:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "ls ~/.clawvisor/diffs/$AGENT_NAME/ 2>/dev/null || echo \"no diff records — skip to step 3\"\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Walk each record and reverse it. Use this Python one-liner (python3 ships\n")
	fmt.Fprintf(&b, "with macOS and every modern Linux). It handles every diff type and is\n")
	fmt.Fprintf(&b, "idempotent — re-running it after a partial uninstall is safe:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "python3 - <<'PY'\n")
	fmt.Fprintf(&b, "import json, os, glob\n")
	fmt.Fprintf(&b, "agent = os.environ['AGENT_NAME']\n")
	fmt.Fprintf(&b, "diffs_dir = os.path.expanduser(f'~/.clawvisor/diffs/{agent}')\n")
	fmt.Fprintf(&b, "for path in sorted(glob.glob(os.path.join(diffs_dir, '*.json'))):\n")
	fmt.Fprintf(&b, "    with open(path) as f: rec = json.load(f)\n")
	fmt.Fprintf(&b, "    target = os.path.expanduser(rec['file'])\n")
	fmt.Fprintf(&b, "    if not os.path.exists(target): continue\n")
	fmt.Fprintf(&b, "    if rec['type'] == 'json_keys':\n")
	fmt.Fprintf(&b, "        with open(target) as f: doc = json.load(f)\n")
	fmt.Fprintf(&b, "        for dotted in rec['paths']:\n")
	fmt.Fprintf(&b, "            cur = doc; parts = dotted.split('.')\n")
	fmt.Fprintf(&b, "            for p in parts[:-1]:\n")
	fmt.Fprintf(&b, "                if not isinstance(cur, dict) or p not in cur: cur = None; break\n")
	fmt.Fprintf(&b, "                cur = cur[p]\n")
	fmt.Fprintf(&b, "            if cur is not None and isinstance(cur, dict): cur.pop(parts[-1], None)\n")
	fmt.Fprintf(&b, "        # Drop empty parent objects we created.\n")
	fmt.Fprintf(&b, "        def prune(d):\n")
	fmt.Fprintf(&b, "            for k, v in list(d.items()):\n")
	fmt.Fprintf(&b, "                if isinstance(v, dict): prune(v)\n")
	fmt.Fprintf(&b, "                if isinstance(v, dict) and not v: del d[k]\n")
	fmt.Fprintf(&b, "        prune(doc)\n")
	fmt.Fprintf(&b, "        with open(target, 'w') as f: json.dump(doc, f, indent=2); f.write('\\n')\n")
	fmt.Fprintf(&b, "    elif rec['type'] in ('text_append', 'text_prepend'):\n")
	fmt.Fprintf(&b, "        with open(target) as f: body = f.read()\n")
	fmt.Fprintf(&b, "        chunk = rec['content']\n")
	fmt.Fprintf(&b, "        # Try variants with surrounding whitespace the install added.\n")
	fmt.Fprintf(&b, "        for needle in ('\\n' + chunk + '\\n', chunk + '\\n\\n', chunk + '\\n', chunk):\n")
	fmt.Fprintf(&b, "            if needle in body:\n")
	fmt.Fprintf(&b, "                body = body.replace(needle, '', 1); break\n")
	fmt.Fprintf(&b, "        with open(target, 'w') as f: f.write(body)\n")
	fmt.Fprintf(&b, "PY\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If `~/.clawvisor/diffs/$AGENT_NAME/` is missing entirely (legacy install\n")
	fmt.Fprintf(&b, "or user-deleted), fall back to surgical removal:\n\n")
	fmt.Fprintf(&b, "- `~/.claude/settings.json`: delete `env.ANTHROPIC_BASE_URL`,\n")
	fmt.Fprintf(&b, "  `env.ANTHROPIC_CUSTOM_HEADERS`, `env.ANTHROPIC_AUTH_TOKEN`, and\n")
	fmt.Fprintf(&b, "  `env.ANTHROPIC_API_KEY` (the last only if it was set to `\"\"` — don't\n")
	fmt.Fprintf(&b, "  clobber a real key).\n")
	fmt.Fprintf(&b, "- Shell rc: find and delete any `claude-cv()` / `function claude-cv … end`\n")
	fmt.Fprintf(&b, "  block. Confirm with the user before writing.\n\n")
	fmt.Fprintf(&b, "Tell the user: the next `claude` session will use their pre-Clawvisor\n")
	fmt.Fprintf(&b, "auth (`claude login` or env API key). The currently-running session\n")
	fmt.Fprintf(&b, "keeps the Clawvisor routing until it restarts. If you removed an alias,\n")
	fmt.Fprintf(&b, "`source` the rc file to drop the function from their live session.\n\n")

	fmt.Fprintf(&b, "## 3. Delete the local token file\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "rm -f \"$TOKEN_FILE\"\n")
	fmt.Fprintf(&b, "```\n\n")

	fmt.Fprintf(&b, "## 4. Tell the user about dashboard cleanup\n\n")
	fmt.Fprintf(&b, "The agent token still exists server-side (Clawvisor doesn't know we removed\n")
	fmt.Fprintf(&b, "the local config). Surface these so the user can finish the revert:\n\n")
	fmt.Fprintf(&b, "- **Revoke the agent.** Open `%s/dashboard/agents`, find `$AGENT_NAME`, and\n", ctx.ClawvisorURL)
	fmt.Fprintf(&b, "  delete it. After delete, the token in `$TOKEN_FILE` (now gone anyway)\n")
	fmt.Fprintf(&b, "  authenticates nothing.\n")
	fmt.Fprintf(&b, "- **Vaulted upstream key (only if you used swap mode).** If you vaulted an\n")
	fmt.Fprintf(&b, "  Anthropic API key during install and don't want Clawvisor to keep it,\n")
	fmt.Fprintf(&b, "  open `%s/dashboard/keys/anthropic` and replace or clear it. Skip this\n", ctx.ClawvisorURL)
	fmt.Fprintf(&b, "  if other agents are still using the vaulted key.\n\n")
	fmt.Fprintf(&b, "Do NOT delete the vaulted key on the user's behalf — it may be shared with\n")
	fmt.Fprintf(&b, "other agents the user wants to keep working.\n\n")

	fmt.Fprintf(&b, "## 5. Self-uninstall\n\n")
	fmt.Fprintf(&b, "Diff records are consumed; remove them and this uninstall skill:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "rm -rf ~/.clawvisor/diffs/$AGENT_NAME\n")
	fmt.Fprintf(&b, "rm -f ~/.claude/commands/clawvisor-uninstall.md\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Tell the user: Clawvisor routing is fully off for Claude Code on this\n")
	fmt.Fprintf(&b, "machine. To reconnect later, paste a fresh one-liner from the dashboard.\n")
	return b.String()
}

func renderCodexUninstaller(ctx installerCtx) string {
	var b strings.Builder
	b.WriteString(uninstallFrontmatter("Codex"))
	fmt.Fprintf(&b, "# Uninstall Clawvisor from Codex\n\n")
	fmt.Fprintf(&b, "You are reverting the Clawvisor setup. The install skill wrote this file\n")
	fmt.Fprintf(&b, "so the user has a one-command revert path. Walk the user through each step\n")
	fmt.Fprintf(&b, "and confirm before destructive actions.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "export AGENT_NAME=%q\n", ctx.AgentName)
	fmt.Fprintf(&b, "export TOKEN_FILE=~/.clawvisor/agents/$AGENT_NAME.json\n")
	fmt.Fprintf(&b, "```\n\n")

	fmt.Fprintf(&b, "## 1. Detect the install state\n\n")
	fmt.Fprintf(&b, "Read `~/.codex/config.toml` and the user's shell rc files:\n\n")
	fmt.Fprintf(&b, "- **Provider block present** — config.toml has a `[model_providers.clawvisor]`\n")
	fmt.Fprintf(&b, "  block. Always installed by the install skill regardless of mode.\n")
	fmt.Fprintf(&b, "- **Default-everywhere** — config.toml has a top-level\n")
	fmt.Fprintf(&b, "  `model_provider = \"clawvisor\"` line (outside any `[…]` section), and\n")
	fmt.Fprintf(&b, "  the shell rc has an `export CLAWVISOR_AGENT_TOKEN=…` or\n")
	fmt.Fprintf(&b, "  `export CLAWVISOR_AGENT_BEARER=…` line pointing at\n")
	fmt.Fprintf(&b, "  `~/.clawvisor/agents/$AGENT_NAME.json`.\n")
	fmt.Fprintf(&b, "- **Alias-only** — shell rc has a `codex-cv()` function (zsh/bash) or\n")
	fmt.Fprintf(&b, "  `function codex-cv` (fish).\n")
	fmt.Fprintf(&b, "- **Submode** — if the provider block has `requires_openai_auth = true`\n")
	fmt.Fprintf(&b, "  it's passthrough; `false` (with an `Authorization` entry in\n")
	fmt.Fprintf(&b, "  `env_http_headers`) is swap.\n\n")
	fmt.Fprintf(&b, "Tell the user what you found and confirm before changing anything.\n\n")

	fmt.Fprintf(&b, "## 2. Reverse the config from the diff records\n\n")
	fmt.Fprintf(&b, "The install left a precise trail under `~/.clawvisor/diffs/$AGENT_NAME/`\n")
	fmt.Fprintf(&b, "— one tiny JSON file per modification, no marker comments or sentinel\n")
	fmt.Fprintf(&b, "keys in the user's files. Each record holds the target path, the diff\n")
	fmt.Fprintf(&b, "type, and either the JSON paths added or the literal text content\n")
	fmt.Fprintf(&b, "appended/prepended.\n\n")
	fmt.Fprintf(&b, "List the records:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "ls ~/.clawvisor/diffs/$AGENT_NAME/ 2>/dev/null || echo \"no diff records — skip to step 3\"\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Walk every record and reverse it. The same python3 one-liner from the\n")
	fmt.Fprintf(&b, "Claude Code uninstall handles every diff type — it's harness-agnostic:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "python3 - <<'PY'\n")
	fmt.Fprintf(&b, "import json, os, glob\n")
	fmt.Fprintf(&b, "agent = os.environ['AGENT_NAME']\n")
	fmt.Fprintf(&b, "diffs_dir = os.path.expanduser(f'~/.clawvisor/diffs/{agent}')\n")
	fmt.Fprintf(&b, "for path in sorted(glob.glob(os.path.join(diffs_dir, '*.json'))):\n")
	fmt.Fprintf(&b, "    with open(path) as f: rec = json.load(f)\n")
	fmt.Fprintf(&b, "    target = os.path.expanduser(rec['file'])\n")
	fmt.Fprintf(&b, "    if not os.path.exists(target): continue\n")
	fmt.Fprintf(&b, "    if rec['type'] == 'json_keys':\n")
	fmt.Fprintf(&b, "        with open(target) as f: doc = json.load(f)\n")
	fmt.Fprintf(&b, "        for dotted in rec['paths']:\n")
	fmt.Fprintf(&b, "            cur = doc; parts = dotted.split('.')\n")
	fmt.Fprintf(&b, "            for p in parts[:-1]:\n")
	fmt.Fprintf(&b, "                if not isinstance(cur, dict) or p not in cur: cur = None; break\n")
	fmt.Fprintf(&b, "                cur = cur[p]\n")
	fmt.Fprintf(&b, "            if cur is not None and isinstance(cur, dict): cur.pop(parts[-1], None)\n")
	fmt.Fprintf(&b, "        def prune(d):\n")
	fmt.Fprintf(&b, "            for k, v in list(d.items()):\n")
	fmt.Fprintf(&b, "                if isinstance(v, dict): prune(v)\n")
	fmt.Fprintf(&b, "                if isinstance(v, dict) and not v: del d[k]\n")
	fmt.Fprintf(&b, "        prune(doc)\n")
	fmt.Fprintf(&b, "        with open(target, 'w') as f: json.dump(doc, f, indent=2); f.write('\\n')\n")
	fmt.Fprintf(&b, "    elif rec['type'] in ('text_append', 'text_prepend'):\n")
	fmt.Fprintf(&b, "        with open(target) as f: body = f.read()\n")
	fmt.Fprintf(&b, "        chunk = rec['content']\n")
	fmt.Fprintf(&b, "        for needle in ('\\n' + chunk + '\\n', chunk + '\\n\\n', chunk + '\\n', chunk):\n")
	fmt.Fprintf(&b, "            if needle in body:\n")
	fmt.Fprintf(&b, "                body = body.replace(needle, '', 1); break\n")
	fmt.Fprintf(&b, "        with open(target, 'w') as f: f.write(body)\n")
	fmt.Fprintf(&b, "PY\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If `~/.clawvisor/diffs/$AGENT_NAME/` is missing (legacy install or\n")
	fmt.Fprintf(&b, "user-deleted), fall back to surgical removal:\n\n")
	fmt.Fprintf(&b, "- `~/.codex/config.toml`: strip the `[model_providers.clawvisor]` block\n")
	fmt.Fprintf(&b, "  (everything between the table header and the next `[…]` header) and\n")
	fmt.Fprintf(&b, "  delete any top-level `model_provider = \"clawvisor\"` line.\n")
	fmt.Fprintf(&b, "  ```bash\n")
	fmt.Fprintf(&b, "  awk 'BEGIN{skip=0} /^\\[model_providers\\.clawvisor/{skip=1; next} /^\\[/ && skip{skip=0} !skip' \\\n")
	fmt.Fprintf(&b, "    ~/.codex/config.toml > ~/.codex/config.toml.new && mv ~/.codex/config.toml.new ~/.codex/config.toml\n")
	fmt.Fprintf(&b, "  sed -i.bak '/^model_provider = \"clawvisor\"$/d' ~/.codex/config.toml\n")
	fmt.Fprintf(&b, "  rm -f ~/.codex/config.toml.bak\n")
	fmt.Fprintf(&b, "  ```\n")
	fmt.Fprintf(&b, "- Shell rc: surgically delete any `export CLAWVISOR_AGENT_TOKEN=…` /\n")
	fmt.Fprintf(&b, "  `export CLAWVISOR_AGENT_BEARER=…` line referencing this agent's token\n")
	fmt.Fprintf(&b, "  file, and any `codex-cv()` / `function codex-cv` block. Confirm with\n")
	fmt.Fprintf(&b, "  the user before writing.\n\n")
	fmt.Fprintf(&b, "Tell the user to `source` the rc file (or restart their shell) to drop\n")
	fmt.Fprintf(&b, "the definitions from their live session.\n\n")

	fmt.Fprintf(&b, "## 3. Delete the local token file\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "rm -f \"$TOKEN_FILE\"\n")
	fmt.Fprintf(&b, "```\n\n")

	fmt.Fprintf(&b, "## 4. Tell the user about dashboard cleanup\n\n")
	fmt.Fprintf(&b, "- **Revoke the agent** at `%s/dashboard/agents` — find `$AGENT_NAME` and\n", ctx.ClawvisorURL)
	fmt.Fprintf(&b, "  delete it.\n")
	fmt.Fprintf(&b, "- **Vaulted upstream key (only if you used swap mode)** — open\n")
	fmt.Fprintf(&b, "  `%s/dashboard/keys/openai` if you want to replace or clear the\n", ctx.ClawvisorURL)
	fmt.Fprintf(&b, "  vaulted key. Skip if other agents are still using it.\n\n")
	fmt.Fprintf(&b, "Do NOT delete the vaulted key on the user's behalf.\n\n")

	fmt.Fprintf(&b, "## 5. Self-uninstall\n\n")
	fmt.Fprintf(&b, "Diff records are consumed; remove them and this uninstall skill:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "rm -rf ~/.clawvisor/diffs/$AGENT_NAME\n")
	fmt.Fprintf(&b, "rm -rf ~/.codex/skills/clawvisor-uninstall\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Tell the user: Clawvisor routing is fully off for Codex on this machine. To\n")
	fmt.Fprintf(&b, "reconnect later, paste a fresh one-liner from the dashboard.\n")
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
	fmt.Fprintf(&b, "The agent token has **already been minted** by the dashboard's bootstrap\n")
	fmt.Fprintf(&b, "script and saved to `~/.clawvisor/agents/%s.json`. Do not re-mint;\n", ctx.AgentName)
	fmt.Fprintf(&b, "the configure step below reads the token from disk.\n\n")
	fmt.Fprintf(&b, "Set the endpoint:\n\n```bash\nexport CLAWVISOR_URL=%s\n```\n\n", ctx.ClawvisorURL)

	b.WriteString(sectionDashboardAnswers(ctx,
		"LLM provider: "+providerName,
		"Hermes configuration mode: "+ctx.HermesConfig,
		"Hermes running mode: "+ctx.HermesMode))

	if ctx.HermesMode == "remote" {
		b.WriteString(sectionHermesRemoteProbe())
	} else {
		b.WriteString(sectionHermesLocalProbe(ctx.HermesMode))
	}

	b.WriteString(sectionHermesPreflight(ctx.HermesMode, ctx.ClawvisorURL, ctx.AgentName))

	// Step 3: Configure (mode-aware).
	fmt.Fprintf(&b, "## 3. Configure Hermes\n\n")
	if ctx.HermesConfig == "file" {
		fmt.Fprintf(&b, "The user chose the persistent config-file path. Prefer the snippet that\n")
		fmt.Fprintf(&b, "writes `~/.hermes/config.yaml`; the env-var snippet is here as a fallback.\n\n")
	} else {
		fmt.Fprintf(&b, "The user chose the env-var launch path. Prefer the env-var snippet; the\n")
		fmt.Fprintf(&b, "config-file snippet is here as a fallback for set-and-forget setups.\n\n")
	}

	switch ctx.HermesMode {
	case "docker":
		basePathHost := dockerHostURL(ctx.ClawvisorURL) + basePath
		fmt.Fprintf(&b, "**Env-var (recommended) — pass the token into the container at run time:**\n\n")
		fmt.Fprintf(&b, "```bash\n")
		fmt.Fprintf(&b, "TOKEN=$(jq -r .token ~/.clawvisor/agents/%s.json)\n", ctx.AgentName)
		fmt.Fprintf(&b, "docker compose run --rm \\\n")
		fmt.Fprintf(&b, "  -e %s=\"%s\" \\\n", baseEnv, basePathHost)
		fmt.Fprintf(&b, "  -e %s=\"$TOKEN\" \\\n", keyEnv)
		fmt.Fprintf(&b, "  hermes hermes chat\n")
		fmt.Fprintf(&b, "```\n\n")
		fmt.Fprintf(&b, "Replace `hermes` (the compose service name) with whatever the probe in\n")
		fmt.Fprintf(&b, "step 1 found.\n\n")
		fmt.Fprintf(&b, "**Config file (persistent) — write to a host path mounted into the container:**\n\n")
		fmt.Fprintf(&b, "```bash\n")
		fmt.Fprintf(&b, "TOKEN=$(jq -r .token ~/.clawvisor/agents/%s.json)\n", ctx.AgentName)
		fmt.Fprintf(&b, "mkdir -p ~/.hermes && cat > ~/.hermes/config.yaml <<EOF\n")
		fmt.Fprintf(&b, "model:\n")
		fmt.Fprintf(&b, "  provider: custom\n")
		fmt.Fprintf(&b, "  base_url: \"%s\"\n", basePathHost)
		fmt.Fprintf(&b, "  api_key: \"$TOKEN\"\n")
		fmt.Fprintf(&b, "EOF\n")
		fmt.Fprintf(&b, "```\n\n")
		fmt.Fprintf(&b, "Make sure `~/.hermes` is mounted into the container at the path Hermes\n")
		fmt.Fprintf(&b, "reads from (commonly `/root/.hermes`) via a `volumes:` entry in\n")
		fmt.Fprintf(&b, "docker-compose.yaml. The probe in step 1 should have surfaced the existing\n")
		fmt.Fprintf(&b, "mount.\n\n")
	case "remote":
		fmt.Fprintf(&b, "Reuse `$HERMES_REMOTE` from the probe and `$HERMES_CLAWVISOR_URL` from\n")
		fmt.Fprintf(&b, "the preflight (already proved reachable from `$HERMES_REMOTE`). The\n")
		fmt.Fprintf(&b, "launch wrapper appends `%s` for the %s base URL.\n\n", basePath, providerName)
		fmt.Fprintf(&b, "```bash\n")
		fmt.Fprintf(&b, "TOKEN=$(jq -r .token ~/.clawvisor/agents/%s.json)\n", ctx.AgentName)
		fmt.Fprintf(&b, "```\n\n")
		fmt.Fprintf(&b, "**Env-var (recommended) — wrap each launch with the SSH call:**\n\n")
		fmt.Fprintf(&b, "```bash\n")
		fmt.Fprintf(&b, "ssh \"$HERMES_REMOTE\" \"%s='$HERMES_CLAWVISOR_URL%s' %s='$TOKEN' hermes chat\"\n", baseEnv, basePath, keyEnv)
		fmt.Fprintf(&b, "```\n\n")
		fmt.Fprintf(&b, "**Config file (persistent) — write to the remote's `~/.hermes/config.yaml`:**\n\n")
		fmt.Fprintf(&b, "```bash\n")
		fmt.Fprintf(&b, "ssh \"$HERMES_REMOTE\" \"mkdir -p ~/.hermes && cat > ~/.hermes/config.yaml\" <<EOF\n")
		fmt.Fprintf(&b, "model:\n")
		fmt.Fprintf(&b, "  provider: custom\n")
		fmt.Fprintf(&b, "  base_url: \"$HERMES_CLAWVISOR_URL%s\"\n", basePath)
		fmt.Fprintf(&b, "  api_key: \"$TOKEN\"\n")
		fmt.Fprintf(&b, "EOF\n")
		fmt.Fprintf(&b, "```\n\n")
		fmt.Fprintf(&b, "Re-bootstrapping rotates the token; if you took the config-file path the\n")
		fmt.Fprintf(&b, "user must re-run this snippet after each rotation.\n\n")
	default: // host
		fmt.Fprintf(&b, "**Env-var (recommended):**\n\n")
		fmt.Fprintf(&b, "```bash\n")
		fmt.Fprintf(&b, "%s=%s%s \\\n", baseEnv, ctx.ClawvisorURL, basePath)
		fmt.Fprintf(&b, "%s=$(jq -r .token ~/.clawvisor/agents/%s.json) \\\n", keyEnv, ctx.AgentName)
		fmt.Fprintf(&b, "hermes chat\n")
		fmt.Fprintf(&b, "```\n\n")
		fmt.Fprintf(&b, "**Config file (persistent):**\n\n")
		fmt.Fprintf(&b, "```bash\n")
		fmt.Fprintf(&b, "mkdir -p ~/.hermes && cat > ~/.hermes/config.yaml <<EOF\n")
		fmt.Fprintf(&b, "model:\n")
		fmt.Fprintf(&b, "  provider: custom\n")
		fmt.Fprintf(&b, "  base_url: \"%s%s\"\n", ctx.ClawvisorURL, basePath)
		fmt.Fprintf(&b, "  api_key: \"$(jq -r .token ~/.clawvisor/agents/%s.json)\"\n", ctx.AgentName)
		fmt.Fprintf(&b, "EOF\n")
		fmt.Fprintf(&b, "```\n\n")
		fmt.Fprintf(&b, "The config-file path bakes the current token into the file; re-bootstrapping\n")
		fmt.Fprintf(&b, "the same agent rotates the token and the user must re-run this snippet.\n\n")
	}

	// Step 4: shell alias — only really useful on the host (the user's
	// terminal *is* the launch environment). Skip for docker/remote where the
	// launch wrapper is more involved than an alias.
	if ctx.HermesMode == "host" {
		fmt.Fprintf(&b, "## 4. Offer a shell alias\n\n")
		fmt.Fprintf(&b, "If they went the env-var route, a shell function keeps it ergonomic:\n\n")
		fmt.Fprintf(&b, "```bash\n")
		fmt.Fprintf(&b, "cat >> ~/.zshrc <<'EOF'\n")
		fmt.Fprintf(&b, "hermes-cv() {\n")
		fmt.Fprintf(&b, "  %s=%s%s \\\n", baseEnv, ctx.ClawvisorURL, basePath)
		fmt.Fprintf(&b, "  %s=$(jq -r .token ~/.clawvisor/agents/%s.json) \\\n", keyEnv, ctx.AgentName)
		fmt.Fprintf(&b, "  hermes \"$@\"\n")
		fmt.Fprintf(&b, "}\n")
		fmt.Fprintf(&b, "EOF\n")
		fmt.Fprintf(&b, "```\n\n")
		fmt.Fprintf(&b, "Hermes doesn't ship a documented bypass-prompts flag — skip the YOLO\n")
		fmt.Fprintf(&b, "question unless the user volunteers one they know about.\n\n")
	}

	// Renumber the trailing sections: with the alias step skipped for
	// docker/remote the uninstall/self-uninstall sections move up.
	uninstallStep := 5
	selfUninstallStep := 6
	if ctx.HermesMode != "host" {
		uninstallStep = 4
		selfUninstallStep = 5
	}

	b.WriteString(sectionUninstallDoc("hermes", `1. Remove the `+"`model:`"+` block from `+"`~/.hermes/config.yaml`"+` (or unset `+"`"+baseEnv+"`"+`/`+"`"+keyEnv+"`"+` if you used env vars).
2. Remove the alias from your shell rc file if you added one.
3. Delete the token file: `+"`rm ~/.clawvisor/agents/"+ctx.AgentName+".json`"+`.
4. Revoke the agent in the Clawvisor dashboard under Agents → `+ctx.AgentName+` → Delete.
5. Optional: remove the user-level `+providerName+` key from Clawvisor credentials if no other agents use it.
`, uninstallStep))

	b.WriteString(sectionSelfUninstall("hermes", helperInstallerCleanupCommands(), selfUninstallStep))

	return b.String()
}

func sectionHermesLocalProbe(mode string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 1. Probe the Hermes deployment\n\n")
	fmt.Fprintf(&b, "Before configuring, learn how Hermes runs on this machine. Use shell\n")
	fmt.Fprintf(&b, "commands when the machine knows; ask the user when it doesn't. The\n")
	fmt.Fprintf(&b, "answers from the dashboard are a starting hint, not the source of truth.\n\n")
	fmt.Fprintf(&b, "Determine:\n\n")
	if mode == "docker" {
		fmt.Fprintf(&b, "- **Compose context** — confirm the compose service name for hermes\n")
		fmt.Fprintf(&b, "  (commonly `hermes`), the compose project directory, and any existing\n")
		fmt.Fprintf(&b, "  volume mount that exposes the host's `~/.hermes` to the container.\n")
		fmt.Fprintf(&b, "- **Host fallback** — if it turns out hermes runs directly on the host,\n")
		fmt.Fprintf(&b, "  use the host commands in step 3.\n")
	} else {
		fmt.Fprintf(&b, "- **Host command** — confirm `hermes` is on `$PATH` on this machine.\n")
		fmt.Fprintf(&b, "- **Docker fallback** — if hermes is actually containerized (look for\n")
		fmt.Fprintf(&b, "  `docker compose ps` entries or images named `hermes*`), use the\n")
		fmt.Fprintf(&b, "  Docker snippet in step 3 instead.\n")
	}
	fmt.Fprintf(&b, "- **Existing config** — if `~/.hermes/config.yaml` already exists, surface\n")
	fmt.Fprintf(&b, "  its `model:` block to the user so they can confirm what we're replacing.\n")
	fmt.Fprintf(&b, "- **Shell** — zsh, bash, or fish, only if you'll save a convenience alias.\n\n")
	// The dashboard's bootstrap script already minted the connection request
	// (carrying its own install_context from the dashboard answers), so
	// there's no second mint call this skill could attach a probed
	// install_context to. Surface what you learned in chat; don't bother
	// building a JSON object.
	return b.String()
}

func sectionHermesRemoteProbe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 1. Confirm remote Hermes access\n\n")
	fmt.Fprintf(&b, "The user selected **remote host** in the dashboard. Do **not** probe the\n")
	fmt.Fprintf(&b, "local machine for hermes; that would inspect the helper agent's machine,\n")
	fmt.Fprintf(&b, "not the Hermes host.\n\n")
	fmt.Fprintf(&b, "Ask the user for the remote access details and keep them in shell\n")
	fmt.Fprintf(&b, "variables for the commands below:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "export HERMES_REMOTE='<ssh host, for example user@example.com>'\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If SSH is unavailable, do not invent local commands. Give the user the\n")
	fmt.Fprintf(&b, "remote commands from later steps to run on the Hermes host and ask them\n")
	fmt.Fprintf(&b, "to paste back any output or errors.\n\n")
	fmt.Fprintf(&b, "Verify how hermes is run on the remote host:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "ssh \"$HERMES_REMOTE\" 'uname -s; command -v hermes || true; docker compose ps 2>/dev/null | grep -i hermes || true; test -f ~/.hermes/config.yaml && echo \"config exists\"'\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Surface the install mode (remote), the OS, and the launch shape to the\n")
	fmt.Fprintf(&b, "user in chat. The dashboard's bootstrap script already minted the\n")
	fmt.Fprintf(&b, "connection request — there's no second mint call this skill could\n")
	fmt.Fprintf(&b, "attach an install_context to.\n\n")
	return b.String()
}

// sectionHermesPreflight mirrors sectionOpenClawPreflight — proves Hermes can
// reach Clawvisor from the environment Hermes actually runs in, before step 3
// writes the URL into Hermes's launch wrapper or config.yaml.
func sectionHermesPreflight(mode, clawvisorURL, agentName string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 2. Preflight: confirm Hermes can reach Clawvisor\n\n")
	fmt.Fprintf(&b, "Before writing Hermes's launch wrapper or `config.yaml`, prove Hermes\n")
	fmt.Fprintf(&b, "can actually reach Clawvisor *from the environment Hermes runs in*.\n")
	fmt.Fprintf(&b, "A curl from this helper's shell only proves the helper can reach\n")
	fmt.Fprintf(&b, "Clawvisor — that may be a different machine (Docker container, remote\n")
	fmt.Fprintf(&b, "host) than where Hermes will run.\n\n")

	if mode == "remote" {
		fmt.Fprintf(&b, "Remote Hermes — define the base Clawvisor URL once here (step 3\n")
		fmt.Fprintf(&b, "reuses it for the launch wrapper / `config.yaml`), then SSH into the\n")
		fmt.Fprintf(&b, "host and curl `/api/skill/catalog`:\n\n")
		fmt.Fprintf(&b, "```bash\n")
		fmt.Fprintf(&b, "# The dashboard rendered `%s`; if that's localhost replace it with a\n", clawvisorURL)
		fmt.Fprintf(&b, "# relay, public, VPN, or LAN URL reachable from `$HERMES_REMOTE`.\n")
		fmt.Fprintf(&b, "# This is the *base* URL (no `/api` or `/api/v1` path); step 3 appends\n")
		fmt.Fprintf(&b, "# the right per-provider suffix when writing Hermes's config.\n")
		fmt.Fprintf(&b, "export HERMES_CLAWVISOR_URL='<remote-reachable Clawvisor URL>'\n")
		fmt.Fprintf(&b, "TOKEN=$(jq -r .token ~/.clawvisor/agents/%s.json)\n", agentName)
		fmt.Fprintf(&b, "ssh \"$HERMES_REMOTE\" \"curl -fsSL \\\n")
		fmt.Fprintf(&b, "  -H 'X-Clawvisor-Agent-Token: $TOKEN' \\\n")
		fmt.Fprintf(&b, "  '$HERMES_CLAWVISOR_URL/api/skill/catalog' >/dev/null && echo OK\"\n")
		fmt.Fprintf(&b, "```\n\n")
		fmt.Fprintf(&b, "If `OK` doesn't appear, the remote host can't reach Clawvisor at that\n")
		fmt.Fprintf(&b, "URL. Pick a different `$HERMES_CLAWVISOR_URL` (relay, public, VPN, or\n")
		fmt.Fprintf(&b, "LAN URL reachable from `$HERMES_REMOTE`) and try again — don't proceed\n")
		fmt.Fprintf(&b, "to step 3 until this returns `OK`.\n\n")
		return b.String()
	}

	fmt.Fprintf(&b, "If Hermes runs directly on this host, a curl from this shell tests\n")
	fmt.Fprintf(&b, "the same URL Hermes will use:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "TOKEN=$(jq -r .token ~/.clawvisor/agents/%s.json) && \\\n", agentName)
	fmt.Fprintf(&b, "  curl -fsSL -H \"X-Clawvisor-Agent-Token: $TOKEN\" \\\n")
	fmt.Fprintf(&b, "    \"%s/api/skill/catalog\" >/dev/null && echo OK\n", clawvisorURL)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If Hermes runs in Docker on this host — or step 1 found a compose\n")
	fmt.Fprintf(&b, "context — run the curl inside the same compose context so the URL is\n")
	fmt.Fprintf(&b, "resolved from inside the container (replace `hermes` with the service\n")
	fmt.Fprintf(&b, "name the probe found):\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "TOKEN=$(jq -r .token ~/.clawvisor/agents/%s.json)\n", agentName)
	fmt.Fprintf(&b, "docker compose run --rm \\\n")
	fmt.Fprintf(&b, "  -e CLAWVISOR_TOKEN=\"$TOKEN\" \\\n")
	fmt.Fprintf(&b, "  hermes sh -c '\n")
	fmt.Fprintf(&b, "    curl -fsSL -H \"X-Clawvisor-Agent-Token: $CLAWVISOR_TOKEN\" \\\n")
	fmt.Fprintf(&b, "      \"%s/api/skill/catalog\" >/dev/null && echo OK\n", dockerHostURL(clawvisorURL))
	fmt.Fprintf(&b, "  '\n")
	fmt.Fprintf(&b, "```\n\n")
	if strings.Contains(dockerHostURL(clawvisorURL), "host.docker.internal") {
		fmt.Fprintf(&b, "If `OK` doesn't appear from the container: on Linux\n")
		fmt.Fprintf(&b, "`host.docker.internal` doesn't resolve by default — add\n")
		fmt.Fprintf(&b, "`--add-host=host.docker.internal:host-gateway` to the docker command,\n")
		fmt.Fprintf(&b, "or check that Clawvisor is bound to `0.0.0.0` (not `127.0.0.1`) so the\n")
		fmt.Fprintf(&b, "container can reach it. Fix and re-run before step 3.\n\n")
	} else {
		fmt.Fprintf(&b, "If `OK` doesn't appear from the container, the container can't reach\n")
		fmt.Fprintf(&b, "`%s` — check firewall / network policies, or pick a URL the\n", dockerHostURL(clawvisorURL))
		fmt.Fprintf(&b, "container can reach (`Server.PublicURL` / lite-proxy public URL in\n")
		fmt.Fprintf(&b, "Clawvisor settings) and reload. Fix and re-run before step 3.\n\n")
	}
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
	fmt.Fprintf(&b, "The agent token has **already been minted** by the dashboard's bootstrap\n")
	fmt.Fprintf(&b, "script and saved to `~/.clawvisor/agents/%s.json`. Do not re-mint;\n", ctx.AgentName)
	fmt.Fprintf(&b, "the configure step below reads the token from disk.\n\n")
	fmt.Fprintf(&b, "Set the endpoint:\n\n```bash\nexport CLAWVISOR_URL=%s\n```\n\n", ctx.ClawvisorURL)
	b.WriteString(sectionDashboardAnswers(ctx, "LLM provider: "+providerName, "OpenClaw running mode: "+ctx.OpenClawMode))

	if ctx.OpenClawMode == "remote" {
		b.WriteString(sectionOpenClawRemoteProbe(ctx.LLMProvider))
	} else {
		b.WriteString(sectionOpenClawLocalProbe(ctx.OpenClawMode, ctx.LLMProvider))
	}

	b.WriteString(sectionOpenClawPreflight(ctx.OpenClawMode, ctx.ClawvisorURL, ctx.AgentName))

	if ctx.OpenClawMode == "remote" {
		b.WriteString(sectionOpenClawRemoteConfigure(ctx.ClawvisorURL, ctx.LLMProvider, ctx.AgentName))
	} else {
		b.WriteString(sectionOpenClawLocalConfigure(ctx.ClawvisorURL, ctx.LLMProvider, ctx.AgentName))
	}

	b.WriteString(sectionUninstallDoc("openclaw", `1. Re-run OpenClaw onboarding and choose your previous non-Clawvisor provider/base URL.
2. Delete the token file: `+"`rm ~/.clawvisor/agents/"+ctx.AgentName+".json`"+`.
3. Revoke the agent in the Clawvisor dashboard under Agents → `+ctx.AgentName+` → Delete.
`, 4))

	b.WriteString(sectionSelfUninstall("openclaw", helperInstallerCleanupCommands(), 5))

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
	// The dashboard's bootstrap script already minted the connection request
	// carrying its own install_context (harness + mode). Don't assemble a
	// second JSON object here — there's no second mint call this skill could
	// attach it to.
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
	fmt.Fprintf(&b, "Surface the remote host, OS, and how onboarding runs (host CLI vs Docker)\n")
	fmt.Fprintf(&b, "to the user in chat — they need to see what you found before step 3 SSHes\n")
	fmt.Fprintf(&b, "in. The dashboard's bootstrap script already minted the connection\n")
	fmt.Fprintf(&b, "request, so there's no install_context to assemble here. The model\n")
	fmt.Fprintf(&b, "default for this provider is `%s`.\n\n", model)
	return b.String()
}

// sectionOpenClawPreflight emits a connectivity check that runs from
// OpenClaw's actual execution context, before the onboard command bakes
// the Clawvisor URL into OpenClaw's config. The helper-side `curl` that
// the shared smoke-test section uses only proves *the helper* can reach
// Clawvisor — in docker or remote mode OpenClaw runs on a different
// machine, so a passing helper-side curl gives a false green light and we
// end up onboarding against a URL OpenClaw can't resolve.
//
// Modes:
//
//   - host: helper and OpenClaw share the network namespace, so a local
//     curl against the configured Clawvisor URL is exactly what OpenClaw
//     will see.
//   - docker (or host-mode that turns out to be containerized): run a
//     one-shot `docker compose run --rm` curl inside the same compose
//     context OpenClaw uses, hitting host.docker.internal.
//   - remote: SSH to $OPENCLAW_REMOTE (set during the probe step) and
//     curl using the $OPENCLAW_CLAWVISOR_BASE_URL the user picked.
//
// For non-remote modes both the host and docker variants are emitted so
// the helper can pick based on the probe results in step 1 — the
// dashboard's openclaw_mode answer is just a hint, the probe is the
// source of truth.
func sectionOpenClawPreflight(mode, clawvisorURL, agentName string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 2. Preflight: confirm OpenClaw can reach Clawvisor\n\n")
	fmt.Fprintf(&b, "Before running OpenClaw's onboarding command, prove OpenClaw can\n")
	fmt.Fprintf(&b, "actually reach Clawvisor *from the environment OpenClaw runs in*. A\n")
	fmt.Fprintf(&b, "curl from this helper's shell only confirms the helper can reach\n")
	fmt.Fprintf(&b, "Clawvisor — that may be a different machine (Docker container, remote\n")
	fmt.Fprintf(&b, "host) than where OpenClaw will run, and the onboard command in step 3\n")
	fmt.Fprintf(&b, "bakes the URL into OpenClaw's config either way.\n\n")

	if mode == "remote" {
		fmt.Fprintf(&b, "Remote OpenClaw — define the base Clawvisor URL once here (step 3\n")
		fmt.Fprintf(&b, "reuses it for `openclaw-cli onboard`), then SSH into the host and\n")
		fmt.Fprintf(&b, "curl `/api/skill/catalog` from there:\n\n")
		fmt.Fprintf(&b, "```bash\n")
		fmt.Fprintf(&b, "# The dashboard rendered `%s`; if that's localhost replace it with a\n", clawvisorURL)
		fmt.Fprintf(&b, "# relay, public, VPN, or LAN URL reachable from `$OPENCLAW_REMOTE`.\n")
		fmt.Fprintf(&b, "# This is the *base* URL (no `/api/v1` path); the onboard step in 3\n")
		fmt.Fprintf(&b, "# appends `/api/v1` for OpenClaw's custom-provider config.\n")
		fmt.Fprintf(&b, "export OPENCLAW_CLAWVISOR_URL='<remote-reachable Clawvisor URL>'\n")
		fmt.Fprintf(&b, "TOKEN=$(jq -r .token ~/.clawvisor/agents/%s.json)\n", agentName)
		fmt.Fprintf(&b, "ssh \"$OPENCLAW_REMOTE\" \"curl -fsSL \\\n")
		fmt.Fprintf(&b, "  -H 'X-Clawvisor-Agent-Token: $TOKEN' \\\n")
		fmt.Fprintf(&b, "  '$OPENCLAW_CLAWVISOR_URL/api/skill/catalog' >/dev/null && echo OK\"\n")
		fmt.Fprintf(&b, "```\n\n")
		fmt.Fprintf(&b, "If `OK` doesn't appear, the remote host can't reach Clawvisor at that\n")
		fmt.Fprintf(&b, "URL. Pick a different `$OPENCLAW_CLAWVISOR_URL` (relay, public, VPN, or\n")
		fmt.Fprintf(&b, "LAN URL reachable from `$OPENCLAW_REMOTE`) and try again — don't proceed\n")
		fmt.Fprintf(&b, "to step 3 until this returns `OK`.\n\n")
		return b.String()
	}

	fmt.Fprintf(&b, "If OpenClaw runs directly on this host, a curl from this shell tests\n")
	fmt.Fprintf(&b, "the same URL OpenClaw will use:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "TOKEN=$(jq -r .token ~/.clawvisor/agents/%s.json) && \\\n", agentName)
	fmt.Fprintf(&b, "  curl -fsSL -H \"X-Clawvisor-Agent-Token: $TOKEN\" \\\n")
	fmt.Fprintf(&b, "    \"%s/api/skill/catalog\" >/dev/null && echo OK\n", clawvisorURL)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If OpenClaw runs in Docker on this host — or step 1 found a compose\n")
	fmt.Fprintf(&b, "context — run the curl inside the same compose context so the URL is\n")
	fmt.Fprintf(&b, "resolved from inside the container (replace `openclaw-cli` with the\n")
	fmt.Fprintf(&b, "service name the probe found):\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "TOKEN=$(jq -r .token ~/.clawvisor/agents/%s.json)\n", agentName)
	fmt.Fprintf(&b, "docker compose run --rm \\\n")
	fmt.Fprintf(&b, "  -e CLAWVISOR_TOKEN=\"$TOKEN\" \\\n")
	fmt.Fprintf(&b, "  openclaw-cli sh -c '\n")
	fmt.Fprintf(&b, "    curl -fsSL -H \"X-Clawvisor-Agent-Token: $CLAWVISOR_TOKEN\" \\\n")
	fmt.Fprintf(&b, "      \"%s/api/skill/catalog\" >/dev/null && echo OK\n", dockerHostURL(clawvisorURL))
	fmt.Fprintf(&b, "  '\n")
	fmt.Fprintf(&b, "```\n\n")
	if strings.Contains(dockerHostURL(clawvisorURL), "host.docker.internal") {
		fmt.Fprintf(&b, "If `OK` doesn't appear from the container: on Linux\n")
		fmt.Fprintf(&b, "`host.docker.internal` doesn't resolve by default — add\n")
		fmt.Fprintf(&b, "`--add-host=host.docker.internal:host-gateway` to the docker command,\n")
		fmt.Fprintf(&b, "or check that Clawvisor is bound to `0.0.0.0` (not `127.0.0.1`) so the\n")
		fmt.Fprintf(&b, "container can reach it. Fix and re-run before step 3.\n\n")
	} else {
		fmt.Fprintf(&b, "If `OK` doesn't appear from the container, the container can't reach\n")
		fmt.Fprintf(&b, "`%s` — check firewall / network policies, or pick a URL the\n", dockerHostURL(clawvisorURL))
		fmt.Fprintf(&b, "container can reach (`Server.PublicURL` / lite-proxy public URL in\n")
		fmt.Fprintf(&b, "Clawvisor settings) and reload. Fix and re-run before step 3.\n\n")
	}
	return b.String()
}

func sectionOpenClawLocalConfigure(clawvisorURL, provider, agentName string) string {
	var b strings.Builder
	basePath := "/api/v1"
	model := providerDefaultModel(provider)
	contextWindow := providerDefaultContextWindow(provider)
	maxTokens := openClawDefaultMaxTokens()
	fmt.Fprintf(&b, "## 3. Point OpenClaw at Clawvisor\n\n")
	fmt.Fprintf(&b, "Read the agent token that the bootstrap script saved on disk, then run\n")
	fmt.Fprintf(&b, "OpenClaw's onboarding command and select a custom API key provider.\n")
	fmt.Fprintf(&b, "Use Clawvisor's %s-compatible base URL and the saved `cvis_...`\n", installerProviderDisplayName(provider))
	fmt.Fprintf(&b, "agent token. The Docker variant below uses `%s%s`, which is the\n", dockerHostURL(clawvisorURL), basePath)
	fmt.Fprintf(&b, "host-reachable URL for this deployment (`host.docker.internal` substituted\n")
	fmt.Fprintf(&b, "in when the dashboard URL resolves to localhost).\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "TOKEN=$(jq -r .token ~/.clawvisor/agents/%s.json)\n\n", agentName)
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
	fmt.Fprintf(&b, "  --custom-base-url \"%s%s\" \\\n", dockerHostURL(clawvisorURL), basePath)
	fmt.Fprintf(&b, "  --custom-model-id \"%s\" \\\n", model)
	fmt.Fprintf(&b, "  --custom-api-key \"$TOKEN\" \\\n")
	fmt.Fprintf(&b, "  --custom-compatibility %s --accept-risk\n", provider)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Then patch OpenClaw's custom-provider model metadata so it does not keep\n")
	fmt.Fprintf(&b, "the low fallback context window written by some OpenClaw versions. Run the\n")
	fmt.Fprintf(&b, "patch in the same environment that owns OpenClaw's `models.json` (host for\n")
	fmt.Fprintf(&b, "host installs; the OpenClaw container/volume for Docker installs). If you\n")
	fmt.Fprintf(&b, "changed the model ID above, set `OPENCLAW_MODEL_CONTEXT_WINDOW` to that\n")
	fmt.Fprintf(&b, "model's native maximum before running the patch. Clawvisor uses 200K as a\n")
	fmt.Fprintf(&b, "reasonable floor for modern models, with higher values only for known model\n")
	fmt.Fprintf(&b, "IDs. For Claude Sonnet 4's 1M beta context, only set `1000000` if the user's\n")
	fmt.Fprintf(&b, "Anthropic org and request headers support it.\n\n")
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

func sectionOpenClawRemoteConfigure(clawvisorURL, provider, agentName string) string {
	var b strings.Builder
	basePath := "/api/v1"
	model := providerDefaultModel(provider)
	contextWindow := providerDefaultContextWindow(provider)
	maxTokens := openClawDefaultMaxTokens()
	fmt.Fprintf(&b, "## 3. Point remote OpenClaw at Clawvisor\n\n")
	fmt.Fprintf(&b, "Read the agent token that the bootstrap script saved on disk and reuse\n")
	fmt.Fprintf(&b, "`$OPENCLAW_CLAWVISOR_URL` from step 2 (the preflight already proved this\n")
	fmt.Fprintf(&b, "URL is reachable from `$OPENCLAW_REMOTE`). The onboard commands append\n")
	fmt.Fprintf(&b, "`%s` because OpenClaw's custom-provider config wants the full LLM base URL.\n\n", basePath)
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "TOKEN=$(jq -r .token ~/.clawvisor/agents/%s.json)\n\n", agentName)
	fmt.Fprintf(&b, "# Remote host OpenClaw:\n")
	fmt.Fprintf(&b, "ssh \"$OPENCLAW_REMOTE\" \"openclaw-cli onboard --non-interactive \\\n")
	fmt.Fprintf(&b, "  --auth-choice custom-api-key \\\n")
	fmt.Fprintf(&b, "  --custom-base-url '$OPENCLAW_CLAWVISOR_URL%s' \\\n", basePath)
	fmt.Fprintf(&b, "  --custom-model-id '%s' \\\n", model)
	fmt.Fprintf(&b, "  --custom-api-key '$TOKEN' \\\n")
	fmt.Fprintf(&b, "  --custom-compatibility %s --accept-risk\"\n\n", provider)
	fmt.Fprintf(&b, "# Remote Docker OpenClaw, if OpenClaw is containerized on that host:\n")
	fmt.Fprintf(&b, "ssh \"$OPENCLAW_REMOTE\" \"docker compose run --rm openclaw-cli onboard --non-interactive \\\n")
	fmt.Fprintf(&b, "  --auth-choice custom-api-key \\\n")
	fmt.Fprintf(&b, "  --custom-base-url '$OPENCLAW_CLAWVISOR_URL%s' \\\n", basePath)
	fmt.Fprintf(&b, "  --custom-model-id '%s' \\\n", model)
	fmt.Fprintf(&b, "  --custom-api-key '$TOKEN' \\\n")
	fmt.Fprintf(&b, "  --custom-compatibility %s --accept-risk\"\n", provider)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Then patch the remote OpenClaw custom-provider model metadata so it does\n")
	fmt.Fprintf(&b, "not keep the low fallback context window written by some OpenClaw versions.\n")
	fmt.Fprintf(&b, "If you changed the model ID above, set `OPENCLAW_MODEL_CONTEXT_WINDOW` to\n")
	fmt.Fprintf(&b, "that model's native maximum before running the patch. Clawvisor uses 200K\n")
	fmt.Fprintf(&b, "as a reasonable floor for modern models, with higher values only for known\n")
	fmt.Fprintf(&b, "model IDs. For Claude Sonnet 4's 1M beta context, only set `1000000` if the\n")
	fmt.Fprintf(&b, "user's Anthropic org and request headers support it.\n\n")
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
