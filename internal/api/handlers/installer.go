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

// installerRenderer renders an installer body of one format (shell or
// markdown). Markdown renderers can't fail; shell renderers can if a
// template is mis-typed at runtime — uniformly typed for one dispatch site.
type installerRenderer func(installerCtx) (string, error)

func markdownRenderer(f func(installerCtx) string) installerRenderer {
	return func(c installerCtx) (string, error) { return f(c), nil }
}

func shellRenderer(f func(installerCtx) (string, error)) installerRenderer {
	return f
}

// installerSpec is the per-target wire shape: which extension the bare URL
// redirects to, and which renderer + content type to serve.
type installerSpec struct {
	canonicalExt string
	contentType  string
	render       installerRenderer
}

// installerTargets is the single source of truth for the per-target install
// surface, *including for the dashboard*. The web UI deliberately emits the
// bare URL (no extension) for the curl one-liner and lets the 301 redirect
// here pick the canonical form — so adding a new target is one map entry
// and the frontend automatically picks up the right extension.
var installerTargets = map[InstallerTarget]installerSpec{
	InstallerClaudeCode: {
		canonicalExt: ".sh",
		contentType:  "application/x-sh; charset=utf-8",
		render:       shellRenderer(renderClaudeCodeShellInstaller),
	},
	InstallerCodex: {
		canonicalExt: ".sh",
		contentType:  "application/x-sh; charset=utf-8",
		render:       shellRenderer(renderCodexShellInstaller),
	},
	InstallerHermes: {
		canonicalExt: ".md",
		contentType:  "text/markdown; charset=utf-8",
		render:       markdownRenderer(renderHermesInstaller),
	},
	InstallerOpenClaw: {
		canonicalExt: ".md",
		contentType:  "text/markdown; charset=utf-8",
		render:       markdownRenderer(renderOpenClawInstaller),
	},
}

// parseInstallerTarget reads the {target} path segment and splits it into
// the suffix + bare target. Returns ok=false when the segment has no
// extension; the caller then issues a redirect to the canonical extension
// looked up via installerTargets.
func parseInstallerTarget(raw string) (target InstallerTarget, suffix string, ok bool) {
	switch {
	case strings.HasSuffix(raw, ".sh"):
		return InstallerTarget(strings.TrimSuffix(raw, ".sh")), ".sh", true
	case strings.HasSuffix(raw, ".md"):
		return InstallerTarget(strings.TrimSuffix(raw, ".md")), ".md", true
	default:
		return InstallerTarget(raw), "", false
	}
}

