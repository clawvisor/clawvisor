package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/runtime/autovault"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
	"golang.org/x/oauth2"
)

// ProxyResolverHandler is the lite-proxy reverse-proxy. The agent harness
// makes its outbound HTTPS call to here; we authenticate the caller via
// X-Clawvisor-Caller, swap any `autovault_…` header placeholders for real
// credentials from the vault (matching ownership against the caller's
// shadow-LLM-token), restore the original target host, and forward.
//
// v1 scope: header-credential placeholders only. Body / query / cookie
// placeholder mutation is Phase 4.
type ProxyResolverHandler struct {
	Store      store.Store
	Vault      vault.Vault
	AdapterReg *adapters.Registry
	Client     *http.Client
	Logger     *slog.Logger

	// AuditEmitter writes one audit_log row per resolver request +
	// per placeholder swapped. nil disables audit logging.
	AuditEmitter *llmproxy.AuditEmitter

	// MaxRequestBytes caps the inbound body. Defaults to 34 MiB to mirror
	// the LLM proxy endpoint (2 MiB above Anthropic's 32 MB Messages cap).
	MaxRequestBytes int64

	// SelfHostnames is the set of hosts this Clawvisor deployment serves
	// itself on. The resolver MUST refuse `X-Clawvisor-Target-Host` values
	// matching any of these — otherwise an agent could read its own audit
	// log via its own placeholder. Defaults to the empty set; populate
	// from config at construction time.
	SelfHostnames []string

	// AllowPrivateNetworks gates whether RFC1918 / loopback / link-local
	// targets are reachable. Defaults to false; flip in self-host
	// development environments.
	AllowPrivateNetworks bool
}

