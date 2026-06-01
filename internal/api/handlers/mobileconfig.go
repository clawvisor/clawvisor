package handlers

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/relay"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// MobileConfigHandler serves a macOS Configuration Profile that wires Claude
// Desktop to route inference through this Clawvisor instance. Unlike the
// installer-skill flow, there's no agent-side bootstrap — the user
// downloads the .mobileconfig, double-clicks it, and macOS installs it via
// System Settings → Profiles. So this endpoint is the consent gate itself,
// gated by user JWT.
type MobileConfigHandler struct {
	st        store.Store
	relayHost string
	daemonID  string
	isLocal   bool
	// publicURL is the externally reachable lite-proxy endpoint, baked into
	// the plist's inferenceGatewayBaseUrl. Claude Desktop calls that URL
	// from the user's Mac, which in cloud deployments isn't the dashboard
	// origin. Empty falls back to the request host (covers local installs).
	publicURL string
}

func NewMobileConfigHandler(st store.Store, relayHost, daemonID string, isLocal bool, publicURL string) *MobileConfigHandler {
	return &MobileConfigHandler{
		st: st, relayHost: relayHost, daemonID: daemonID, isLocal: isLocal,
		publicURL: strings.TrimRight(strings.TrimSpace(publicURL), "/"),
	}
}

// agentNameSafe constrains user-supplied names to a small, filesystem-safe
// shape — they show up in the plist's PayloadDisplayName and (URL-encoded)
// in the Content-Disposition filename.
//
// NOTE: this is intentionally stricter than the dashboard's
// `sanitizeAgentName` / `validAgentName` (`^[a-zA-Z0-9_.-]{1,64}$`).
// Dashboard-generated defaults (`claude-desktop`, `claude-desktop-2`, …)
// fall inside this regex, but a user-typed name like `My_Agent` or
// `claude.desktop` passes the dashboard validator and is then rejected
// here. Consolidating the validators is a follow-up — keep that in mind
// before relaxing either side.
var agentNameSafe = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// ClaudeDesktop handles GET /api/agents/install/claude-desktop.mobileconfig.
// Requires user JWT — downloading from the dashboard is the consent.
//
// Each download mints a fresh agent + token. If the user already has an
// agent named "claude-desktop" we bump the name to "claude-desktop-2", etc.,
// so re-downloading is non-destructive. The user can revoke individual
// installs by deleting the agent from the dashboard.
func (h *MobileConfigHandler) ClaudeDesktop(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = "claude-desktop"
	}
	if !agentNameSafe.MatchString(name) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "name must match [a-z0-9-]{1,40}")
		return
	}

	existing, err := h.st.ListAgents(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list agents")
		return
	}
	chosen := uniqueAgentName(existing, name)

	rawToken, err := auth.GenerateAgentToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not generate token")
		return
	}
	agent, err := h.st.CreateAgent(r.Context(), user.ID, chosen, auth.HashToken(rawToken))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create agent")
		return
	}
	// Stamp the install context so the dashboard's agent detail page can
	// render the harness-appropriate "Connect this agent" panel — and so
	// re-download / revoke flows can recognize claude-desktop installs.
	// auth_mode=swap because the mobileconfig path always vaults an upstream
	// Anthropic key (Claude Desktop hands us a cvis_ token; we swap it in).
	if err := h.st.SetAgentInstallContext(r.Context(), agent.ID, &store.InstallContext{
		Harness:     "claude-desktop",
		InstallMode: "host",
		HostOS:      "darwin",
		AuthMode:    "swap",
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not stamp install context")
		return
	}

	gatewayURL := h.resolveURL(r) + "/api"
	plist := renderClaudeDesktopMobileConfig(rawToken, gatewayURL, chosen)

	w.Header().Set("Content-Type", "application/x-apple-aspen-config")
	w.Header().Set("Content-Disposition", `attachment; filename="`+chosen+`.mobileconfig"`)
	// The response body embeds `rawToken` directly. Without explicit no-store
	// headers, browsers and intermediate proxies are free to cache the
	// response (mobileconfig downloads look like static file downloads to
	// most caches), and the token then sits in cache directories where any
	// other process with disk access can read it. Pragma is here for the
	// handful of HTTP/1.0 caches still in the wild.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	_, _ = w.Write([]byte(plist))
}