// Setup handles GET /skill/install/{target}. Two content formats live
// behind the same path:
//
//   - `.sh` — the deterministic shell one-liner. Available for the self-
//     install harnesses (claude-code, codex) where every step is fixed.
//   - `.md` — the LLM-driven markdown skill. Used by the cross-target
//     harnesses (hermes, openclaw) where probing the user's environment
//     benefits from an LLM-shaped adaptation loop.
//
// Per-target policy lives in installerTargets (one map entry per target).
// Setup is just routing: parse the suffix, redirect bare URLs to the
// canonical extension, then dispatch via the map. A request whose suffix
// doesn't match the target's canonical extension gets 410 (when the
// alternate extension was previously supported — i.e. claude-code/codex
// requested as .md) or 404 (when the alternate was never supported).
func (h *InstallerHandler) Setup(w http.ResponseWriter, r *http.Request) {
	rawTarget := r.PathValue("target")
	target, suffix, hasSuffix := parseInstallerTarget(rawTarget)
	spec, known := installerTargets[target]
	if !known {
		http.Error(w, "unknown installer target", http.StatusNotFound)
		return
	}

	if !hasSuffix {
		h.redirectToCanonicalExt(w, r, spec)
		return
	}
	if suffix != spec.canonicalExt {
		h.writeNonCanonicalSuffix(w, target, spec, suffix)
		return
	}

	ctx := h.installerCtxFromRequest(r, target)
	body, err := spec.render(ctx)
	if err != nil {
		// Template execution should be infallible at runtime (parsed once
		// at init); a failure here is a server bug and the user can't act
		// on it, so 500 rather than shipping a fake script to their shell.
		http.Error(w, "installer render failure", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", spec.contentType)
	_, _ = w.Write([]byte(body))
}

func (h *InstallerHandler) redirectToCanonicalExt(w http.ResponseWriter, r *http.Request, spec installerSpec) {
	redirectURL := r.URL.Path + spec.canonicalExt
	if raw := r.URL.RawQuery; raw != "" {
		redirectURL += "?" + raw
	}
	http.Redirect(w, r, redirectURL, http.StatusMovedPermanently)
}

// writeNonCanonicalSuffix handles the off-extension cases:
//   - .md for a shell-canonical target → 410 with a pointer at the .sh URL
//     (claude-code/codex used to be markdown; dashboards may still link there).
//   - .sh for a markdown-canonical target → 404 (never had a shell installer).
func (h *InstallerHandler) writeNonCanonicalSuffix(w http.ResponseWriter, target InstallerTarget, spec installerSpec, suffix string) {
	if suffix == ".md" && spec.canonicalExt == ".sh" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusGone)
		fmt.Fprintf(w, "The markdown installer for %s has been replaced by a one-line shell\ninstaller. Update your dashboard paste blob to:\n\n  curl -fsSL \"<host>/skill/install/%s.sh?...\" | sh\n", target, target)
		return
	}
	http.Error(w, "no shell installer available for this target", http.StatusNotFound)
}

// Uninstall handles GET /skill/uninstall/{target}. The deprecated markdown
// installer used to write a `/clawvisor-uninstall` slash command on the
// user's disk that fetched the uninstall skill from this URL. The shell
// installer drops the revert recipe to ~/.clawvisor/uninstall-<target>.md
// directly — no fetch needed — but stale slash commands still hit this
// path. Return 410 with a one-line pointer at the local file so users see
// a clear next step instead of a bare 404.
func (h *InstallerHandler) Uninstall(w http.ResponseWriter, r *http.Request) {
	rawTarget := r.PathValue("target")
	target, _, _ := parseInstallerTarget(rawTarget)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusGone)
	// Only the two self-install targets ever had an uninstall renderer;
	// hermes/openclaw shipped their revert recipe inline in the installer
	// skill. For unknown / never-supported targets, return the same 410
	// with a generic message — anything else is a fetch from a slash
	// command that no longer makes sense.
	if target == InstallerClaudeCode || target == InstallerCodex {
		fmt.Fprintf(w, "The remote uninstall skill has been retired. The shell installer\nalready wrote your revert recipe to:\n\n  ~/.clawvisor/uninstall-%s.md\n\nOpen that file for the step-by-step undo.\n", target)
		return
	}
	fmt.Fprintf(w, "The remote uninstall skill has been retired. If the install left a\nrevert recipe, look under ~/.clawvisor/uninstall-*.md on your disk.\n")
}