// NewProxyResolverHandler builds the handler with sensible defaults. The
// http.Client has no overall timeout (some upstreams stream) but the
// transport caps response-header time so a slow attacker upstream can't
// hold a goroutine indefinitely. The DialContext re-resolves and
// re-validates each address at dial time, closing the DNS-rebinding
// window between checkSSRF's preflight and Client.Do's actual dial.
func NewProxyResolverHandler(st store.Store, v vault.Vault, logger *slog.Logger) *ProxyResolverHandler {
	if logger == nil {
		logger = slog.Default()
	}
	h := &ProxyResolverHandler{
		Store:           st,
		Vault:           v,
		Logger:          logger,
		MaxRequestBytes: 34 << 20,
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.DialContext = h.safeDialContext
	h.Client = &http.Client{
		Timeout:   0,
		Transport: transport,
		// Refuse to follow redirects. Default http.Client follows up
		// to 10 cross-host redirects, which would replay the swapped
		// vault credential at the redirect target — bypassing the
		// bound-service host allowlist and SSRF preflight. Surface
		// the 3xx to the upstream call site as-is.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return h
}

// safeDialContext re-resolves the dial address and refuses any private/
// loopback/link-local IP unless AllowPrivateNetworks is set. Closes the
// TOCTOU window between checkSSRF's preflight resolution and the
// transport's own dial-time resolution: a short-TTL attacker domain that
// returned a public IP at preflight cannot smuggle a private IP at dial.
func (h *ProxyResolverHandler) safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if h.AllowPrivateNetworks {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	// If the addr is already an IP literal, validate it directly.
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateIP(ip) {
			return nil, fmt.Errorf("dial blocked: %s resolves to private IP %s", host, ip)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("dial blocked: resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("dial blocked: no IPs for %s", host)
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return nil, fmt.Errorf("dial blocked: %s resolves to private IP %s", host, ip)
		}
	}
	// Try every validated IP in order, returning the first successful
	// dial. Without the fallback, a host with a stale or unreachable
	// first record would fail even when later records work — surprising
	// for multi-A or dual-stack hosts that already passed the public-IP
	// check.
	var d net.Dialer
	var firstErr error
	for _, ip := range ips {
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

// Forward handles ANY method on /api/proxy/<path>. Path mapping: the request
// path after `/api/proxy/` becomes the upstream path verbatim.
func (h *ProxyResolverHandler) Forward(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	requestID := r.Header.Get("X-Request-Id")

	// Per-request audit state captured at every exit path.
	var (
		auditAgent       *store.Agent
		auditPlaceholder string
		auditService     string
		auditTargetHost  string
		auditTargetPath  string
		auditStatus      int
		auditDecide      = "allow"
		auditOutcome     string
		auditReason      string
	)
	defer func() {
		if h.AuditEmitter == nil || auditAgent == nil {
			return
		}
		h.AuditEmitter.LogResolverSwap(r.Context(), auditAgent, requestID,
			auditPlaceholder, auditService, auditTargetHost, auditTargetPath,
			r.Method, auditStatus, auditDecide, auditOutcome, auditReason,
			time.Since(start))
	}()

	agent := middleware.AgentFromContext(r.Context())
	if agent == nil {
		auditStatus = http.StatusUnauthorized
		auditDecide = "deny"
		auditOutcome = "unauthorized"
		writeJSONError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing agent token")
		return
	}
	auditAgent = agent

	targetHost := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Clawvisor-Target-Host")))
	if targetHost == "" {
		auditStatus = http.StatusBadRequest
		auditDecide = "deny"
		auditOutcome = "missing_target"
		writeJSONError(w, http.StatusBadRequest, "MISSING_TARGET", "X-Clawvisor-Target-Host header required")
		return
	}
	auditTargetHost = targetHost
	if h.isSelfHost(targetHost) {
		auditStatus = http.StatusForbidden
		auditDecide = "deny"
		auditOutcome = "self_target"
		writeJSONError(w, http.StatusForbidden, "SELF_TARGET", "target host points at the proxy itself")
		return
	}
	if err := h.checkSSRF(r.Context(), targetHost); err != nil {
		auditStatus = http.StatusForbidden
		auditDecide = "deny"
		auditOutcome = "ssrf_blocked"
		auditReason = err.Error()
		h.Logger.WarnContext(r.Context(), "lite-proxy resolver: blocked target host",
			"host", targetHost, "err", err.Error())
		writeJSONError(w, http.StatusForbidden, "SSRF_BLOCKED", "target host is not allowed")
		return
	}

	// Build the upstream URL. The path after `/api/proxy/` is the upstream
	// request path; query string is preserved.
	upstreamPath := strings.TrimPrefix(r.URL.Path, "/api/proxy")
	if upstreamPath == "" {
		upstreamPath = "/"
	}
	upstreamURL := &url.URL{
		Scheme:   "https",
		Host:     targetHost,
		Path:     upstreamPath,
		RawQuery: r.URL.RawQuery,
	}
	auditTargetPath = upstreamPath

	// Read the inbound body in full so we can replay it verbatim.
	// (Body-embedded placeholder mutation is Phase 4.)
	body, err := readLimited(r.Body, h.MaxRequestBytes)
	if err != nil {
		auditStatus = http.StatusRequestEntityTooLarge
		auditDecide = "deny"
		auditOutcome = "request_too_large"
		auditReason = err.Error()
		writeJSONError(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", err.Error())
		return
	}

	// Build outbound headers, swapping any header values that contain a
	// shadow placeholder. The target host is the authoritative input to
	// the bound-service host check: every placeholder swapped must allow
	// `targetHost`.
	outHeaders, replacedPlaceholders, err := h.swapHeaderPlaceholders(r, agent, targetHost)
	if err != nil {
		var apiErr *resolverAPIError
		if errors.As(err, &apiErr) {
			auditStatus = apiErr.status
			auditDecide = "deny"
			auditOutcome = strings.ToLower(apiErr.code)
			auditReason = apiErr.msg
			writeJSONError(w, apiErr.status, apiErr.code, apiErr.msg)
			return
		}
		h.Logger.WarnContext(r.Context(), "lite-proxy resolver: header swap failed",
			"agent_id", agent.ID, "err", err.Error())
		auditStatus = http.StatusInternalServerError
		auditDecide = "deny"
		auditOutcome = "swap_error"
		auditReason = err.Error()
		writeJSONError(w, http.StatusInternalServerError, "SWAP_ERROR", "credential swap failed")
		return
	}
	if len(replacedPlaceholders) == 0 {
		auditStatus = http.StatusBadRequest
		auditDecide = "deny"
		auditOutcome = "no_placeholder"
		writeJSONError(w, http.StatusBadRequest, "NO_PLACEHOLDER", "no autovault placeholder found in headers")
		return
	}
	if len(replacedPlaceholders) > 0 {
		auditPlaceholder = replacedPlaceholders[0]
		// Look up the bound service for the placeholder, for audit.
		if ph, lerr := h.Store.GetRuntimePlaceholder(r.Context(), auditPlaceholder); lerr == nil {
			auditService = ph.ServiceID
		}
	}

	// Build and send the upstream request.
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(),
		bytes.NewReader(body))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "FORWARD_ERROR", err.Error())
		return
	}
	upstreamReq.Header = outHeaders
	upstreamReq.Header.Del("X-Clawvisor-Target-Host")
	upstreamReq.Host = targetHost

	// The resolver IS a proxy — its purpose is to forward
	// model-emitted URLs after validating them upstream. SSRF
	// defenses applied before this point: checkSSRF rejects private
	// / loopback hosts at the application layer; safeDialContext
	// re-resolves and re-validates at dial time (closing TOCTOU);
	// isSelfHost rejects loops back to this daemon; the placeholder
	// bound-host allowlist (inspector/boundary.go) constrains target
	// to the credential's authorized services; the redirect handler
	// refuses 3xx so credentials can't be replayed elsewhere; and
	// the forwarded-header strip prevents inbound source-IP /
	// vhost-routing metadata from reaching upstream. CodeQL's
	// reachability analysis correctly traces user input to Client.Do
	// but cannot see those defenses.
	// codeql[go/request-forgery] Proxy resolver upstream requests are constrained by the handler's configured upstream URL policy.
	resp, err := h.Client.Do(upstreamReq)
	if err != nil {
		h.Logger.WarnContext(r.Context(), "lite-proxy resolver: upstream call failed",
			"agent_id", agent.ID, "host", targetHost, "err", err.Error())
		auditStatus = http.StatusBadGateway
		auditDecide = "deny"
		auditOutcome = "upstream_error"
		auditReason = err.Error()
		writeJSONError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "upstream request failed")
		return
	}
	defer resp.Body.Close()
	auditStatus = resp.StatusCode
	auditOutcome = outcomeFromStatus(resp.StatusCode)

	for name, values := range resp.Header {
		switch http.CanonicalHeaderKey(name) {
		case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
			"Te", "Trailer", "Transfer-Encoding", "Upgrade":
			continue
		}
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			return
		}
		if readErr != nil {
			return
		}
	}
}

