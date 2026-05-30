package lite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	runtimeautovault "github.com/clawvisor/clawvisor/internal/runtime/autovault"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// newLiteResolver is the lite harness's stand-in for handlers.ProxyResolverHandler.
//
// What it does:
//   - Reads X-Clawvisor-Target-Host (the upstream host the rewriter pulled
//     off the agent's curl URL).
//   - Substitutes any `autovault_*` placeholder in every outbound header
//     with the real secret looked up via store.GetRuntimePlaceholder →
//     vault.Get.
//   - Forwards the request to http://<target-host><path?query> (HTTP, not
//     HTTPS — the production resolver hardcodes HTTPS, which doesn't
//     match our httptest mock upstream).
//   - Streams the response body back.
//
// Outbound dials are redirected to the mock upstream's address: when an
// agent curls https://api.github.com/repos/..., the inspector's
// bound-service check sees `api.github.com` and allows it natively; the
// rewritten curl reaches this resolver, which then dials the mock
// upstream instead of the real internet. The Host header still says
// `api.github.com`, so the mock receives a request shaped like the real
// production call without the harness having to subvert the
// bound-service allowlist.
//
// What it deliberately skips (production parity is not the goal here):
//   - SSRF / private-network preflight (the mock upstream IS private).
//   - Audit emission.
//   - Body-embedded placeholder mutation (the production resolver doesn't
//     do this yet either — Phase 4 in its docstring).
//   - Self-host loop refusal (no production deployment in test).
//
// The agent must be present on the request context (placed there by the
// nonce middleware); otherwise we return 401 to match production shape.
func newLiteResolver(st store.Store, v vault.Vault, logger *slog.Logger, mockUpstreamAddr string) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	// Custom transport: every outbound dial is redirected to the mock
	// upstream's address. The dial-time redirect happens after the
	// proxy's bound-service check has already accepted the agent's
	// target host (e.g. api.github.com), so the agent practices the
	// production URL shape without our test code having to extend
	// the production allowlist.
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, mockUpstreamAddr)
		},
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent := middleware.AgentFromContext(r.Context())
		if agent == nil {
			writeResolverError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing agent token")
			return
		}
		targetHost := strings.TrimSpace(r.Header.Get("X-Clawvisor-Target-Host"))
		if targetHost == "" {
			writeResolverError(w, http.StatusBadRequest, "MISSING_TARGET", "X-Clawvisor-Target-Host header required")
			return
		}
		upstreamPath := strings.TrimPrefix(r.URL.Path, "/api/proxy")
		if upstreamPath == "" {
			upstreamPath = "/"
		}
		upstreamURL := &url.URL{
			Scheme:   "http",
			Host:     targetHost,
			Path:     upstreamPath,
			RawQuery: r.URL.RawQuery,
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 34<<20))
		if err != nil {
			writeResolverError(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", err.Error())
			return
		}

		outHeaders, swapped, err := swapHeaderPlaceholders(r.Context(), st, v, agent.UserID, r.Header)
		if err != nil {
			logger.WarnContext(r.Context(), "lite resolver: header swap failed",
				"agent_id", agent.ID, "err", err.Error())
			writeResolverError(w, http.StatusInternalServerError, "SWAP_ERROR", "credential swap failed")
			return
		}
		if !swapped {
			writeResolverError(w, http.StatusBadRequest, "NO_PLACEHOLDER", "no autovault placeholder found in headers")
			return
		}

		upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), bytes.NewReader(body))
		if err != nil {
			writeResolverError(w, http.StatusInternalServerError, "FORWARD_ERROR", err.Error())
			return
		}
		upstreamReq.Header = outHeaders
		upstreamReq.Header.Del("X-Clawvisor-Target-Host")
		upstreamReq.Header.Del("X-Clawvisor-Caller")
		upstreamReq.Host = targetHost

		resp, err := client.Do(upstreamReq)
		if err != nil {
			logger.WarnContext(r.Context(), "lite resolver: upstream call failed",
				"agent_id", agent.ID, "host", targetHost, "err", err.Error())
			writeResolverError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "upstream request failed")
			return
		}
		defer resp.Body.Close()

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
		_, _ = io.Copy(w, resp.Body)
	})
}

// swapHeaderPlaceholders walks every header value, finds autovault_*
// substrings, and substitutes each with the real vault secret. Returns
// (newHeader, anySwapped, err). Lookup failures (placeholder not in
// store, vault read error) are bubbled up so the caller can 500.
func swapHeaderPlaceholders(ctx context.Context, st store.Store, v vault.Vault, userID string, in http.Header) (http.Header, bool, error) {
	out := make(http.Header, len(in))
	swapped := false
	for name, values := range in {
		if strings.EqualFold(name, "Proxy-Authorization") {
			continue
		}
		newValues := make([]string, len(values))
		for i, value := range values {
			replaced, placeholders, err := runtimeautovault.ReplaceHeaderValue(value, func(placeholder string) (string, error) {
				ph, err := st.GetRuntimePlaceholder(ctx, placeholder)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "", errPlaceholderNotRegistered
					}
					return "", err
				}
				// VaultItemID is the planted vault entry id (e.g.
				// "github:personal") which is also the vault storage
				// key in our lite memoryVault.
				secret, err := v.Get(ctx, userID, ph.VaultItemID)
				if err != nil {
					return "", err
				}
				return string(secret), nil
			})
			if err != nil {
				return nil, false, err
			}
			if len(placeholders) > 0 {
				swapped = true
			}
			newValues[i] = replaced
		}
		out[name] = newValues
	}
	return out, swapped, nil
}

var errPlaceholderNotRegistered = errors.New("placeholder not registered")

func writeResolverError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "error": msg})
}