// installerCtxFromRequest reads + sanitizes all per-request fields off the
// query string into an installerCtx. Pulled out of Setup so the writers
// above stay focused on dispatch.
func (h *InstallerHandler) installerCtxFromRequest(r *http.Request, target InstallerTarget) installerCtx {
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
	ctx.AgentName = string(target)
	if n := r.URL.Query().Get("agent_name"); n != "" && validAgentName.MatchString(n) {
		ctx.AgentName = n
	}
	return ctx
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

// ── Shared markdown helpers ──────────────────────────────────────────────────
//
// Every installer skill follows the same outline: probe → mint → persist →
// configure → alias → smoke test → uninstall reference →
// self-uninstall. The shared helpers render the steps that don't vary; the
// per-target functions slot in their own configure/alias/self-uninstall.

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
	fmt.Fprintf(&b, "probe below uses `python3 -c yaml.safe_load(...)` to read only\n")
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
	fmt.Fprintf(&b, "  BASE=$(python3 -c \"import yaml; d=yaml.safe_load(open('$HOME/.hermes/config.yaml')) or {}; print((d.get('model') or {}).get('base_url') or '')\" 2>/dev/null || true)\n")
	fmt.Fprintf(&b, "  case \"$BASE\" in\n")
	fmt.Fprintf(&b, "    *anthropic.com*)      DETECTED=\"$DETECTED anthropic\" ;;\n")
	fmt.Fprintf(&b, "    *openai.com*)         DETECTED=\"$DETECTED openai\" ;;\n")
	fmt.Fprintf(&b, "    */api/v1*)            DETECTED=\"$DETECTED openai\" ;;\n")
	fmt.Fprintf(&b, "    */api|*/api/)         DETECTED=\"$DETECTED anthropic\" ;;\n")
	fmt.Fprintf(&b, "  esac\n")
	fmt.Fprintf(&b, "  # model.default name pattern — strongest hint of what's *actively*\n")
	fmt.Fprintf(&b, "  # used, since base_url alone doesn't say which model is selected.\n")
	fmt.Fprintf(&b, "  DEFAULT=$(python3 -c \"import yaml; d=yaml.safe_load(open('$HOME/.hermes/config.yaml')) or {}; print((d.get('model') or {}).get('default') or '')\" 2>/dev/null || true)\n")
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
	fmt.Fprintf(&b, "If yes, vault via stdin pipe (value never enters argv). Note `?agent_id=`\n")
	fmt.Fprintf(&b, "— agent-token writes are constrained to the caller's own agent_id; the\n")
	fmt.Fprintf(&b, "forwarder's agent-scoped-first fallback uses it transparently:\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "printf '%%s' \"$KEY_VALUE\" | jq -Rs '{api_key:.}' | \\\n")
	fmt.Fprintf(&b, "  curl -sS -X PUT \"$CLAWVISOR_APP_URL/api/runtime/llm-credentials/$PROVIDER?agent_id=$AGENT_ID\" \\\n")
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

// ── Per-target renders ───────────────────────────────────────────────────────

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
	fmt.Fprintf(&b, "ssh \"$HERMES_REMOTE\" \"$BASE_ENV='$HERMES_CLAWVISOR_URL$BASE_PATH' $KEY_ENV='$TOKEN' hermes chat\"\n")
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
	fmt.Fprintf(&b, "ssh \"$HERMES_REMOTE\" \"mkdir -p ~/.hermes && touch ~/.hermes/.env && \\\n")
	fmt.Fprintf(&b, "  { grep -v '^HERMES_CV_API_KEY=' ~/.hermes/.env 2>/dev/null; echo HERMES_CV_API_KEY='$TOKEN'; } > ~/.hermes/.env.tmp && \\\n")
	fmt.Fprintf(&b, "  mv ~/.hermes/.env.tmp ~/.hermes/.env && chmod 600 ~/.hermes/.env\"\n")
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
	fmt.Fprintf(&b, "openclaw config set models.providers.clawvisor \"$PROVIDER_JSON\" --strict-json --merge\n")
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "**docker:**\n\n")
	fmt.Fprintf(&b, "```bash\n")
	fmt.Fprintf(&b, "# `docker exec` doesn't expand the helper's $PROVIDER_JSON inside the\n")
	fmt.Fprintf(&b, "# container — pass it via argv after the host shell substitution.\n")
	fmt.Fprintf(&b, "docker exec \"$OPENCLAW_CONTAINER\" openclaw config set models.providers.clawvisor \"$PROVIDER_JSON\" --strict-json --merge\n")
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
	fmt.Fprintf(&b, "ssh \"$OPENCLAW_REMOTE\" 'openclaw config set models.providers.clawvisor \"$(cat)\" --strict-json --merge' <<EOF\n")
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