// resolverAPIError is an internal sentinel that swap routes can throw when
// they want a specific HTTP status returned. Carrying it through error
// values keeps the swap function's signature simple.
type resolverAPIError struct {
	status int
	code   string
	msg    string
}

func (e *resolverAPIError) Error() string {
	return fmt.Sprintf("%s: %s", e.code, e.msg)
}

// swapHeaderPlaceholders walks the inbound request's headers, replaces any
// `autovault_…` substring with the corresponding vault credential, and
// returns the resulting header map plus the list of placeholders that
// were replaced (useful for audit + a "no placeholder => 400" guard).
//
// Ownership is enforced: a placeholder must belong to the same UserID +
// AgentID as the agent authenticating the request. Mismatch throws a
// 403 via resolverAPIError.
//
// Bound-service host check: every placeholder's `ServiceID` produces an
// allowed-host set; the resolver's `targetHost` argument MUST be in that
// set. Mismatch fails closed.
//
// Headers that contained the caller-auth (the agent token in
// Authorization / x-api-key / X-Clawvisor-Caller) are stripped from
// outbound — they are FOR US, not the upstream service. Headers that
// carried a placeholder are forwarded with the swap applied.
//
// Caller-auth detection is value-shaped: a header value that begins with
// `cvis_` (the agent token prefix) and does NOT contain an `autovault_…`
// substring is treated as caller-auth. This means a user can cleanly send
// `Authorization: Bearer cvis_…` for caller-auth AND
// `X-API-Key: autovault_…` for the placeholder swap in the same request.
func (h *ProxyResolverHandler) swapHeaderPlaceholders(r *http.Request, agent *store.Agent, targetHost string) (http.Header, []string, error) {
	out := make(http.Header, len(r.Header))
	var allReplaced []string

	resolve := func(placeholder string) (string, error) {
		ph, err := h.Store.GetRuntimePlaceholder(r.Context(), placeholder)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return "", &resolverAPIError{
					status: http.StatusUnauthorized,
					code:   "UNKNOWN_PLACEHOLDER",
					msg:    "placeholder not registered",
				}
			}
			return "", err
		}
		if ph.UserID != agent.UserID || (ph.AgentID != "" && ph.AgentID != agent.ID) {
			return "", &resolverAPIError{
				status: http.StatusForbidden,
				code:   "PLACEHOLDER_OWNERSHIP",
				msg:    "placeholder does not belong to the calling agent",
			}
		}
		now := time.Now().UTC()
		if reason, ok := llmproxy.ValidateRuntimePlaceholderAccess(r.Context(), h.Store, ph, agent.UserID, agent.ID, now); !ok {
			code := "PLACEHOLDER_REJECTED"
			status := http.StatusForbidden
			if strings.Contains(reason, "revoked") {
				code = "PLACEHOLDER_REVOKED"
				status = http.StatusUnauthorized
			} else if strings.Contains(reason, "expired") {
				code = "PLACEHOLDER_EXPIRED"
				status = http.StatusUnauthorized
			}
			return "", &resolverAPIError{
				status: status,
				code:   code,
				msg:    reason,
			}
		}
		// Bound-service host check.
		hosts, boundReason := llmproxy.RuntimePlaceholderBoundHosts(r.Context(), h.Store, ph)
		if len(hosts) == 0 {
			return "", &resolverAPIError{
				status: http.StatusForbidden,
				code:   "TARGET_HOST_NOT_BOUND",
				msg:    "target host not in placeholder's bound-service allowlist: " + boundReason,
			}
		}
		// Strip port for allowlist comparison; preserve the original
		// host:port for the upstream dial. Allowlist entries are
		// hostnames (e.g. "api.github.com"), so targetHost like
		// "api.github.com:443" must compare as "api.github.com".
		hostOnly := targetHost
		if h, _, err := net.SplitHostPort(targetHost); err == nil {
			hostOnly = h
		}
		if ok, reason := inspector.BoundaryCheck(inspector.Verdict{IsAPICall: true, Host: hostOnly}, hosts); !ok {
			return "", &resolverAPIError{
				status: http.StatusForbidden,
				code:   "TARGET_HOST_NOT_BOUND",
				msg:    "target host not in placeholder's bound-service allowlist: " + reason,
			}
		}
		vaultLookupKey := ph.ServiceID
		if ph.CredentialGrantID != "" {
			if auth, authErr := h.Store.GetCredentialAuthorization(r.Context(), ph.CredentialGrantID); authErr == nil && strings.TrimSpace(auth.CredentialRef) != "" {
				vaultLookupKey = strings.TrimSpace(auth.CredentialRef)
			}
		}
		raw, err := h.Vault.Get(r.Context(), ph.UserID, vaultLookupKey)
		if err != nil {
			if errors.Is(err, vault.ErrNotFound) {
				return "", &resolverAPIError{
					status: http.StatusUnauthorized,
					code:   "VAULT_MISS",
					msg:    "no vault credential bound to placeholder",
				}
			}
			return "", err
		}
		extracted, err := h.extractRuntimeCredentialValue(r.Context(), ph, raw)
		if err != nil {
			return "", err
		}
		go func(id string) {
			// Fire-and-forget: detach cancellation but cap so a stuck DB
			// can't leak goroutines forever. Recover from panics so an
			// unexpected store impl bug doesn't crash the process.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
			defer cancel()
			defer func() {
				if rec := recover(); rec != nil {
					h.Logger.ErrorContext(ctx, "lite-proxy resolver: touch placeholder panicked",
						"placeholder", id, "recover", fmt.Sprintf("%v", rec))
				}
			}()
			if err := h.Store.TouchRuntimePlaceholder(ctx, id, time.Now().UTC()); err != nil {
				h.Logger.WarnContext(ctx, "lite-proxy resolver: touch placeholder failed",
					"placeholder", id, "err", err.Error())
			}
		}(placeholder)
		return extracted, nil
	}

	connectionDrop := resolverConnectionScopedHeaders(r.Header)
	for name, values := range r.Header {
		canonical := http.CanonicalHeaderKey(name)
		// Strip every X-Clawvisor-* private header from the outbound
		// request — they're for our edge, not the upstream service.
		if strings.HasPrefix(canonical, "X-Clawvisor-") {
			continue
		}
		if _, skip := resolverHopByHopHeaders[canonical]; skip {
			continue
		}
		if _, skip := resolverDropForwardedHeaders[canonical]; skip {
			// Forwarded / X-Forwarded-* / X-Real-IP / Via never propagate
			// to the upstream — the resolver is a confused-deputy boundary
			// and these can carry attacker-influenced metadata that some
			// backends trust for IP allowlists or vhost routing.
			continue
		}
		if _, skip := connectionDrop[strings.ToLower(canonical)]; skip {
			continue
		}
		swapped := make([]string, 0, len(values))
		dropAll := false
		for _, v := range values {
			containsShadow := autovault.HeaderMaybeContainsShadow(v)
			// Caller-auth detection: cvis_… token without a placeholder.
			// Strip the entire header in that case.
			if !containsShadow && looksLikeCallerAuthValue(v) {
				dropAll = true
				break
			}
			if !containsShadow {
				swapped = append(swapped, v)
				continue
			}
			replaced, hits, err := autovault.ReplaceHeaderValue(v, resolve)
			if err != nil {
				return nil, nil, err
			}
			swapped = append(swapped, replaced)
			allReplaced = append(allReplaced, hits...)
		}
		if dropAll {
			continue
		}
		out[canonical] = swapped
	}
	return out, allReplaced, nil
}

