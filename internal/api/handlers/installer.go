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

// validClaimCode guards the `claim` query param. Claim codes are URL-safe
// base64 (rand.Read → base64.RawURLEncoding, truncated to 10 chars) — see
// MintClaim in connections.go. The interpolation site renders the claim
// straight into a shell URL inside the install skill, so any character
// outside `[A-Za-z0-9_-]` could break out of the surrounding shell quote
// and inject arbitrary commands into the user's terminal. Length-cap alone
// is not enough; the charset has to be locked down too.
var validClaimCode = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

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
	// AppURL is the control-plane / dashboard endpoint: where agent
	// registration (/api/agents/connect), credential storage
	// (/api/runtime/llm-credentials), the skill catalog, and the dashboard
	// itself live. Resolves to cfg.Server.PublicURL, falling back to the
	// request host. Distinct from LLMURL because in split deployments these
	// two surfaces live on different hosts (e.g. app.clawvisor.com vs
	// llm.clawvisor.com), and registering against the LLM host 404s.
	AppURL string
	// LLMURL is the data-plane / LLM-proxy endpoint: what gets baked into
	// ANTHROPIC_BASE_URL, OpenAI base_url, etc. Resolves to
	// cfg.ProxyLite.PublicURL when configured, otherwise falls back to
	// AppURL (single-host deployments).
	LLMURL          string
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

	appURL := h.resolveAppURL(r)
	ctx := installerCtx{
		AppURL:  appURL,
		LLMURL:  h.resolveLLMURL(appURL),
		IsLocal: h.isLocal,
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
	// `claim` is interpolated directly into the shell-quoted curl URL inside
	// the rendered skill, so charset matters — not just length. Reject any
	// value that isn't pure URL-safe base64. A `"` in the claim would close
	// the shell string and let the rest run as arbitrary commands when the
	// user pastes the skill into a terminal.
	if claim := r.URL.Query().Get("claim"); claim != "" && validClaimCode.MatchString(claim) {
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

	appURL := h.resolveAppURL(r)
	ctx := installerCtx{
		AppURL:  appURL,
		LLMURL:  h.resolveLLMURL(appURL),
		IsLocal: h.isLocal,
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

// resolveAppURL returns the control-plane / dashboard URL — where agent
// registration, credentials, the skill catalog, and dashboard pages live.
// Precedence:
//
//  1. cfg.Server.PublicURL, when configured.
//  2. The actual request / relay / local server URL.
//
// Notably NOT cfg.ProxyLite.PublicURL — the LLM proxy host typically does
// not serve the control-plane endpoints. Conflating them is what caused the
// install script to POST /api/agents/connect at the proxy host and 404.
func (h *InstallerHandler) resolveAppURL(r *http.Request) string {
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

// resolveLLMURL returns the data-plane / LLM-proxy URL — what gets baked
// into ANTHROPIC_BASE_URL / OpenAI base_url. Prefers cfg.ProxyLite.PublicURL
// when set; falls back to the app URL for single-host deployments where the
// proxy lives on the same origin.
func (h *InstallerHandler) resolveLLMURL(appURL string) string {
	if h.llmProxyURL != "" {
		return h.llmProxyURL
	}
	return appURL
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

// helperSetupCleanupCommands removes the one-paste setup skill the dashboard
// dropped on the helper's disk. Mirrors helperInstallerCleanupCommands but
// targets the `clawvisor-setup` path the new one-paste flow writes to (see
// ONE_PASTE_SPECS in the dashboard). The setup skill is one-shot
// scaffolding — once setup completes it removes itself; the user can
// re-trigger via the dashboard if they want another install.
func helperSetupCleanupCommands() string {
	return `rm -f ~/.claude/commands/clawvisor-setup.md
rm -rf ~/.codex/skills/clawvisor-setup`
}

// providerCaseBlock is the shell `case "$PROVIDER"` block emitted at the
// end of sectionDetectProvider. Once $PROVIDER is set to "anthropic" or
// "openai", it derives every other per-provider value (label, URL path,
// env-var names, key prefix, default model id, native context window) as
// shell variables that the rest of the skill consumes. Centralizing this
// here means later steps reference $BASE_PATH / $KEY_ENV / $MODEL_ID
// uniformly — they don't care which provider was picked.
//
// CONTEXT_WINDOW is set per provider's native maximum here: Claude
// Sonnet 4's 1M beta only kicks in for Anthropic orgs that have it
// enabled, so we surface 200K as the conservative floor and tell the
// helper to override only when the user explicitly opts in.
// `OPENCLAW_API` is the on-disk value the OpenClaw provider registry uses
// (verified against docs.openclaw.ai/concepts/model-providers). It's
// distinct from the `--custom-compatibility` flag value used by
// `openclaw onboard`; we configure via `openclaw config set --strict-json
// --merge` and write the on-disk value directly.
const providerCaseBlock = `case "$PROVIDER" in
  anthropic)
    PROVIDER_LABEL='Anthropic'
    BASE_PATH='/api'
    BASE_ENV='ANTHROPIC_BASE_URL'
    KEY_ENV='ANTHROPIC_API_KEY'
    KEY_VALUE="$ANTHROPIC_API_KEY"
    KEY_PREFIX='sk-ant-'
    MODEL_ID='claude-sonnet-4-6'
    CONTEXT_WINDOW=200000
    OPENCLAW_API='anthropic-messages'
    ;;
  openai)
    PROVIDER_LABEL='OpenAI'
    BASE_PATH='/api/v1'
    BASE_ENV='OPENAI_BASE_URL'
    KEY_ENV='OPENAI_API_KEY'
    KEY_VALUE="$OPENAI_API_KEY"
    KEY_PREFIX='sk-'
    MODEL_ID='gpt-5.4'
    CONTEXT_WINDOW=1000000
    OPENCLAW_API='openai-completions'
    ;;
  *)
    echo "unsupported provider: $PROVIDER" >&2; exit 1
    ;;
esac
KEY_PREFIX_LEN=${#KEY_PREFIX}
`

// sectionDetectProviderHermes emits the provider-detection step for the
// Hermes installer. It probes env vars (presence only, never values) and
// reads only the model.base_url field of ~/.hermes/config.yaml via python
// yaml — deliberately avoiding cat/grep/head/tail of the file because the
// adjacent api_key field carries the secret. Once a provider is picked
// (single hit → confirm; multiple → ask; none → ask), $PROVIDER is set
// and the shared providerCaseBlock derives every other variable.
func sectionDetectProviderHermes(step int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %d. Detect the upstream LLM provider\n\n", step)
	fmt.Fprintf(&b, "Clawvisor's proxy-lite swaps in a vaulted upstream key on every call.\n")
	fmt.Fprintf(&b, "Figure out which provider — Anthropic or OpenAI — by looking at how\n")
	fmt.Fprintf(&b, "Hermes is configured today.\n\n")
	fmt.Fprintf(&b, "**Read only safe-to-print metadata.** `~/.hermes/config.yaml` carries\n")
	fmt.Fprintf(&b, "an `api_key` field — DO NOT `cat`, `grep`, `head`, or `tail` it. The\n")
	fmt.Fprintf(&b, "probe below uses `python3 -c` with `re` to read only\n")
	fmt.Fprintf(&b, "`model.base_url`, which doesn't carry the secret.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "DETECTED=''\n")
	fmt.Fprintf(&b, "# Env vars — presence only, never value\n")
	fmt.Fprintf(&b, "[ -n \"$ANTHROPIC_API_KEY\" ] && DETECTED=\"$DETECTED anthropic\"\n")
	fmt.Fprintf(&b, "[ -n \"$OPENAI_API_KEY\" ]   && DETECTED=\"$DETECTED openai\"\n")
	fmt.Fprintf(&b, "if [ -f ~/.hermes/config.yaml ]; then\n")
	fmt.Fprintf(&b, "  # base_url path / host — `*/api/v1*` and `*/api` catch re-install\n")
	fmt.Fprintf(&b, "  # cases where base_url points at a Clawvisor instance and the\n")
	fmt.Fprintf(&b, "  # trailing path tells us which provider was picked last time.\n")
	fmt.Fprintf(&b, "  BASE=$(%s)\n", hermesYamlParserPython("base_url"))
	fmt.Fprintf(&b, "  case \"$BASE\" in\n")
	fmt.Fprintf(&b, "    *anthropic.com*)      DETECTED=\"$DETECTED anthropic\" ;;\n")
	fmt.Fprintf(&b, "    *openai.com*)         DETECTED=\"$DETECTED openai\" ;;\n")
	fmt.Fprintf(&b, "    */api/v1*)            DETECTED=\"$DETECTED openai\" ;;\n")
	fmt.Fprintf(&b, "    */api|*/api/)         DETECTED=\"$DETECTED anthropic\" ;;\n")
	fmt.Fprintf(&b, "  esac\n")
	fmt.Fprintf(&b, "  # model.default name pattern — strongest hint of what's *actively*\n")
	fmt.Fprintf(&b, "  # used, since base_url alone doesn't say which model is selected.\n")
	fmt.Fprintf(&b, "  DEFAULT=$(%s)\n", hermesYamlParserPython("default"))
	fmt.Fprintf(&b, "  case \"$DEFAULT\" in\n")
	fmt.Fprintf(&b, "    anthropic/*|*claude*)            DETECTED=\"$DETECTED anthropic\" ;;\n")
	fmt.Fprintf(&b, "    openai/*|*gpt*|*o1-*|*o3-*|*o4-*) DETECTED=\"$DETECTED openai\" ;;\n")
	fmt.Fprintf(&b, "  esac\n")
	fmt.Fprintf(&b, "fi\n")
	fmt.Fprintf(&b, "UNIQ=$(printf '%%s\\n' $DETECTED | sort -u | tr '\\n' ' ' | sed 's/ $//')\n")
	fmt.Fprintf(&b, "echo \"detected: ${UNIQ:-none}\"\n")
	fmt.Fprintf(&b, "```\n\n")
	b.WriteString(sectionDetectProviderAskAndCase("Hermes"))
	return b.String()
}

func hermesYamlParserPython(field string) string {
	return `python3 - <<'EOF' 2>/dev/null || true
import os, re
fn = os.path.expanduser('~/.hermes/config.yaml')
if os.path.exists(fn):
    d = {}
    st, ind = [d], [-1]
    for l in open(fn):
        l = re.sub(r'#.*', '', l)
        if ':' not in l: continue
        indent = len(l) - len(l.lstrip())
        k, v = l.strip().split(':', 1)
        k, v = k.strip(), v.strip().strip('\'"')
        while ind and indent <= ind[-1]: st.pop(); ind.pop()
        if not v:
            nm = {}
            st[-1][k] = nm
            st.append(nm)
            ind.append(indent)
        else: st[-1][k] = v
    print(d.get('model', {}).get('` + field + `', ''))
EOF`
}

// sectionDetectProviderOpenClaw emits the provider-detection step for the
// OpenClaw installer. It probes env vars and reads provider keys from
// ~/.openclaw/openclaw.json — which is OpenClaw's single global config
// file (per docs.openclaw.ai/concepts/model-providers, all agents inherit
// from `models.providers` here; no per-agent models.json file exists).
// jq is safe because it extracts only the provider id keys, never the
// nested apiKey values.
func sectionDetectProviderOpenClaw(step int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %d. Detect the upstream LLM provider\n\n", step)
	fmt.Fprintf(&b, "Clawvisor's proxy-lite swaps in a vaulted upstream key on every call.\n")
	fmt.Fprintf(&b, "Figure out which provider — Anthropic or OpenAI — by looking at how\n")
	fmt.Fprintf(&b, "OpenClaw is configured today.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "DETECTED=''\n")
	fmt.Fprintf(&b, "# Env vars — presence only, never value\n")
	fmt.Fprintf(&b, "[ -n \"$ANTHROPIC_API_KEY\" ] && DETECTED=\"$DETECTED anthropic\"\n")
	fmt.Fprintf(&b, "[ -n \"$OPENAI_API_KEY\" ]   && DETECTED=\"$DETECTED openai\"\n")
	fmt.Fprintf(&b, "# Existing OpenClaw provider registry — global, under models.providers\n")
	fmt.Fprintf(&b, "if [ -f ~/.openclaw/openclaw.json ]; then\n")
	fmt.Fprintf(&b, "  # Provider id key patterns — easy hit when the user named the entry\n")
	fmt.Fprintf(&b, "  # `anthropic` / `claude-3-5-sonnet` / etc.\n")
	fmt.Fprintf(&b, "  for p in $(jq -r '.models.providers // {} | keys[]?' ~/.openclaw/openclaw.json 2>/dev/null); do\n")
	fmt.Fprintf(&b, "    case \"$p\" in\n")
	fmt.Fprintf(&b, "      anthropic*|claude*) DETECTED=\"$DETECTED anthropic\" ;;\n")
	fmt.Fprintf(&b, "      openai*|gpt*)       DETECTED=\"$DETECTED openai\" ;;\n")
	fmt.Fprintf(&b, "    esac\n")
	fmt.Fprintf(&b, "  done\n")
	fmt.Fprintf(&b, "  # Strongest signal: scan EVERY provider's `api` field. This is the\n")
	fmt.Fprintf(&b, "  # wire-protocol the user picked, regardless of how they named the\n")
	fmt.Fprintf(&b, "  # provider key (e.g. `custom-host-docker-internal-25297`, `local-llm`,\n")
	fmt.Fprintf(&b, "  # `clawvisor`). Without this scan a Clawvisor re-install or any\n")
	fmt.Fprintf(&b, "  # non-default-named provider would silently produce \"no signal.\"\n")
	fmt.Fprintf(&b, "  for api in $(jq -r '.models.providers // {} | to_entries[]?.value.api // empty' ~/.openclaw/openclaw.json 2>/dev/null); do\n")
	fmt.Fprintf(&b, "    case \"$api\" in\n")
	fmt.Fprintf(&b, "      anthropic-messages)                  DETECTED=\"$DETECTED anthropic\" ;;\n")
	fmt.Fprintf(&b, "      openai-completions|openai-responses) DETECTED=\"$DETECTED openai\" ;;\n")
	fmt.Fprintf(&b, "    esac\n")
	fmt.Fprintf(&b, "  done\n")
	fmt.Fprintf(&b, "  # Default model's provider — what's *actively* used. Strongest hint.\n")
	fmt.Fprintf(&b, "  DEFAULT_MODEL=$(jq -r '.models.default // empty' ~/.openclaw/openclaw.json 2>/dev/null)\n")
	fmt.Fprintf(&b, "  if [ -n \"$DEFAULT_MODEL\" ]; then\n")
	fmt.Fprintf(&b, "    DEFAULT_PROVIDER=\"${DEFAULT_MODEL%%%%/*}\"\n")
	fmt.Fprintf(&b, "    DEFAULT_API=$(jq -r --arg p \"$DEFAULT_PROVIDER\" '.models.providers[$p].api // empty' ~/.openclaw/openclaw.json 2>/dev/null)\n")
	fmt.Fprintf(&b, "    case \"$DEFAULT_API\" in\n")
	fmt.Fprintf(&b, "      anthropic-messages)                  DETECTED=\"$DETECTED anthropic\" ;;\n")
	fmt.Fprintf(&b, "      openai-completions|openai-responses) DETECTED=\"$DETECTED openai\" ;;\n")
	fmt.Fprintf(&b, "    esac\n")
	fmt.Fprintf(&b, "  fi\n")
	fmt.Fprintf(&b, "fi\n")
	fmt.Fprintf(&b, "UNIQ=$(printf '%%s\\n' $DETECTED | sort -u | tr '\\n' ' ' | sed 's/ $//')\n")
	fmt.Fprintf(&b, "echo \"detected: ${UNIQ:-none}\"\n")
	fmt.Fprintf(&b, "```\n\n")
	b.WriteString(sectionDetectProviderAskAndCase("OpenClaw"))
	return b.String()
}

// sectionDetectProviderAskAndCase is the trailing portion shared by both
// Hermes and OpenClaw detect steps: surface what the probe found, ask the
// user, wait for their answer, then emit the case block that derives
// every per-provider variable.
//
// The "ask the user" framing is centered as a HARD CONSTRAINT — helpers
// reading natural-language instructions tend to interpret "ask if you
// can't tell" as "decide for them and announce it." We've seen exactly
// that failure in the field (helper picked Anthropic silently because
// the detect step returned no signal). The phrasing here forbids that
// path explicitly: the only acceptable behavior is to ask and wait.
func sectionDetectProviderAskAndCase(harness string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Ask the user which provider — DO NOT decide silently\n\n")
	fmt.Fprintf(&b, "**HARD CONSTRAINT: you must not pick `$PROVIDER` yourself. Ask the user\n")
	fmt.Fprintf(&b, "the question below and wait for their reply before continuing.** No\n")
	fmt.Fprintf(&b, "defaulting to Anthropic because Claude Code is running this skill, no\n")
	fmt.Fprintf(&b, "defaulting to OpenAI because the user mentioned GPT in passing — your\n")
	fmt.Fprintf(&b, "context window is not the source of truth here, the user is. Surface\n")
	fmt.Fprintf(&b, "what the probe found in chat (`detected: ...` line), then ask one of\n")
	fmt.Fprintf(&b, "these questions verbatim (or close), picking the form that matches\n")
	fmt.Fprintf(&b, "`$UNIQ`:\n\n")
	fmt.Fprintf(&b, "- **Single match** (`$UNIQ` is exactly `anthropic` or exactly `openai`):\n")
	fmt.Fprintf(&b, "  > %s appears set up for `<detected>`. Vault a `<detected>` key in\n", harness)
	fmt.Fprintf(&b, "  > Clawvisor so the proxy can route there — or would you rather use\n")
	fmt.Fprintf(&b, "  > the other one (`<other>`)?\n\n")
	fmt.Fprintf(&b, "- **Multiple matches** (`$UNIQ` contains both `anthropic` and `openai`):\n")
	fmt.Fprintf(&b, "  > %s has both `anthropic` and `openai` configured. Which one should\n", harness)
	fmt.Fprintf(&b, "  > I route through Clawvisor?\n\n")
	fmt.Fprintf(&b, "- **No match** (`$UNIQ` empty):\n")
	fmt.Fprintf(&b, "  > I couldn't detect a current LLM provider for %s. Clawvisor's\n", harness)
	fmt.Fprintf(&b, "  > proxy-lite supports `anthropic` (Claude) and `openai` (GPT). Which\n")
	fmt.Fprintf(&b, "  > one do you want to vault for this install?\n\n")
	fmt.Fprintf(&b, "**Wait for the user's reply before going further.** If they reply with\n")
	fmt.Fprintf(&b, "anything ambiguous (\"either is fine\", \"you pick\", silence), ask once\n")
	fmt.Fprintf(&b, "more and surface that Clawvisor needs a definite choice — don't fill\n")
	fmt.Fprintf(&b, "the silence by picking yourself.\n\n")
	fmt.Fprintf(&b, "Once you have a clear answer, set `$PROVIDER` to `anthropic` or `openai`\n")
	fmt.Fprintf(&b, "and run the case block that derives every per-provider variable later\n")
	fmt.Fprintf(&b, "steps consume:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	b.WriteString(providerCaseBlock)
	fmt.Fprintf(&b, "```\n\n")
	return b.String()
}

// sectionEnsureVaultedKeyDynamic is the shell-variable-driven equivalent
// of sectionEnsureVaultedKey for the swap-mode-only harnesses (Hermes,
// OpenClaw) where the provider isn't known until the detect step picked
// it. Uses $PROVIDER / $KEY_ENV / $KEY_VALUE / $KEY_PREFIX from the
// preceding detect-step case block, so this step is provider-agnostic at
// render time — the helper picks the path at runtime.
//
// Same HARD CONSTRAINTS as sectionVaultUpstreamKey: no reading rc files,
// no echoing the value, no argv. The detect step's python yaml read of
// ~/.hermes/config.yaml deliberately extracts only base_url; this step
// must not loosen that constraint.
func sectionEnsureVaultedKeyDynamic(step int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %d. Ensure a vaulted upstream key exists\n\n", step)
	fmt.Fprintf(&b, "Check if Clawvisor already has a `$PROVIDER` key for this user (or this\n")
	fmt.Fprintf(&b, "agent). Accept either scope — a prior install (Claude Code, Codex,\n")
	fmt.Fprintf(&b, "another agent) may have saved at either.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "AGENT_ID=$(jq -r .agent_id \"$TOKEN_FILE\")\n")
	fmt.Fprintf(&b, "EXISTING=$(curl -sS -H \"Authorization: Bearer $TOKEN\" \\\n")
	fmt.Fprintf(&b, "  \"$CLAWVISOR_APP_URL/api/runtime/llm-credentials?agent_id=$AGENT_ID\")\n")
	fmt.Fprintf(&b, "if echo \"$EXISTING\" | jq -e --arg p \"$PROVIDER\" '.credentials[] | select(.provider==$p and (.stored==true or .agent_stored==true))' >/dev/null 2>&1; then\n")
	fmt.Fprintf(&b, "  echo \"existing $PROVIDER_LABEL key found — skipping vault\"\n")
	fmt.Fprintf(&b, "  KEY_VAULTED=1\n")
	fmt.Fprintf(&b, "else\n")
	fmt.Fprintf(&b, "  KEY_VAULTED=0\n")
	fmt.Fprintf(&b, "fi\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If `KEY_VAULTED=1`, skip the sub-steps below and continue. Otherwise:\n\n")
	fmt.Fprintf(&b, "### %d.a. Vault a $PROVIDER_LABEL API key\n\n", step)
	fmt.Fprintf(&b, "**HARD CONSTRAINTS — read carefully, these are non-negotiable:**\n\n")
	fmt.Fprintf(&b, "- DO NOT `grep`, `cat`, `head`, `tail` `~/.zshrc`, `~/.bashrc`,\n")
	fmt.Fprintf(&b, "  `~/.zshenv`, `~/.profile`, `.env`, `.envrc`,\n")
	fmt.Fprintf(&b, "  `~/.config/fish/config.fish`, `~/.hermes/config.yaml`, or any file\n")
	fmt.Fprintf(&b, "  that might contain `$KEY_ENV=…` or the key value in plaintext.\n")
	fmt.Fprintf(&b, "- DO NOT `echo \"$KEY_VALUE\"`, `printenv`, or print the value any way.\n")
	fmt.Fprintf(&b, "- DO NOT use `set -x`, `bash -x`, or any trace mode.\n")
	fmt.Fprintf(&b, "- DO NOT pass the value through argv (`jq --arg`, `curl -d \"key=$VAR\"`).\n")
	fmt.Fprintf(&b, "  Argv shows up in `/proc` and process listings. Use stdin pipes only.\n")
	fmt.Fprintf(&b, "- Use ONLY the live environment of the shell you're running in right now.\n\n")
	fmt.Fprintf(&b, "Detect (prefix + length only — zero entropy revealed):\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "if [ -n \"$KEY_VALUE\" ]; then\n")
	fmt.Fprintf(&b, "  printf 'present prefix=%%s length=%%d\\n' \\\n")
	fmt.Fprintf(&b, "    \"$(printf '%%s' \"$KEY_VALUE\" | head -c \"$KEY_PREFIX_LEN\")\" \\\n")
	fmt.Fprintf(&b, "    \"${#KEY_VALUE}\"\n")
	fmt.Fprintf(&b, "else\n")
	fmt.Fprintf(&b, "  echo absent\n")
	fmt.Fprintf(&b, "fi\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**If output is `present prefix=<KEY_PREFIX> length=<N>`**, ask the user:\n\n")
	fmt.Fprintf(&b, "> I see a `$PROVIDER_LABEL` API key in your environment (prefix\n")
	fmt.Fprintf(&b, "> `$KEY_PREFIX`, `<N>` chars). Vault it in Clawvisor so this agent can\n")
	fmt.Fprintf(&b, "> route through proxy-lite? I won't read the key — it'll pipe straight\n")
	fmt.Fprintf(&b, "> from your shell into Clawvisor's vault.\n\n")
	fmt.Fprintf(&b, "If yes, vault via stdin pipe (value never enters argv):\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "printf '%%s' \"$KEY_VALUE\" | jq -Rs '{api_key:.}' | \\\n")
	fmt.Fprintf(&b, "  curl -sS -X PUT \"$CLAWVISOR_APP_URL/api/runtime/llm-credentials/$PROVIDER\" \\\n")
	fmt.Fprintf(&b, "    -H \"Authorization: Bearer $TOKEN\" \\\n")
	fmt.Fprintf(&b, "    -H \"Content-Type: application/json\" \\\n")
	fmt.Fprintf(&b, "    --data-binary @-\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Expected response: `{\"provider\":\"<provider>\",\"service_id\":\"…\",\"status\":\"stored\"}`\n")
	fmt.Fprintf(&b, "(or `\"rotated\"` / `\"unchanged\"`). No key is echoed back.\n\n")
	fmt.Fprintf(&b, "**If env var is `absent` or user declined**, fall back to the dashboard:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "echo \"$CLAWVISOR_APP_URL/dashboard/keys/$PROVIDER?for=$AGENT_ID\"\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Tell the user:\n\n")
	fmt.Fprintf(&b, "> Open the URL above to add your `$PROVIDER_LABEL` key. I'll wait — once\n")
	fmt.Fprintf(&b, "> you save it, I'll continue automatically.\n\n")
	fmt.Fprintf(&b, "Then poll (up to ~3 min); accept user-scope OR agent-scope as success:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "for i in $(seq 1 90); do\n")
	fmt.Fprintf(&b, "  RESP=$(curl -sS -H \"Authorization: Bearer $TOKEN\" \\\n")
	fmt.Fprintf(&b, "    \"$CLAWVISOR_APP_URL/api/runtime/llm-credentials?agent_id=$AGENT_ID\")\n")
	fmt.Fprintf(&b, "  if echo \"$RESP\" | jq -e --arg p \"$PROVIDER\" '.credentials[] | select(.provider==$p and (.stored==true or .agent_stored==true))' >/dev/null 2>&1; then\n")
	fmt.Fprintf(&b, "    echo 'key vaulted'; break\n")
	fmt.Fprintf(&b, "  fi\n")
	fmt.Fprintf(&b, "  sleep 2\n")
	fmt.Fprintf(&b, "done\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If the loop ends without `key vaulted`, surface that to the user and STOP.\n\n")
	return b.String()
}

// upstreamKeyPrefix returns the canonical leading characters for the
// provider's API keys. Used by the env-detect probe in sectionVaultUpstreamKey
// to confirm the env var holds a plausibly-shaped key without revealing the
// rest of the value.
func upstreamKeyPrefix(provider string) string {
	if provider == "openai" {
		return "sk-"
	}
	return "sk-ant-"
}

// ── Shared helpers for the one-paste setup skill (Claude Code, Codex) ────────

// sectionClaimedConnect renders the connect-with-claim curl + token-file
// write. The claim is the user's pre-authorization from the dashboard;
// the connect endpoint consumes it and auto-approves in one round-trip,
// so the curl returns the agent token directly (no waiting, no second
// dashboard click).
func sectionClaimedConnect(harness, appURL, llmURL, claim, agentName string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 1. Register and persist the token\n\n")
	fmt.Fprintf(&b, "The claim code below is the user's pre-authorization — the connect endpoint\n")
	fmt.Fprintf(&b, "consumes it and returns the agent token immediately. No second dashboard\n")
	fmt.Fprintf(&b, "click required.\n\n")
	fmt.Fprintf(&b, "Set the variables this skill uses (already filled in). Two URLs because\n")
	fmt.Fprintf(&b, "Clawvisor's control plane (registration, dashboard, credentials) and its\n")
	fmt.Fprintf(&b, "LLM proxy (`ANTHROPIC_BASE_URL` / OpenAI `base_url`) can live on\n")
	fmt.Fprintf(&b, "separate hosts in split deployments:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "export CLAWVISOR_APP_URL=%q\n", appURL)
	fmt.Fprintf(&b, "export CLAWVISOR_LLM_URL=%q\n", llmURL)
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
	fmt.Fprintf(&b, "> `$CLAWVISOR_APP_URL/dashboard/agents` when you're ready. The previous install's\n")
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
		fmt.Fprintf(&b, "  \"$CLAWVISOR_APP_URL/api/agents/connect?claim=%s&name=$AGENT_NAME&harness=%s\" \\\n", claim, harness)
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
	fmt.Fprintf(&b, "  Ask the user to refresh `$CLAWVISOR_APP_URL/dashboard/agents` and re-paste the\n")
	fmt.Fprintf(&b, "  one-liner from the Connect-an-Agent panel.\n")
	fmt.Fprintf(&b, "- **AGENT_NAME_EXISTS** — an agent with this name already exists. The user\n")
	fmt.Fprintf(&b, "  can delete the old one from the dashboard, or you can pick a fresh name\n")
	fmt.Fprintf(&b, "  (e.g. `%s-2`) by re-running with `AGENT_NAME` set differently.\n", agentName)
	fmt.Fprintf(&b, "- **HTTP 5xx** — Clawvisor is unhealthy. Ask the user to check the daemon.\n")
	fmt.Fprintf(&b, "- **Connection refused / network unreachable / curl exit 7** — *not\n")
	fmt.Fprintf(&b, "  automatically a \"Clawvisor is down\".* If you are an agent running inside\n")
	fmt.Fprintf(&b, "  a network sandbox (Codex with restrictive `sandbox_mode`, or any harness\n")
	fmt.Fprintf(&b, "  with default-deny egress), the sandbox is the more likely cause — the\n")
	fmt.Fprintf(&b, "  user's normal shell can reach `$CLAWVISOR_APP_URL`, but yours can't. Ask\n")
	fmt.Fprintf(&b, "  the user verbatim: \"My sandbox is refusing the connection to Clawvisor\n")
	fmt.Fprintf(&b, "  at `$CLAWVISOR_APP_URL`. Can you grant me elevated local network access\n")
	fmt.Fprintf(&b, "  (e.g. approve workspace-write with `network_access=true`, or rerun me\n")
	fmt.Fprintf(&b, "  with `--full-auto` / `--dangerously-bypass-approvals-and-sandbox`) so\n")
	fmt.Fprintf(&b, "  the install can reach the daemon?\" Only conclude \"Clawvisor is down\"\n")
	fmt.Fprintf(&b, "  after the user confirms they can `curl $CLAWVISOR_APP_URL/api/status`\n")
	fmt.Fprintf(&b, "  successfully from their own shell.\n\n")
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
	preamble := fmt.Sprintf("The passthrough smoke test failed with an upstream auth error — the user\nhas no working login session and no `%s` in env. Either they fix that\nand re-run the install, or we vault a %s key here for the proxy to\nsubstitute on every call. This step does the second.\n\n", envVar, providerLabel)
	return sectionVaultUpstreamKeyWithPreamble(heading, preamble, provider, providerLabel, envVar, keyPrefix, dashboardPath)
}

// sectionVaultUpstreamKeyWithPreamble is the shared body of the env-detect +
// stdin-pipe + dashboard-fallback vault flow. sectionVaultUpstreamKey wraps it
// with the Claude Code / Codex "passthrough smoke test failed" framing; the
// Hermes / OpenClaw entry point (sectionEnsureVaultedKey) supplies its own
// preamble because those harnesses have no passthrough mode and vault runs
// unconditionally when no existing credential is found.
func sectionVaultUpstreamKeyWithPreamble(heading, preamble, provider, providerLabel, envVar, keyPrefix, dashboardPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", heading)
	b.WriteString(preamble)
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
	fmt.Fprintf(&b, "  curl -sS -X PUT \"$CLAWVISOR_APP_URL/api/runtime/llm-credentials/%s\" \\\n", provider)
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
	fmt.Fprintf(&b, "echo \"$CLAWVISOR_APP_URL%s?for=$AGENT_ID\"\n", dashboardPath)
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
	fmt.Fprintf(&b, "    \"$CLAWVISOR_APP_URL/api/runtime/llm-credentials?agent_id=$AGENT_ID\")\n")
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

// diffWalkerPython is the body of the python3 heredoc both uninstall
// skills emit. It walks ~/.clawvisor/diffs/$AGENT_NAME/*.json and reverses
// each record:
//
//   - json_keys: for each entry, restore the prior_value (or delete the
//     path if prior was null). Prunes parent objects we made empty.
//   - text_append / text_prepend: find the install's exact recorded content
//     and delete the first occurrence (with whitespace-variant fallbacks).
//
// Defined once so both Claude Code and Codex uninstall skills emit the
// same logic and the prior-value-restore property holds uniformly.
const diffWalkerPython = `python3 - <<'PY'
import json, os, glob
agent = os.environ['AGENT_NAME']
diffs_dir = os.path.expanduser(f'~/.clawvisor/diffs/{agent}')
def set_at(doc, parts, value):
    cur = doc
    for p in parts[:-1]:
        if not isinstance(cur, dict): return
        cur = cur.setdefault(p, {})
    if isinstance(cur, dict): cur[parts[-1]] = value
def del_at(doc, parts):
    cur = doc
    for p in parts[:-1]:
        if not isinstance(cur, dict) or p not in cur: return
        cur = cur[p]
    if isinstance(cur, dict): cur.pop(parts[-1], None)
def prune(d):
    for k, v in list(d.items()):
        if isinstance(v, dict):
            prune(v)
            if not v: del d[k]
for path in sorted(glob.glob(os.path.join(diffs_dir, '*.json'))):
    with open(path) as f: rec = json.load(f)
    target = os.path.expanduser(rec['file'])
    if not os.path.exists(target): continue
    if rec['type'] == 'json_keys':
        with open(target) as f: doc = json.load(f)
        # Newer records have 'entries' with prior_value; legacy 'paths' just
        # deletes (no prior captured — best-effort revert).
        entries = rec.get('entries') or [{'path': p, 'prior_value': None} for p in rec.get('paths', [])]
        for entry in entries:
            parts = entry['path'].split('.')
            if entry.get('prior_value') is None:
                del_at(doc, parts)
            else:
                set_at(doc, parts, entry['prior_value'])
        prune(doc)
        with open(target, 'w') as f: json.dump(doc, f, indent=2); f.write('\n')
    elif rec['type'] in ('text_append', 'text_prepend'):
        with open(target) as f: body = f.read()
        chunk = rec['content']
        for needle in ('\n' + chunk + '\n', chunk + '\n\n', chunk + '\n', chunk):
            if needle in body:
                body = body.replace(needle, '', 1); break
        with open(target, 'w') as f: f.write(body)
PY
`

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

// recordJSONKeyDiff renders the shell snippet that records the dotted JSON
// paths the install added to `targetFile` **along with their prior values**
// (or null if the path didn't exist before). Uninstall walks the entries
// and restores each: null prior → delete the path; non-null prior → set the
// value back. This preserves any user-set values our install overwrote —
// without prior-value capture, uninstall would permanently delete keys the
// user had set themselves before we showed up.
//
// `paths` is a comma-separated list of dotted JSON paths
// (e.g. `env.ANTHROPIC_BASE_URL,env.ANTHROPIC_CUSTOM_HEADERS`). The shell
// reads each prior value from `targetFile` via jq getpath() and writes a
// single diff record listing all (path, prior) pairs.
//
// CALL THIS BEFORE THE MERGE — the prior-value read has to see the file
// as it was, not after our keys land.
func recordJSONKeyDiff(id, targetFile, paths string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mkdir -p ~/.clawvisor/diffs/$AGENT_NAME\n")
	// Snapshot the pre-merge document so we can read prior values. Three
	// failure modes that the obvious `cat || echo '{}'` form drops on the
	// floor:
	//   - File is empty (or whitespace-only): cat exits 0 with empty output;
	//     `--argjson prior ""` errors and the whole diff record is never
	//     written.
	//   - File has invalid JSON (typos, comments, truncation): cat exits 0
	//     with garbage; same failure on --argjson.
	//   - File is valid JSON but not an object (e.g. an array at the root):
	//     getpath() would error on the lookup.
	// Pipe through `jq` to validate AND require an object — anything that
	// doesn't parse-as-object falls back to `{}` so the diff still records
	// (with prior_value=null for every path, which means uninstall deletes
	// the keys we added; the best we can do when there's no prior to
	// restore).
	fmt.Fprintf(&b, "PRIOR_JSON=$(jq -c 'if type == \"object\" then . else {} end' %s 2>/dev/null)\n", targetFile)
	// `[ -n ] || ...` (not `[ -z ] && ...`) so the line returns 0 on both
	// branches — otherwise `set -e` callers see a phantom failure when the
	// candidate JSON was non-empty.
	fmt.Fprintf(&b, "[ -n \"$PRIOR_JSON\" ] || PRIOR_JSON='{}'\n")
	fmt.Fprintf(&b, "jq -n --argjson prior \"$PRIOR_JSON\" \\\n")
	fmt.Fprintf(&b, "  --arg file %s \\\n", targetFile)
	fmt.Fprintf(&b, "  --argjson paths '[%s]' '\n", quoteJSONPathList(paths))
	fmt.Fprintf(&b, "  {file: $file, type: \"json_keys\",\n")
	fmt.Fprintf(&b, "   entries: [$paths[] as $p | {path: $p, prior_value: ($prior | getpath($p / \".\"))}]}' \\\n")
	fmt.Fprintf(&b, "  > ~/.clawvisor/diffs/$AGENT_NAME/%s.json\n", id)
	return b.String()
}

// quoteJSONPathList accepts a comma-separated list of unquoted dotted JSON
// paths (e.g. `env.X,env.Y`) and returns a JSON-array body of quoted strings
// (e.g. `"env.X","env.Y"`). Used by recordJSONKeyDiff to feed jq's --argjson.
func quoteJSONPathList(paths string) string {
	if paths == "" {
		return ""
	}
	parts := strings.Split(paths, ",")
	for i, p := range parts {
		parts[i] = `"` + strings.TrimSpace(p) + `"`
	}
	return strings.Join(parts, ",")
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
	fmt.Fprintf(&b, "- **Connection refused / network unreachable** — *don't jump to \"Clawvisor\n")
	fmt.Fprintf(&b, "  is down\".* If you are an agent running inside a network sandbox (Codex\n")
	fmt.Fprintf(&b, "  with restrictive `sandbox_mode`, or any harness with default-deny\n")
	fmt.Fprintf(&b, "  egress), the sandbox is the more likely cause — your sibling `codex` or\n")
	fmt.Fprintf(&b, "  `claude` process inherits the same restriction and can't reach\n")
	fmt.Fprintf(&b, "  `$CLAWVISOR_LLM_URL`. Ask the user verbatim: \"My sandbox is refusing\n")
	fmt.Fprintf(&b, "  the connection to Clawvisor at `$CLAWVISOR_LLM_URL`. Can you grant me\n")
	fmt.Fprintf(&b, "  elevated local network access (e.g. approve workspace-write with\n")
	fmt.Fprintf(&b, "  `network_access=true`, or rerun me with `--full-auto` /\n")
	fmt.Fprintf(&b, "  `--dangerously-bypass-approvals-and-sandbox`) so the smoke test can\n")
	fmt.Fprintf(&b, "  reach the daemon?\" Only after the user confirms they can\n")
	fmt.Fprintf(&b, "  `curl $CLAWVISOR_LLM_URL/api/status` successfully from their own shell\n")
	fmt.Fprintf(&b, "  is it correct to say \"Clawvisor daemon is not running\". Either way,\n")
	fmt.Fprintf(&b, "  surface and STOP.\n")
	fmt.Fprintf(&b, "- **Timeout** — Clawvisor is unreachable or hung. Surface and STOP.\n")
	fmt.Fprintf(&b, "- **Anything else** — surface and STOP. Don't write any config; don't\n")
	fmt.Fprintf(&b, "  guess at a fix. The user can re-run the install after debugging.\n\n")
	return b.String()
}

// sectionInstallSummary renders the one-screen summary the agent prints to the
// user right before self-uninstall. The fields are derived from in-context
// state the agent already tracked: $AGENT_NAME, $MODE (passthrough|swap), and
// the user's default-vs-alias answer from the make-default question. The
// harness-specific bits — provider label and the revert command — come in as
// arguments.
func sectionInstallSummary(stepNum int, harness, provider, revertCmd, aliasName string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %d. Print the install summary for the user\n\n", stepNum)
	fmt.Fprintf(&b, "Before the self-uninstall step, surface a one-screen recap so the user can\n")
	fmt.Fprintf(&b, "see exactly what landed and how to revert. Substitute concrete values for\n")
	fmt.Fprintf(&b, "every placeholder — no `$VAR`, no `<…>` — using the state you tracked\n")
	fmt.Fprintf(&b, "during the install (`$AGENT_NAME`, `$MODE`, the user's default-vs-alias\n")
	fmt.Fprintf(&b, "answer) and the actual paths you wrote to.\n\n")
	fmt.Fprintf(&b, "Print this block verbatim (with substitutions) — no extra prose around it:\n\n")
	fmt.Fprintf(&b, "```\n")
	fmt.Fprintf(&b, "Clawvisor install complete\n")
	fmt.Fprintf(&b, "──────────────────────────\n")
	fmt.Fprintf(&b, "Agent:        <$AGENT_NAME>\n")
	fmt.Fprintf(&b, "Harness:      %s\n", harness)
	fmt.Fprintf(&b, "Provider:     %s\n", provider)
	fmt.Fprintf(&b, "Routing:      <default for every `%s` invocation  |  alias-only via `%s`>\n", strings.ToLower(harness), aliasName)
	fmt.Fprintf(&b, "Auth mode:    <passthrough | swap>\n")
	fmt.Fprintf(&b, "Files changed:\n")
	fmt.Fprintf(&b, "  - ~/.clawvisor/agents/<$AGENT_NAME>.json   (token, mode 600)\n")
	fmt.Fprintf(&b, "  - <every other file you actually touched, with a 1-line reason>\n")
	fmt.Fprintf(&b, "Revert:       %s\n", revertCmd)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Pick exactly one option in the angle-brackets — don't print both. For\n")
	fmt.Fprintf(&b, "**Files changed**, list every file the install wrote to (config files,\n")
	fmt.Fprintf(&b, "shell rc, diff records under `~/.clawvisor/diffs/<$AGENT_NAME>/`) so the\n")
	fmt.Fprintf(&b, "user can audit and so they understand what `Revert` will undo.\n\n")
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
	fmt.Fprintf(&b, "curl -sf \"$CLAWVISOR_APP_URL/skill/uninstall/%s.md?agent_name=$AGENT_NAME\" \\\n", installerTarget)
	fmt.Fprintf(&b, "  --create-dirs -o %s\n", uninstallSkillPath)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**Remove this setup skill.** It's one-shot scaffolding, not needed once\n")
	fmt.Fprintf(&b, "%s is connected:\n\n", harness)
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "%s\n", removeSetupCmd)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Tell the user:\n\n")
	fmt.Fprintf(&b, "- %s is now connected to Clawvisor as `$AGENT_NAME`.\n", harness)
	fmt.Fprintf(&b, "- Manage it from `$CLAWVISOR_APP_URL/dashboard/agents`.\n")
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

	b.WriteString(sectionClaimedConnect("claude-code", ctx.AppURL, ctx.LLMURL, ctx.Claim, ctx.AgentName))

	// Step 2: passthrough smoke test. Don't clear ANTHROPIC_AUTH_TOKEN /
	// ANTHROPIC_API_KEY — let the user's existing auth (claude login OAuth
	// or env-set API key) flow through. The X-Clawvisor-Agent-Token custom
	// header rides alongside for policy ID.
	fmt.Fprintf(&b, "## 2. Smoke-test Clawvisor routing in **passthrough mode**\n\n")
	fmt.Fprintf(&b, "Run a fresh `claude` in a child process pointed at Clawvisor. We do NOT\n")
	fmt.Fprintf(&b, "clear `ANTHROPIC_AUTH_TOKEN` or `ANTHROPIC_API_KEY` here — the user's\n")
	fmt.Fprintf(&b, "existing auth needs to flow through for passthrough mode to work.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "env \\\n")
	fmt.Fprintf(&b, "  ANTHROPIC_BASE_URL=\"$CLAWVISOR_LLM_URL/api\" \\\n")
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
	fmt.Fprintf(&b, "env \\\n")
	fmt.Fprintf(&b, "  ANTHROPIC_BASE_URL=\"$CLAWVISOR_LLM_URL/api\" \\\n")
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
	fmt.Fprintf(&b, "values for `$CLAWVISOR_LLM_URL` and `$TOKEN`. Then record what you added in\n")
	fmt.Fprintf(&b, "an external diff file so the uninstall skill can reverse it — the user's\n")
	fmt.Fprintf(&b, "settings.json stays clean of any Clawvisor-related metadata.\n\n")
	fmt.Fprintf(&b, "**If MODE=passthrough** — keep the user's upstream auth flowing through.\n")
	fmt.Fprintf(&b, "Merge this into settings.json:\n\n")
	fmt.Fprintf(&b, "```json\n")
	fmt.Fprintf(&b, "{\n")
	fmt.Fprintf(&b, "  \"env\": {\n")
	fmt.Fprintf(&b, "    \"ANTHROPIC_BASE_URL\": \"$CLAWVISOR_LLM_URL/api\",\n")
	fmt.Fprintf(&b, "    \"ANTHROPIC_CUSTOM_HEADERS\": \"X-Clawvisor-Agent-Token: $TOKEN\"\n")
	fmt.Fprintf(&b, "  }\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Do NOT add `ANTHROPIC_AUTH_TOKEN` or `ANTHROPIC_API_KEY` keys — that would\n")
	fmt.Fprintf(&b, "blank the user's `claude login` / env key.\n\n")
	fmt.Fprintf(&b, "**Record the diff BEFORE the merge** so the prior values get captured\n")
	fmt.Fprintf(&b, "for restore-on-uninstall (uninstall sets the key back to the recorded\n")
	fmt.Fprintf(&b, "prior, or deletes it if there was none — without this we'd erase any\n")
	fmt.Fprintf(&b, "value the user had set in settings.json before this install):\n\n")
	fmt.Fprintf(&b, "```bash\n")
	b.WriteString(recordJSONKeyDiff("settings", `"$HOME/.claude/settings.json"`, `env.ANTHROPIC_BASE_URL,env.ANTHROPIC_CUSTOM_HEADERS`))
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Then write the merged settings.json (do not include the `_clawvisor`\n")
	fmt.Fprintf(&b, "marker that earlier iterations of this skill used — the diff record\n")
	fmt.Fprintf(&b, "above is the only persistent uninstall trail).\n\n")
	fmt.Fprintf(&b, "**If MODE=swap** — put the `cvis_…` token in the auth slot so Clawvisor\n")
	fmt.Fprintf(&b, "swaps it for the vaulted upstream key:\n\n")
	fmt.Fprintf(&b, "```json\n")
	fmt.Fprintf(&b, "{\n")
	fmt.Fprintf(&b, "  \"env\": {\n")
	fmt.Fprintf(&b, "    \"ANTHROPIC_BASE_URL\": \"$CLAWVISOR_LLM_URL/api\",\n")
	fmt.Fprintf(&b, "    \"ANTHROPIC_AUTH_TOKEN\": \"$TOKEN\",\n")
	fmt.Fprintf(&b, "    \"ANTHROPIC_API_KEY\": \"\"\n")
	fmt.Fprintf(&b, "  }\n")
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Same pre-merge diff capture:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	b.WriteString(recordJSONKeyDiff("settings", `"$HOME/.claude/settings.json"`, `env.ANTHROPIC_BASE_URL,env.ANTHROPIC_AUTH_TOKEN,env.ANTHROPIC_API_KEY`))
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
	fmt.Fprintf(&b, "  ANTHROPIC_BASE_URL=\"$CLAWVISOR_LLM_URL/api\" \\\\\n")
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
	fmt.Fprintf(&b, "  ANTHROPIC_BASE_URL=\"$CLAWVISOR_LLM_URL/api\" \\\\\n")
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

	b.WriteString(sectionInstallSummary(6, "Claude Code", "Anthropic", "`/clawvisor-uninstall`", "claude-cv"))
	b.WriteString(sectionSelfUninstallSetup(7, "Claude Code", "claude-code", "~/.claude/commands/clawvisor-uninstall.md", "rm -f ~/.claude/commands/clawvisor-setup.md"))

	return b.String()
}

// codexProviderID derives the [model_providers.<slug>] key (and matching
// display name) for the Codex config block from the LLM proxy URL host. Lets
// the user install prod, staging, and dev side-by-side in one ~/.codex/config.toml
// without the blocks colliding.
//
//	llm.staging.clawvisor.com → "clawvisor-staging" / "Clawvisor (staging)"
//	llm.clawvisor.com         → "clawvisor"         / "Clawvisor"
//	localhost / anything else → "clawvisor-dev"     / "Clawvisor (dev)"
func codexProviderID(llmURL string) (slug, display string) {
	u, err := url.Parse(llmURL)
	host := ""
	if err == nil && u != nil {
		host = strings.ToLower(u.Hostname())
	}
	switch {
	case strings.Contains(host, "staging"):
		return "clawvisor-staging", "Clawvisor (staging)"
	case strings.HasSuffix(host, "clawvisor.com") && !strings.Contains(host, "dev"):
		return "clawvisor", "Clawvisor"
	default:
		return "clawvisor-dev", "Clawvisor (dev)"
	}
}

func renderCodexInstaller(ctx installerCtx) string {
	var b strings.Builder
	// Env-aware provider slug + display name keyed off the LLM proxy host so
	// prod / staging / dev installs can coexist in one ~/.codex/config.toml.
	slug, display := codexProviderID(ctx.LLMURL)
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

	b.WriteString(sectionClaimedConnect("codex", ctx.AppURL, ctx.LLMURL, ctx.Claim, ctx.AgentName))

	// Step 2: write the provider block in passthrough form. `requires_openai_auth
	// = true` makes Codex send its OAuth/env auth as Authorization upstream;
	// the X-Clawvisor-Agent-Token custom header rides alongside for policy ID.
	fmt.Fprintf(&b, "## 2. Write the Clawvisor provider block (passthrough form)\n\n")
	fmt.Fprintf(&b, "Codex reads `~/.codex/config.toml`. We add a `[model_providers.%s]`\n", slug)
	fmt.Fprintf(&b, "block so `codex -c model_provider=%s` (and the smoke test below)\n", slug)
	fmt.Fprintf(&b, "can target it. The slug is env-derived from the LLM proxy host\n")
	fmt.Fprintf(&b, "(`%s` for this install) so a user with prod + staging + dev installs can\n", slug)
	fmt.Fprintf(&b, "keep all three blocks side-by-side in one config.toml without colliding.\n")
	fmt.Fprintf(&b, "`requires_openai_auth = true` keeps Codex's normal subscription / env-key\n")
	fmt.Fprintf(&b, "auth flowing through; the cvis_ token rides in a custom header for policy\n")
	fmt.Fprintf(&b, "ID only. (If the smoke test fails because the user has no working upstream\n")
	fmt.Fprintf(&b, "auth, step 3 rewrites this block to swap form.)\n\n")
	fmt.Fprintf(&b, "**Idempotent — grep first.** Codex rejects duplicate `[model_providers.<n>]`\n")
	fmt.Fprintf(&b, "entries on startup. We append only the block itself; the uninstall trail\n")
	fmt.Fprintf(&b, "lives outside the file in `~/.clawvisor/diffs/$AGENT_NAME/`.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "mkdir -p ~/.codex ~/.clawvisor/diffs/$AGENT_NAME\n")
	fmt.Fprintf(&b, "if ! grep -q '^\\[model_providers\\.%s\\]' ~/.codex/config.toml 2>/dev/null; then\n", slug)
	fmt.Fprintf(&b, "  CONTENT=$(cat <<EOF\n")
	fmt.Fprintf(&b, "[model_providers.%s]\n", slug)
	fmt.Fprintf(&b, "name = \"%s\"\n", display)
	fmt.Fprintf(&b, "base_url = \"$CLAWVISOR_LLM_URL/api/v1\"\n")
	fmt.Fprintf(&b, "wire_api = \"responses\"\n")
	fmt.Fprintf(&b, "requires_openai_auth = true\n\n")
	fmt.Fprintf(&b, "[model_providers.%s.env_http_headers]\n", slug)
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
	fmt.Fprintf(&b, "CLAWVISOR_AGENT_TOKEN=\"$TOKEN\" codex \\\n")
	fmt.Fprintf(&b, "  -c model_provider=%s \\\n", slug)
	fmt.Fprintf(&b, "  -c sandbox_workspace_write.network_access=true \\\n")
	fmt.Fprintf(&b, "  exec --skip-git-repo-check \"respond with the word OK\"\n")
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
	fmt.Fprintf(&b, "[model_providers.%s]\n", slug)
	fmt.Fprintf(&b, "name = \"%s\"\n", display)
	fmt.Fprintf(&b, "base_url = \"$CLAWVISOR_LLM_URL/api/v1\"\n")
	fmt.Fprintf(&b, "wire_api = \"responses\"\n")
	fmt.Fprintf(&b, "requires_openai_auth = false\n\n")
	fmt.Fprintf(&b, "[model_providers.%s.env_http_headers]\n", slug)
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
	fmt.Fprintf(&b, "CLAWVISOR_AGENT_BEARER=\"Bearer $TOKEN\" codex \\\n")
	fmt.Fprintf(&b, "  -c model_provider=%s \\\n", slug)
	fmt.Fprintf(&b, "  -c sandbox_workspace_write.network_access=true \\\n")
	fmt.Fprintf(&b, "  exec --skip-git-repo-check \"respond with the word OK\"\n")
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
	fmt.Fprintf(&b, "> `model_provider = \"%s\"` at the top of `~/.codex/config.toml` so\n", slug)
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
	fmt.Fprintf(&b, "### 6.a. Default-everywhere — set `model_provider = \"%s\"` as the default\n\n", slug)
	fmt.Fprintf(&b, "Prepend a top-level `model_provider = \"%s\"` line to\n", slug)
	fmt.Fprintf(&b, "`~/.codex/config.toml` (outside any `[…]` section). Record the diff so\n")
	fmt.Fprintf(&b, "the uninstall can find and remove this exact line:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "if ! grep -q '^model_provider = \"%s\"$' ~/.codex/config.toml 2>/dev/null; then\n", slug)
	fmt.Fprintf(&b, "  CONTENT='model_provider = \"%s\"'\n", slug)
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
	fmt.Fprintf(&b, "and the `-c model_provider=%s` flag.\n\n", slug)
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
	fmt.Fprintf(&b, "  codex -c model_provider=%s \"\\$@\"\n", slug)
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
	fmt.Fprintf(&b, "  codex -c model_provider=%s \"\\$@\"\n", slug)
	fmt.Fprintf(&b, "}\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, ")\n")
	b.WriteString(recordTextDiff("codex_cv", `"$RC"`))
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Tell the user to `source \"$RC\"` (or restart their shell), then run\n")
	fmt.Fprintf(&b, "`codex-cv` instead of `codex` when they want Clawvisor routing.\n\n")

	b.WriteString(sectionInstallSummary(7, "Codex", "OpenAI", "invoke the `clawvisor-uninstall` skill", "codex-cv"))
	b.WriteString(sectionSelfUninstallSetup(8, "Codex", "codex", "~/.codex/skills/clawvisor-uninstall/SKILL.md", "rm -rf ~/.codex/skills/clawvisor-setup"))

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
	fmt.Fprintf(&b, "with macOS and every modern Linux). For `json_keys` it RESTORES the prior\n")
	fmt.Fprintf(&b, "value the install captured (or deletes the path if nothing was there);\n")
	fmt.Fprintf(&b, "for `text_append` / `text_prepend` it removes the exact recorded chunk.\n")
	fmt.Fprintf(&b, "Idempotent — re-running it after a partial uninstall is safe:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	b.WriteString(diffWalkerPython)
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
	fmt.Fprintf(&b, "- **Revoke the agent.** Open `%s/dashboard/agents`, find `$AGENT_NAME`, and\n", ctx.AppURL)
	fmt.Fprintf(&b, "  delete it. After delete, the token in `$TOKEN_FILE` (now gone anyway)\n")
	fmt.Fprintf(&b, "  authenticates nothing.\n")
	fmt.Fprintf(&b, "- **Vaulted upstream key (only if you used swap mode).** If you vaulted an\n")
	fmt.Fprintf(&b, "  Anthropic API key during install and don't want Clawvisor to keep it,\n")
	fmt.Fprintf(&b, "  open `%s/dashboard/keys/anthropic` and replace or clear it. Skip this\n", ctx.AppURL)
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
	fmt.Fprintf(&b, "- **Provider block present** — config.toml has a `[model_providers.clawvisor]`,\n")
	fmt.Fprintf(&b, "  `[model_providers.clawvisor-staging]`, or `[model_providers.clawvisor-dev]`\n")
	fmt.Fprintf(&b, "  block (the install picks the slug from the LLM proxy host so prod /\n")
	fmt.Fprintf(&b, "  staging / dev installs don't collide). Always installed by the install\n")
	fmt.Fprintf(&b, "  skill regardless of mode.\n")
	fmt.Fprintf(&b, "- **Default-everywhere** — config.toml has a top-level\n")
	fmt.Fprintf(&b, "  `model_provider = \"clawvisor…\"` line (outside any `[…]` section), and\n")
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
	fmt.Fprintf(&b, "Claude Code uninstall handles every diff type — it's harness-agnostic.\n")
	fmt.Fprintf(&b, "It restores prior JSON values (not just deletes) so any setting the user\n")
	fmt.Fprintf(&b, "had before install comes back exactly:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	b.WriteString(diffWalkerPython)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If `~/.clawvisor/diffs/$AGENT_NAME/` is missing (legacy install or\n")
	fmt.Fprintf(&b, "user-deleted), fall back to surgical removal:\n\n")
	fmt.Fprintf(&b, "- `~/.codex/config.toml`: strip any `[model_providers.clawvisor*]` block\n")
	fmt.Fprintf(&b, "  (everything between the table header and the next `[…]` header — the\n")
	fmt.Fprintf(&b, "  `clawvisor` prefix covers all three env slugs `clawvisor`,\n")
	fmt.Fprintf(&b, "  `clawvisor-staging`, `clawvisor-dev`) and delete any top-level\n")
	fmt.Fprintf(&b, "  `model_provider = \"clawvisor…\"` line.\n")
	fmt.Fprintf(&b, "  ```bash\n")
	fmt.Fprintf(&b, "  awk 'BEGIN{skip=0} /^\\[model_providers\\.clawvisor/{skip=1; next} /^\\[/ && skip{skip=0} !skip' \\\n")
	fmt.Fprintf(&b, "    ~/.codex/config.toml > ~/.codex/config.toml.new && mv ~/.codex/config.toml.new ~/.codex/config.toml\n")
	fmt.Fprintf(&b, "  sed -i.bak -E '/^model_provider = \"clawvisor(-staging|-dev)?\"$/d' ~/.codex/config.toml\n")
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
	fmt.Fprintf(&b, "- **Revoke the agent** at `%s/dashboard/agents` — find `$AGENT_NAME` and\n", ctx.AppURL)
	fmt.Fprintf(&b, "  delete it.\n")
	fmt.Fprintf(&b, "- **Vaulted upstream key (only if you used swap mode)** — open\n")
	fmt.Fprintf(&b, "  `%s/dashboard/keys/openai` if you want to replace or clear the\n", ctx.AppURL)
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
	llmHost := dockerHostURL(ctx.LLMURL)
	b.WriteString(setupFrontmatter("Hermes"))
	fmt.Fprintf(&b, "# Connect Hermes to Clawvisor\n\n")
	fmt.Fprintf(&b, "You are running a one-shot setup skill. The dashboard pre-baked the\n")
	fmt.Fprintf(&b, "Clawvisor URL, a single-use claim code, and the agent name into this file.\n")
	fmt.Fprintf(&b, "The dashboard already approved the connection — no second click is needed.\n\n")
	fmt.Fprintf(&b, "Hermes runs in **swap mode**: Hermes presents the Clawvisor agent token\n")
	fmt.Fprintf(&b, "as the upstream provider's API-key env var; Clawvisor swaps in the user's\n")
	fmt.Fprintf(&b, "vaulted upstream key on each call. This skill first detects which\n")
	fmt.Fprintf(&b, "provider Hermes is using today (Anthropic or OpenAI), confirms with the\n")
	fmt.Fprintf(&b, "user, then vaults a key (if one isn't already vaulted) and reconfigures\n")
	fmt.Fprintf(&b, "Hermes to point at Clawvisor.\n\n")

	// Step 1: auto-approved claim connect → token saved to $TOKEN_FILE.
	b.WriteString(sectionClaimedConnect("hermes", ctx.AppURL, ctx.LLMURL, ctx.Claim, ctx.AgentName))

	// Step 2: detect the provider (sets $PROVIDER + shell-derived vars).
	b.WriteString(sectionDetectProviderHermes(2))

	// Step 3: detect existing vaulted credential; vault one if absent.
	b.WriteString(sectionEnsureVaultedKeyDynamic(3))

	// Step 4: probe Hermes deployment (helper picks mode at runtime).
	fmt.Fprintf(&b, "## 4. Probe the Hermes deployment\n\n")
	fmt.Fprintf(&b, "Figure out where Hermes runs on this user's machine — the rest of the\n")
	fmt.Fprintf(&b, "skill branches on the answer. Use shell commands first; ask the user only\n")
	fmt.Fprintf(&b, "when the machine can't tell you.\n\n")
	fmt.Fprintf(&b, "Use `docker ps` (not `docker compose ps`) for the container check — the\n")
	fmt.Fprintf(&b, "compose form only sees containers from the current working directory's\n")
	fmt.Fprintf(&b, "compose project, so if you're in `~/` or anywhere outside the user's\n")
	fmt.Fprintf(&b, "compose dir it false-negatives on running containers.\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "command -v hermes >/dev/null 2>&1 && echo 'host hermes present'\n")
	fmt.Fprintf(&b, "docker ps --format '{{.Names}}\\t{{.Image}}' 2>/dev/null | grep -i hermes\n")
	fmt.Fprintf(&b, "test -f ~/.hermes/config.yaml && echo 'config file exists'\n")
	fmt.Fprintf(&b, "echo \"$SHELL\"\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Pick one of three modes and remember it as `$HERMES_MODE`:\n\n")
	fmt.Fprintf(&b, "- **host** — `hermes` is on `$PATH` on this machine.\n")
	fmt.Fprintf(&b, "- **docker** — `docker ps` matched a running container. Capture its\n")
	fmt.Fprintf(&b, "  exact name (first column) as `$HERMES_CONTAINER` — the rest of the\n")
	fmt.Fprintf(&b, "  skill uses `docker exec \"$HERMES_CONTAINER\"` to run commands inside\n")
	fmt.Fprintf(&b, "  the already-running container, which works regardless of the helper's\n")
	fmt.Fprintf(&b, "  current directory.\n")
	fmt.Fprintf(&b, "- **remote** — neither of the above; ask the user for an SSH host\n")
	fmt.Fprintf(&b, "  (`user@example.com`) and store it as `$HERMES_REMOTE`. If they decline,\n")
	fmt.Fprintf(&b, "  STOP and surface what the probe found — don't guess.\n\n")
	fmt.Fprintf(&b, "Surface what you picked and why in chat so the user can correct you.\n\n")

	// Step 5: preflight — prove the harness can reach Clawvisor from its own
	// execution context. Covers all three modes because the helper picked at
	// runtime.
	fmt.Fprintf(&b, "## 5. Preflight: confirm Hermes can reach Clawvisor\n\n")
	fmt.Fprintf(&b, "A curl from this helper's shell only proves *the helper* can reach\n")
	fmt.Fprintf(&b, "Clawvisor — Hermes may run in a different network namespace (Docker\n")
	fmt.Fprintf(&b, "container, remote host). Run the variant matching `$HERMES_MODE`.\n\n")
	fmt.Fprintf(&b, "**If `$HERMES_MODE=host`:**\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "curl -fsSL -H \"X-Clawvisor-Agent-Token: $TOKEN\" \\\n")
	fmt.Fprintf(&b, "  \"$CLAWVISOR_LLM_URL/api/skill/catalog\" >/dev/null && echo OK\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**If `$HERMES_MODE=docker`:**\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "docker exec -e CLAWVISOR_TOKEN=\"$TOKEN\" \"$HERMES_CONTAINER\" sh -c '\n")
	fmt.Fprintf(&b, "  curl -fsSL -H \"X-Clawvisor-Agent-Token: $CLAWVISOR_TOKEN\" \\\n")
	fmt.Fprintf(&b, "    \"%s/api/skill/catalog\" >/dev/null && echo OK\n", llmHost)
	fmt.Fprintf(&b, "'\n")
	fmt.Fprintf(&b, "```\n\n")
	if strings.Contains(llmHost, "host.docker.internal") {
		fmt.Fprintf(&b, "If `OK` doesn't appear: on Linux `host.docker.internal` doesn't resolve\n")
		fmt.Fprintf(&b, "by default — add `--add-host=host.docker.internal:host-gateway`, or\n")
		fmt.Fprintf(&b, "ensure Clawvisor is bound to `0.0.0.0` (not `127.0.0.1`).\n\n")
	}
	fmt.Fprintf(&b, "**If `$HERMES_MODE=remote`:**\n\n")
	fmt.Fprintf(&b, "Define a remote-reachable base URL once (the dashboard rendered\n")
	fmt.Fprintf(&b, "`%s`; if that's localhost, replace it with a relay, public, VPN, or\n", ctx.LLMURL)
	fmt.Fprintf(&b, "LAN URL the remote host can reach):\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "export HERMES_CLAWVISOR_URL='<remote-reachable Clawvisor URL>'\n")
	fmt.Fprintf(&b, "ssh \"$HERMES_REMOTE\" \"curl -fsSL \\\n")
	fmt.Fprintf(&b, "  -H 'X-Clawvisor-Agent-Token: $TOKEN' \\\n")
	fmt.Fprintf(&b, "  '$HERMES_CLAWVISOR_URL/api/skill/catalog' >/dev/null && echo OK\"\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Don't proceed past this step until preflight returns `OK`. Wrong URL\n")
	fmt.Fprintf(&b, "now means Hermes can't reach Clawvisor after configure bakes the URL in.\n\n")

	// Step 6: configure. Ask user env vs file; emit per-mode snippets that
	// substitute $BASE_ENV / $KEY_ENV at install time so the resolved
	// provider's variable names land in the rc / config file.
	fmt.Fprintf(&b, "## 6. Configure Hermes\n\n")
	fmt.Fprintf(&b, "Ask the user once:\n\n")
	fmt.Fprintf(&b, "> Should I configure Hermes via **environment variables on each launch**\n")
	fmt.Fprintf(&b, "> (recommended — clean, no persistent state) or via a **persistent\n")
	fmt.Fprintf(&b, "> `~/.hermes/config.yaml`** (set-and-forget)? Default is env.\n\n")
	fmt.Fprintf(&b, "Remember the answer as `$HERMES_CONFIG` (`env` or `file`).\n\n")
	fmt.Fprintf(&b, "### 6.a. Env-var snippets (when `$HERMES_CONFIG=env`)\n\n")
	fmt.Fprintf(&b, "**host:** `env` accepts NAME=VALUE pairs in argv, so we can set\n")
	fmt.Fprintf(&b, "dynamically-named provider env vars from `$BASE_ENV` / `$KEY_ENV`:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "env \\\n")
	fmt.Fprintf(&b, "  \"$BASE_ENV=$CLAWVISOR_LLM_URL$BASE_PATH\" \\\n")
	fmt.Fprintf(&b, "  \"$KEY_ENV=$TOKEN\" \\\n")
	fmt.Fprintf(&b, "  hermes chat\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Optional ergonomic alias (`hermes-cv`) — append to the user's shell rc.\n")
	fmt.Fprintf(&b, "The function body is constructed with the *resolved* names from\n")
	fmt.Fprintf(&b, "`$BASE_ENV` / `$KEY_ENV`, so what lands in the rc is e.g.\n")
	fmt.Fprintf(&b, "`ANTHROPIC_BASE_URL=...` or `OPENAI_BASE_URL=...` literally:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "case \"$SHELL\" in\n")
	fmt.Fprintf(&b, "  */zsh)  RC=~/.zshrc ;;\n")
	fmt.Fprintf(&b, "  */bash) RC=~/.bashrc ;;\n")
	fmt.Fprintf(&b, "  *)      RC=\"\"; echo \"unknown shell: $SHELL — append manually\" ;;\n")
	fmt.Fprintf(&b, "esac\n")
	fmt.Fprintf(&b, "if [ -n \"$RC\" ]; then\n")
	fmt.Fprintf(&b, "  CONTENT=$(printf 'hermes-cv() {\\n  %%s=\"%%s\" \\\\\\n  %%s=$(jq -r .token $HOME/.clawvisor/agents/%s.json) \\\\\\n  hermes \"$@\"\\n}\\n' \\\n", ctx.AgentName)
	fmt.Fprintf(&b, "    \"$BASE_ENV\" \"$CLAWVISOR_LLM_URL$BASE_PATH\" \"$KEY_ENV\")\n")
	b.WriteString(recordTextDiff("hermes_cv", `"$RC"`))
	fmt.Fprintf(&b, "fi\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**docker:**\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "docker exec -it \\\n")
	fmt.Fprintf(&b, "  -e \"$BASE_ENV=%s$BASE_PATH\" \\\n", llmHost)
	fmt.Fprintf(&b, "  -e \"$KEY_ENV=$TOKEN\" \\\n")
	fmt.Fprintf(&b, "  \"$HERMES_CONTAINER\" hermes chat\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**remote:**\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "ssh \"$HERMES_REMOTE\" 'mkdir -p ~/.hermes && umask 077 && cat > ~/.hermes/.token.tmp && (sleep 15; rm -f ~/.hermes/.token.tmp) >/dev/null 2>&1 &' <<EOF\n")
	fmt.Fprintf(&b, "$TOKEN\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, `ssh -t "$HERMES_REMOTE" "token=\$(cat ~/.hermes/.token.tmp); rm -f ~/.hermes/.token.tmp; export $BASE_ENV='$HERMES_CLAWVISOR_URL$BASE_PATH' $KEY_ENV=\"\$token\" && hermes chat"`+"\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "### 6.b. Config-file snippets (when `$HERMES_CONFIG=file`)\n\n")
	fmt.Fprintf(&b, "Hermes's docs are explicit: secrets go in `~/.hermes/.env`, everything\n")
	fmt.Fprintf(&b, "else in `~/.hermes/config.yaml`. The YAML references the env var via\n")
	fmt.Fprintf(&b, "`${HERMES_CV_API_KEY}` substitution, so the token doesn't sit inline\n")
	fmt.Fprintf(&b, "next to the rest of the config. (Use `HERMES_CV_API_KEY` rather than\n")
	fmt.Fprintf(&b, "`$KEY_ENV`'s value here — `$KEY_ENV` is the upstream provider's env\n")
	fmt.Fprintf(&b, "var, which Hermes also recognizes, and writing both forms in the\n")
	fmt.Fprintf(&b, "same file would shadow each other.)\n\n")
	fmt.Fprintf(&b, "If the user re-runs setup, the token rotates and `.env` must be\n")
	fmt.Fprintf(&b, "re-written; `config.yaml` survives unchanged.\n\n")
	fmt.Fprintf(&b, "**host:**\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "mkdir -p ~/.hermes\n")
	fmt.Fprintf(&b, "# Token to .env — strip any prior HERMES_CV_API_KEY entry first so\n")
	fmt.Fprintf(&b, "# re-runs don't append duplicates.\n")
	fmt.Fprintf(&b, "touch ~/.hermes/.env\n")
	fmt.Fprintf(&b, "{ grep -v '^HERMES_CV_API_KEY=' ~/.hermes/.env 2>/dev/null; echo \"HERMES_CV_API_KEY=$TOKEN\"; } > ~/.hermes/.env.tmp \\\n")
	fmt.Fprintf(&b, "  && mv ~/.hermes/.env.tmp ~/.hermes/.env\n")
	fmt.Fprintf(&b, "chmod 600 ~/.hermes/.env\n")
	fmt.Fprintf(&b, "# Non-secret config — references the .env var via ${VAR} substitution\n")
	fmt.Fprintf(&b, "cat > ~/.hermes/config.yaml <<EOF\n")
	fmt.Fprintf(&b, "model:\n")
	fmt.Fprintf(&b, "  provider: custom\n")
	fmt.Fprintf(&b, "  base_url: \"$CLAWVISOR_LLM_URL$BASE_PATH\"\n")
	fmt.Fprintf(&b, "  api_key: \"\\${HERMES_CV_API_KEY}\"\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**docker:** same shape, but write both files to the host path that's\n")
	fmt.Fprintf(&b, "mounted into the container (`~/.hermes`, typically at `/root/.hermes`\n")
	fmt.Fprintf(&b, "in the container). Use `base_url: \"%s$BASE_PATH\"` so the container\n", llmHost)
	fmt.Fprintf(&b, "can resolve the Clawvisor host.\n\n")
	fmt.Fprintf(&b, "**remote:**\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "ssh \"$HERMES_REMOTE\" 'mkdir -p ~/.hermes && touch ~/.hermes/.env && \\\n")
	fmt.Fprintf(&b, "  { grep -v \"^HERMES_CV_API_KEY=\" ~/.hermes/.env 2>/dev/null; echo \"HERMES_CV_API_KEY=$(cat)\"; } > ~/.hermes/.env.tmp && \\\n")
	fmt.Fprintf(&b, "  mv ~/.hermes/.env.tmp ~/.hermes/.env && chmod 600 ~/.hermes/.env' <<EOF\n")
	fmt.Fprintf(&b, "$TOKEN\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, "ssh \"$HERMES_REMOTE\" \"cat > ~/.hermes/config.yaml\" <<EOF\n")
	fmt.Fprintf(&b, "model:\n")
	fmt.Fprintf(&b, "  provider: custom\n")
	fmt.Fprintf(&b, "  base_url: \"$HERMES_CLAWVISOR_URL$BASE_PATH\"\n")
	fmt.Fprintf(&b, "  api_key: \"\\${HERMES_CV_API_KEY}\"\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, "```\n\n")

	// Step 7: uninstall reference doc. Refer to the env var names by their
	// shell-variable form ($BASE_ENV / $KEY_ENV) since the user picked the
	// provider at install time — the uninstall reference is read after
	// install, so the actual names are resolved when the user reads it.
	b.WriteString(sectionUninstallDoc("hermes", `1. Remove the `+"`model:`"+` block from `+"`~/.hermes/config.yaml`"+` (or unset the provider's `+"`*_BASE_URL`"+`/`+"`*_API_KEY`"+` env vars if you used env vars).
2. Remove the `+"`HERMES_CV_API_KEY`"+` line from `+"`~/.hermes/.env`"+` if you used the config-file path.
3. Remove the `+"`hermes-cv`"+` function from your shell rc if you added one (diff record in `+"`~/.clawvisor/diffs/"+ctx.AgentName+"/hermes_cv.json`"+`).
4. Delete the token file: `+"`rm ~/.clawvisor/agents/"+ctx.AgentName+".json`"+`.
5. Revoke the agent in the Clawvisor dashboard under Agents → `+ctx.AgentName+` → Delete.
6. Optional: remove the user-level upstream key from Clawvisor credentials if no other agents use it (Anthropic or OpenAI, depending on what you vaulted).
`, 7))

	// Step 8: self-uninstall — remove this setup skill from the helper.
	b.WriteString(sectionSelfUninstall("hermes", helperSetupCleanupCommands(), 8))

	return b.String()
}


func renderOpenClawInstaller(ctx installerCtx) string {
	var b strings.Builder
	maxTokens := openClawDefaultMaxTokens()
	llmHost := dockerHostURL(ctx.LLMURL)
	b.WriteString(setupFrontmatter("OpenClaw"))
	fmt.Fprintf(&b, "# Connect OpenClaw to Clawvisor\n\n")
	fmt.Fprintf(&b, "You are running a one-shot setup skill. The dashboard pre-baked the\n")
	fmt.Fprintf(&b, "Clawvisor URL, a single-use claim code, and the agent name into this file.\n")
	fmt.Fprintf(&b, "The dashboard already approved the connection — no second click is needed.\n\n")
	fmt.Fprintf(&b, "OpenClaw points its LLM base URL at Clawvisor's provider-compatible\n")
	fmt.Fprintf(&b, "endpoint and uses the minted Clawvisor agent token as the custom API\n")
	fmt.Fprintf(&b, "key. This skill first detects which provider OpenClaw is using today\n")
	fmt.Fprintf(&b, "(Anthropic or OpenAI), confirms with the user, then vaults a key (if one\n")
	fmt.Fprintf(&b, "isn't already vaulted) and reconfigures OpenClaw to point at Clawvisor.\n\n")

	// Step 1: auto-approved claim connect → token saved to $TOKEN_FILE.
	b.WriteString(sectionClaimedConnect("openclaw", ctx.AppURL, ctx.LLMURL, ctx.Claim, ctx.AgentName))

	// Step 2: detect the provider (sets $PROVIDER + shell-derived vars).
	b.WriteString(sectionDetectProviderOpenClaw(2))

	// Step 3: detect existing vaulted credential; vault one if absent.
	b.WriteString(sectionEnsureVaultedKeyDynamic(3))

	// Step 4: probe — helper picks mode at runtime.
	fmt.Fprintf(&b, "## 4. Probe the OpenClaw deployment\n\n")
	fmt.Fprintf(&b, "Figure out how the user runs OpenClaw's onboarding command. Don't install\n")
	fmt.Fprintf(&b, "extra OpenClaw components — just learn enough to invoke the right launch\n")
	fmt.Fprintf(&b, "form in step 6.\n\n")
	fmt.Fprintf(&b, "Use `docker ps` (not `docker compose ps`) for the container check — the\n")
	fmt.Fprintf(&b, "compose form only sees containers from the current working directory's\n")
	fmt.Fprintf(&b, "compose project, so if you're in `~/` or anywhere outside the user's\n")
	fmt.Fprintf(&b, "compose dir it false-negatives on running containers (e.g. a real\n")
	fmt.Fprintf(&b, "`openclaw-openclaw-gateway-1` container will be missed).\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "command -v openclaw >/dev/null 2>&1 && echo 'host openclaw present'\n")
	fmt.Fprintf(&b, "docker ps --format '{{.Names}}\\t{{.Image}}' 2>/dev/null | grep -i openclaw\n")
	fmt.Fprintf(&b, "test -f ~/.openclaw/openclaw.json && echo 'openclaw.json exists'\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Pick one of three modes and remember it as `$OPENCLAW_MODE`:\n\n")
	fmt.Fprintf(&b, "- **host** — `openclaw` is on `$PATH` on this machine.\n")
	fmt.Fprintf(&b, "- **docker** — `docker ps` matched a running container. Capture its\n")
	fmt.Fprintf(&b, "  exact name (first column) as `$OPENCLAW_CONTAINER` — the rest of the\n")
	fmt.Fprintf(&b, "  skill uses `docker exec \"$OPENCLAW_CONTAINER\"` to run commands inside\n")
	fmt.Fprintf(&b, "  the already-running container, which works regardless of the helper's\n")
	fmt.Fprintf(&b, "  current directory.\n")
	fmt.Fprintf(&b, "- **remote** — neither of the above; ask the user for an SSH host and\n")
	fmt.Fprintf(&b, "  store it as `$OPENCLAW_REMOTE`. If they decline, STOP — don't guess.\n\n")
	fmt.Fprintf(&b, "Surface what you picked in chat so the user can correct you.\n\n")

	// Step 5: preflight — verify connectivity from OpenClaw's network namespace.
	fmt.Fprintf(&b, "## 5. Preflight: confirm OpenClaw can reach Clawvisor\n\n")
	fmt.Fprintf(&b, "Before `openclaw config set` writes a Clawvisor provider entry into\n")
	fmt.Fprintf(&b, "`~/.openclaw/openclaw.json`, prove the URL works from OpenClaw's own\n")
	fmt.Fprintf(&b, "execution context (not just the helper's shell, which may be a\n")
	fmt.Fprintf(&b, "different network namespace).\n\n")
	fmt.Fprintf(&b, "**If `$OPENCLAW_MODE=host`:**\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "curl -fsSL -H \"X-Clawvisor-Agent-Token: $TOKEN\" \\\n")
	fmt.Fprintf(&b, "  \"$CLAWVISOR_LLM_URL/api/skill/catalog\" >/dev/null && echo OK\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**If `$OPENCLAW_MODE=docker`:**\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "docker exec -e CLAWVISOR_TOKEN=\"$TOKEN\" \"$OPENCLAW_CONTAINER\" sh -c '\n")
	fmt.Fprintf(&b, "  curl -fsSL -H \"X-Clawvisor-Agent-Token: $CLAWVISOR_TOKEN\" \\\n")
	fmt.Fprintf(&b, "    \"%s/api/skill/catalog\" >/dev/null && echo OK\n", llmHost)
	fmt.Fprintf(&b, "'\n")
	fmt.Fprintf(&b, "```\n\n")
	if strings.Contains(llmHost, "host.docker.internal") {
		fmt.Fprintf(&b, "If `OK` doesn't appear: on Linux `host.docker.internal` doesn't resolve\n")
		fmt.Fprintf(&b, "by default — add `--add-host=host.docker.internal:host-gateway`, or\n")
		fmt.Fprintf(&b, "ensure Clawvisor is bound to `0.0.0.0` (not `127.0.0.1`).\n\n")
	}
	fmt.Fprintf(&b, "**If `$OPENCLAW_MODE=remote`:**\n\n")
	fmt.Fprintf(&b, "Define a remote-reachable base URL (the dashboard rendered `%s`; if\n", ctx.LLMURL)
	fmt.Fprintf(&b, "that's localhost, replace it with a relay, public, VPN, or LAN URL the\n")
	fmt.Fprintf(&b, "remote host can reach):\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "export OPENCLAW_CLAWVISOR_URL='<remote-reachable Clawvisor URL>'\n")
	fmt.Fprintf(&b, "ssh \"$OPENCLAW_REMOTE\" \"curl -fsSL \\\n")
	fmt.Fprintf(&b, "  -H 'X-Clawvisor-Agent-Token: $TOKEN' \\\n")
	fmt.Fprintf(&b, "  '$OPENCLAW_CLAWVISOR_URL/api/skill/catalog' >/dev/null && echo OK\"\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Don't proceed past this step until preflight returns `OK`.\n\n")

	// Step 6: configure — `openclaw config set models.providers.<id>` writes
	// the Clawvisor provider into the global ~/.openclaw/openclaw.json
	// registry (per docs.openclaw.ai/concepts/model-providers — there's no
	// per-agent models.json file; all agents inherit from
	// models.providers). `--strict-json` says the value is a JSON value
	// (not a string); `--merge` preserves any sibling providers the user
	// already had configured. `onboard` is for first-time auth, not for
	// adding additional providers, so we don't use it.
	fmt.Fprintf(&b, "## 6. Point OpenClaw at Clawvisor\n\n")
	fmt.Fprintf(&b, "Build the provider JSON once (uses `$BASE_PATH` / `$OPENCLAW_API` /\n")
	fmt.Fprintf(&b, "`$MODEL_ID` / `$CONTEXT_WINDOW` / `$PROVIDER_LABEL` from the case block\n")
	fmt.Fprintf(&b, "in step 2; `$TOKEN` is the Clawvisor agent token from step 1):\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "PROVIDER_JSON=$(jq -n \\\n")
	fmt.Fprintf(&b, "  --arg baseUrl \"$CLAWVISOR_LLM_URL$BASE_PATH\" \\\n")
	fmt.Fprintf(&b, "  --arg apiKey  \"$TOKEN\" \\\n")
	fmt.Fprintf(&b, "  --arg api     \"$OPENCLAW_API\" \\\n")
	fmt.Fprintf(&b, "  --arg modelId \"$MODEL_ID\" \\\n")
	fmt.Fprintf(&b, "  --arg modelName \"Clawvisor ($PROVIDER_LABEL)\" \\\n")
	fmt.Fprintf(&b, "  --argjson contextWindow \"$CONTEXT_WINDOW\" \\\n")
	fmt.Fprintf(&b, "  --argjson maxTokens %d \\\n", maxTokens)
	fmt.Fprintf(&b, "  '{baseUrl:$baseUrl, apiKey:$apiKey, api:$api,\n")
	fmt.Fprintf(&b, "    models:[{id:$modelId, name:$modelName,\n")
	fmt.Fprintf(&b, "             contextWindow:$contextWindow, maxTokens:$maxTokens}]}')\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Then write it via `openclaw config set` in the variant matching\n")
	fmt.Fprintf(&b, "`$OPENCLAW_MODE`. The same `$PROVIDER_JSON` value is reused across all\n")
	fmt.Fprintf(&b, "three. (`--merge` preserves any existing providers; `--strict-json`\n")
	fmt.Fprintf(&b, "treats the value as a JSON object, not a string.)\n\n")
	fmt.Fprintf(&b, "**host:**\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, `python3 -c 'import json, os, sys; fn = os.path.expanduser("~/.openclaw/openclaw.json"); os.makedirs(os.path.dirname(fn), exist_ok=True); d = json.load(open(fn)) if os.path.exists(fn) else {}; p = json.load(sys.stdin); d.setdefault("models", {}).setdefault("providers", {})["clawvisor"] = p; json.dump(d, open(fn, "w"), indent=2)' <<EOF`+"\n")
	fmt.Fprintf(&b, "$PROVIDER_JSON\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**docker:**\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, `docker exec -i "$OPENCLAW_CONTAINER" python3 -c 'import json, os, sys; fn = os.path.expanduser("~/.openclaw/openclaw.json"); os.makedirs(os.path.dirname(fn), exist_ok=True); d = json.load(open(fn)) if os.path.exists(fn) else {}; p = json.load(sys.stdin); d.setdefault("models", {}).setdefault("providers", {})["clawvisor"] = p; json.dump(d, open(fn, "w"), indent=2)' <<EOF`+"\n")
	fmt.Fprintf(&b, "$PROVIDER_JSON\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "Note: the docker variant writes Clawvisor's `$CLAWVISOR_LLM_URL`\n")
	fmt.Fprintf(&b, "verbatim. If Clawvisor runs on the host (not in a separate container\n")
	fmt.Fprintf(&b, "network), rebuild `$PROVIDER_JSON` with `%s` substituted for\n", llmHost)
	fmt.Fprintf(&b, "`$CLAWVISOR_LLM_URL` so the container can reach it.\n\n")
	fmt.Fprintf(&b, "**remote:** pipe `$PROVIDER_JSON` over SSH via stdin substitution so\n")
	fmt.Fprintf(&b, "the JSON's double quotes don't fight with ssh's argv quoting:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "# Rebuild $PROVIDER_JSON for the remote host so the URL is reachable\n")
	fmt.Fprintf(&b, "# from there (uses $OPENCLAW_CLAWVISOR_URL exported in step 5):\n")
	fmt.Fprintf(&b, "PROVIDER_JSON_REMOTE=$(jq -n \\\n")
	fmt.Fprintf(&b, "  --arg baseUrl \"$OPENCLAW_CLAWVISOR_URL$BASE_PATH\" \\\n")
	fmt.Fprintf(&b, "  --arg apiKey  \"$TOKEN\" \\\n")
	fmt.Fprintf(&b, "  --arg api     \"$OPENCLAW_API\" \\\n")
	fmt.Fprintf(&b, "  --arg modelId \"$MODEL_ID\" \\\n")
	fmt.Fprintf(&b, "  --arg modelName \"Clawvisor ($PROVIDER_LABEL)\" \\\n")
	fmt.Fprintf(&b, "  --argjson contextWindow \"$CONTEXT_WINDOW\" \\\n")
	fmt.Fprintf(&b, "  --argjson maxTokens %d \\\n", maxTokens)
	fmt.Fprintf(&b, "  '{baseUrl:$baseUrl, apiKey:$apiKey, api:$api,\n")
	fmt.Fprintf(&b, "    models:[{id:$modelId, name:$modelName,\n")
	fmt.Fprintf(&b, "             contextWindow:$contextWindow, maxTokens:$maxTokens}]}')\n")
	fmt.Fprintf(&b, `ssh "$OPENCLAW_REMOTE" 'python3 -c "import json, os, sys; fn = os.path.expanduser(\"~/.openclaw/openclaw.json\"); os.makedirs(os.path.dirname(fn), exist_ok=True); d = json.load(open(fn)) if os.path.exists(fn) else {}; p = json.load(sys.stdin); d.setdefault(\"models\", {}).setdefault(\"providers\", {})[\"clawvisor\"] = p; json.dump(d, open(fn, \"w\"), indent=2)"' <<EOF`+"\n")
	fmt.Fprintf(&b, "$PROVIDER_JSON_REMOTE\n")
	fmt.Fprintf(&b, "EOF\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "If `openclaw config set` exits non-zero with \"refusing to overwrite\n")
	fmt.Fprintf(&b, "protected map\" or similar, the user already has a `clawvisor` provider\n")
	fmt.Fprintf(&b, "and the merge tried to do something destructive — re-run with\n")
	fmt.Fprintf(&b, "`--replace` added. If it fails with \"openclaw not initialized\" or\n")
	fmt.Fprintf(&b, "similar, the user hasn't run `openclaw onboard` yet — surface that to\n")
	fmt.Fprintf(&b, "the user and STOP (it's a prerequisite, outside this skill's scope).\n\n")
	fmt.Fprintf(&b, "After `openclaw config set` lands, the new provider is available to\n")
	fmt.Fprintf(&b, "every OpenClaw agent. The user can select it from OpenClaw's model\n")
	fmt.Fprintf(&b, "picker (TUI) or by setting `models.default` in `openclaw.json`.\n\n")

	// Step 7: uninstall reference doc.
	b.WriteString(sectionUninstallDoc("openclaw", `1. Remove the Clawvisor provider entry: `+"`openclaw config set models.providers.clawvisor null --strict-json --replace`"+` (or hand-edit `+"`~/.openclaw/openclaw.json`"+` to remove the `+"`clawvisor`"+` key under `+"`models.providers`"+`).
2. If you set `+"`models.default`"+` to the Clawvisor model, point it back at your prior default.
3. Delete the token file: `+"`rm ~/.clawvisor/agents/"+ctx.AgentName+".json`"+`.
4. Revoke the agent in the Clawvisor dashboard under Agents → `+ctx.AgentName+` → Delete.
5. Optional: remove the user-level upstream key from Clawvisor credentials if no other agents use it (Anthropic or OpenAI, depending on what you vaulted).
`, 7))

	// Step 8: self-uninstall — remove this setup skill from the helper.
	b.WriteString(sectionSelfUninstall("openclaw", helperSetupCleanupCommands(), 8))

	return b.String()
}