// uniqueAgentName picks the lowest-numbered name not already in `existing`,
// starting from `base` and bumping to base-2, base-3, etc.
func uniqueAgentName(existing []*store.Agent, base string) string {
	names := map[string]bool{}
	for _, a := range existing {
		names[a.Name] = true
	}
	if !names[base] {
		return base
	}
	for i := 2; i < 10000; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if !names[cand] {
			return cand
		}
	}
	// Fallback only reachable with 10k+ collisions — same user would have
	// bigger problems by then. Use a UUID suffix to guarantee uniqueness.
	return base + "-" + uuid.New().String()[:8]
}

func (h *MobileConfigHandler) resolveURL(r *http.Request) string {
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

// renderClaudeDesktopMobileConfig builds the macOS Configuration Profile
// (a plist) that Claude Desktop reads to discover the inference gateway.
// Key shape comes from Anthropic's published payload type
// com.anthropic.claudefordesktop:
//
//   inferenceProvider          = "gateway"
//   inferenceCredentialKind    = "static"
//   inferenceGatewayBaseUrl    = <Clawvisor /api root>
//   inferenceGatewayApiKey     = <agent token>
//
// Two UUIDs are minted: one identifies this specific payload (so the user
// can re-install over an older profile cleanly), one identifies the outer
// configuration profile.
func renderClaudeDesktopMobileConfig(token, gatewayURL, agentName string) string {
	payloadUUID := uuid.New().String()
	profileUUID := uuid.New().String()
	// macOS keys profile install/replace on PayloadIdentifier. If every
	// download emitted the same identifier, the second download (which
	// mints a fresh `claude-desktop-2` agent server-side) would silently
	// replace the first install — the user ends up with two server-side
	// agents but only the second is bound to Claude Desktop, and the first
	// agent's token sits orphaned in the DB until it's manually revoked.
	// Suffix the per-payload AND outer profile identifiers with the agent
	// name so distinct downloads coexist on the user's machine; agentName
	// is already validated by `agentNameSafe` (`^[a-z0-9-]{1,40}$`), so
	// it's safe to splice into a reverse-DNS identifier as-is.
	settingsIdentifier := "com.anthropic.claudefordesktop.settings." + agentName
	profileIdentifier := "com.anthropic.claudefordesktop.profile." + agentName
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
	<dict>
		<key>PayloadContent</key>
		<array>
			<dict>
				<key>PayloadType</key>
				<string>com.anthropic.claudefordesktop</string>
				<key>PayloadIdentifier</key>
				<string>%s</string>
				<key>PayloadUUID</key>
				<string>%s</string>
				<key>PayloadVersion</key>
				<integer>1</integer>
				<key>PayloadDisplayName</key>
				<string>Claude Desktop (Clawvisor: %s)</string>
				<key>inferenceProvider</key>
				<string>gateway</string>
				<key>inferenceCredentialKind</key>
				<string>static</string>
				<key>inferenceGatewayBaseUrl</key>
				<string>%s</string>
				<key>inferenceGatewayApiKey</key>
				<string>%s</string>
			</dict>
		</array>
		<key>PayloadDisplayName</key>
		<string>Claude Desktop Third-Party Inference</string>
		<key>PayloadIdentifier</key>
		<string>%s</string>
		<key>PayloadType</key>
		<string>Configuration</string>
		<key>PayloadUUID</key>
		<string>%s</string>
		<key>PayloadVersion</key>
		<integer>1</integer>
		<key>PayloadScope</key>
		<string>User</string>
	</dict>
</plist>
`, settingsIdentifier, payloadUUID, escapeXML(agentName), escapeXML(gatewayURL), escapeXML(token), profileIdentifier, profileUUID)
}

// escapeXML handles the minimum of plist-required escaping for the values we
// inject. Agent names and tokens are alphanumeric+dashes by construction so
// `&`/`<`/`>` only appear via URL paths in pathological deployments — still
// worth escaping defensively.
func escapeXML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