type oauthCredentialForResolver struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
}

func (h *ProxyResolverHandler) extractRuntimeCredentialValue(ctx context.Context, ph *store.RuntimePlaceholder, raw []byte) (string, error) {
	if token, ok, err := h.oauthAccessTokenForRuntimeCredential(ctx, ph, raw); ok || err != nil {
		if err != nil {
			return "", err
		}
		return token, nil
	}
	extracted, err := autovault.ExtractCredentialValue(raw)
	if err != nil {
		return "", fmt.Errorf("extract credential: %w", err)
	}
	return extracted, nil
}

func (h *ProxyResolverHandler) oauthAccessTokenForRuntimeCredential(ctx context.Context, ph *store.RuntimePlaceholder, raw []byte) (string, bool, error) {
	if h.AdapterReg == nil || ph == nil {
		return "", false, nil
	}
	serviceID, _ := splitServiceScopedVaultItemID(ph.ServiceID)
	if serviceID == "" {
		return "", false, nil
	}
	adapter, ok := h.AdapterReg.GetForUser(ctx, serviceID, ph.UserID)
	if !ok {
		return "", false, nil
	}
	oauthCfg := adapter.OAuthConfig()
	if oauthCfg == nil {
		return "", false, nil
	}
	var stored oauthCredentialForResolver
	if err := json.Unmarshal(raw, &stored); err != nil {
		return "", false, nil
	}
	if stored.AccessToken == "" && stored.RefreshToken == "" {
		return "", false, nil
	}
	token, err := oauthCfg.TokenSource(ctx, &oauth2.Token{
		AccessToken:  stored.AccessToken,
		RefreshToken: stored.RefreshToken,
		Expiry:       stored.Expiry,
		TokenType:    "Bearer",
	}).Token()
	if err != nil {
		return "", true, &resolverAPIError{
			status: http.StatusUnauthorized,
			code:   "OAUTH_REFRESH_FAILED",
			msg:    "could not refresh OAuth credential",
		}
	}
	if token.AccessToken == "" {
		return "", true, &resolverAPIError{
			status: http.StatusUnauthorized,
			code:   "OAUTH_REFRESH_FAILED",
			msg:    "OAuth credential did not return an access token",
		}
	}
	return token.AccessToken, true, nil
}

// resolverHopByHopHeaders is the canonical set of hop-by-hop headers
// that must not be forwarded to the upstream service. Matches the set
// already stripped from upstream responses in Forward.
var resolverHopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// resolverDropForwardedHeaders enumerates request headers that must
// never reach the upstream. They carry source-IP / vhost metadata that
// the model — and the agent's harness — can influence by setting them
// in the rewritten tool_use. Trusted by some backends for allowlists
// or routing, so passing them through turns the resolver into an
// arbitrary-header injection vector.
var resolverDropForwardedHeaders = map[string]struct{}{
	"Forwarded":         {},
	"X-Forwarded-For":   {},
	"X-Forwarded-Host":  {},
	"X-Forwarded-Proto": {},
	"X-Forwarded-Port":  {},
	"X-Forwarded-Ssl":   {},
	"X-Real-Ip":         {},
	"Via":               {},
}

// resolverConnectionScopedHeaders returns the lowercased header names
// listed in the inbound Connection header(s). RFC 7230 designates these
// as hop-by-hop and they must not be forwarded.
func resolverConnectionScopedHeaders(src http.Header) map[string]struct{} {
	values := src.Values("Connection")
	if len(values) == 0 {
		return nil
	}
	out := map[string]struct{}{}
	for _, v := range values {
		for _, token := range strings.Split(v, ",") {
			token = strings.ToLower(strings.TrimSpace(token))
			if token == "" || token == "close" || token == "keep-alive" {
				continue
			}
			out[token] = struct{}{}
		}
	}
	return out
}

// looksLikeCallerAuthValue reports whether a header value carries the
// Clawvisor agent caller-auth token. We strip those before forwarding so
// a third-party upstream never sees the cvis_ token.
func looksLikeCallerAuthValue(v string) bool {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "cvis_") {
		return true
	}
	// Bearer cvis_ / Token cvis_ / etc.
	if i := strings.IndexByte(v, ' '); i > 0 {
		return strings.HasPrefix(strings.TrimSpace(v[i+1:]), "cvis_")
	}
	return false
}

// isSelfHost reports whether host is one of this deployment's own
// hostnames. Used to refuse target-host values that would loop back
// through Clawvisor itself. Strips an optional :port suffix before
// comparing so `clawvisor.example:443` doesn't slip past the check.
func (h *ProxyResolverHandler) isSelfHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return true
	}
	if hostOnly, _, err := net.SplitHostPort(host); err == nil {
		host = hostOnly
	}
	host = strings.TrimSuffix(host, ".")
	for _, self := range h.SelfHostnames {
		self = strings.TrimSpace(strings.ToLower(self))
		if hostOnly, _, err := net.SplitHostPort(self); err == nil {
			self = hostOnly
		}
		self = strings.TrimSuffix(self, ".")
		if self == host {
			return true
		}
	}
	return false
}

// checkSSRF guards against RFC1918 / loopback / link-local destinations
// unless AllowPrivateNetworks is set. Resolves DNS once via the
// request's context so a slow/hostile resolver can't pin the goroutine
// forever.
func (h *ProxyResolverHandler) checkSSRF(ctx context.Context, host string) error {
	if h.AllowPrivateNetworks {
		return nil
	}
	hostOnly := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostOnly = h
	}
	if ip := net.ParseIP(hostOnly); ip != nil {
		if isPrivateIP(ip) {
			return errors.New("target host resolves to private IP")
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupIP(ctx, "ip", hostOnly)
	if err != nil {
		return fmt.Errorf("resolve target host: %w", err)
	}
	for _, ip := range addrs {
		if isPrivateIP(ip) {
			return errors.New("target host resolves to private IP")
		}
	}
	return nil
}

func isPrivateIP(ip net.IP) bool {
	cgnat := &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
	return ip.IsLoopback() || ip.IsPrivate() || cgnat.Contains(ip) || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast()
}
