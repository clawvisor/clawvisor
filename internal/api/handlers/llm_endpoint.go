package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// LLMEndpointHandler is the lite-proxy LLM termination point. It accepts
// Anthropic-/OpenAI-shaped requests authenticated by the agent's existing
// `cvis_…` token (carried in either Authorization or x-api-key), fetches
// the real upstream API key from the vault under (user_id, "anthropic" |
// "openai"), and proxies the response back. v1 is pure passthrough —
// inspector and rewriter layer in via the response-body wrap path in
// subsequent files.
type LLMEndpointHandler struct {
	Store     store.Store
	Vault     vault.Vault
	Forwarder *llmproxy.Forwarder
	Parsers   *conversation.Registry
	Logger    *slog.Logger

	// Inspector enables tool_use rewriting on the response leg. When nil,
	// the handler runs in pure passthrough mode (no inspection).
	Inspector *inspector.Inspector

	// ResolverBaseURL is the URL the rewriter redirects credentialed
	// tool_uses through (e.g. https://clawvisor.example/proxy/v1). Empty
	// disables rewriting even when Inspector is set.
	ResolverBaseURL string

	// MaxRequestBytes caps the inbound request body. Defaults to 4 MiB.
	MaxRequestBytes int64

	// MaxResponseBytes caps the upstream response body when buffering for
	// inspection. Default 32 MiB. Exceeding this returns 502
	// UPSTREAM_TOO_LARGE.
	MaxResponseBytes int64
}

// NewLLMEndpointHandler builds the handler with sensible defaults.
func NewLLMEndpointHandler(st store.Store, v vault.Vault, logger *slog.Logger) *LLMEndpointHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &LLMEndpointHandler{
		Store:           st,
		Vault:           v,
		Forwarder:       llmproxy.NewForwarder(v),
		Parsers:         conversation.DefaultRegistry(),
		Logger:          logger,
		MaxRequestBytes: 4 << 20,
	}
}

// Messages handles `POST /v1/messages` (Anthropic) and `POST
// /v1/messages/count_tokens`. The route-selected parser dispatches to the
// Anthropic parser regardless of the inbound Host header.
func (h *LLMEndpointHandler) Messages(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r)
}

// ChatCompletions handles `POST /v1/chat/completions` (OpenAI Chat API).
func (h *LLMEndpointHandler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r)
}

// Responses handles `POST /v1/responses` (OpenAI Responses API).
func (h *LLMEndpointHandler) Responses(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r)
}

func (h *LLMEndpointHandler) serve(w http.ResponseWriter, r *http.Request) {
	agent := middleware.AgentFromContext(r.Context())
	if agent == nil {
		// Middleware should have rejected this; defense-in-depth.
		writeJSONError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing agent token")
		return
	}

	parser := h.Parsers.ParserForRoute(r.URL.Path)
	if parser == nil {
		writeJSONError(w, http.StatusNotFound, "NOT_FOUND", "unsupported route")
		return
	}
	provider := parser.Name()

	// Read the inbound body in full. v1 doesn't stream the request side
	// (Anthropic/OpenAI don't either; bodies are bounded by tokens-of-context).
	body, err := readLimited(r.Body, h.MaxRequestBytes)
	if err != nil {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", err.Error())
		return
	}

	// Validate that the body parses for the selected provider. Surfaces
	// schema errors as a 400 before we burn an upstream call.
	if _, err := parser.ParseRequest(body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "MALFORMED_REQUEST", err.Error())
		return
	}

	resp, err := h.Forwarder.Forward(r.Context(), agent.UserID, provider, r, body)
	if err != nil {
		if isVaultMiss(err) {
			writeJSONError(w, http.StatusUnauthorized, "UPSTREAM_KEY_MISSING",
				"no upstream API key configured in vault for this provider")
			return
		}
		h.Logger.WarnContext(r.Context(), "lite-proxy forward failed",
			"agent_id", agent.ID, "provider", string(provider), "err", err.Error())
		writeJSONError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "upstream request failed")
		return
	}
	defer resp.Body.Close()

	// Mirror upstream status + headers. Strip hop-by-hop. We rewrite
	// Content-Length below if postprocess mutates the body.
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

	upstreamCT := resp.Header.Get("Content-Type")

	// Postprocess runs when we have an inspector + resolver URL. SSE
	// responses are buffered whole-turn before re-emitting; JSON
	// responses are parsed-and-mutated. Streaming-while-rewriting is a
	// future optimization — the harness sees a correct SSE turn either way.
	if h.Inspector != nil && h.ResolverBaseURL != "" {
		full, readErr := readResponseLimited(resp.Body, h.MaxResponseBytes)
		if readErr != nil {
			h.Logger.WarnContext(r.Context(), "lite-proxy upstream read error",
				"agent_id", agent.ID, "err", readErr.Error())
			writeJSONError(w, http.StatusBadGateway, "UPSTREAM_TOO_LARGE", "upstream response exceeded size cap")
			return
		}
		callerToken := middleware.CallerTokenFromContext(r.Context())
		if callerToken == "" {
			// Fallback: extract from inbound headers — the LLM endpoint
			// uses Authorization / x-api-key for the agent's own token,
			// which is exactly the caller-auth the rewriter needs to
			// inject so the harness's outbound resolver call works.
			callerToken = inboundAgentToken(r)
		}
		opts := inspector.DefaultRewriteOpts(h.ResolverBaseURL)
		opts.CallerToken = callerToken

		processed := llmproxy.Postprocess(r, full, upstreamCT, llmproxy.PostprocessConfig{
			Inspector:   h.Inspector,
			RewriteOpts: opts,
			Store:       h.Store,
			AgentUserID: agent.UserID,
			AgentID:     agent.ID,
		})
		if processed.Rewritten {
			w.Header().Set("Content-Length", "")
			// Stripping Content-Encoding because we mutated the body
			// after upstream may have compressed it; the harness should
			// not try to gunzip our plaintext.
			w.Header().Del("Content-Encoding")
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(processed.Body)
		return
	}

	w.WriteHeader(resp.StatusCode)

	// Stream the upstream body back unchanged.
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
			h.Logger.WarnContext(r.Context(), "lite-proxy upstream stream error",
				"agent_id", agent.ID, "err", readErr.Error())
			return
		}
	}
}


func isVaultMiss(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, vault.ErrNotFound) {
		return true
	}
	// Forwarder wraps the not-found case in its own error string for user
	// clarity; match on substring as a last resort.
	return false
}

// writeJSONError produces a uniform JSON error response.
func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": message,
		"code":  code,
	})
}

// readLimited reads at most max bytes from r. Returns an error if the body
// exceeds max.
func readLimited(r io.Reader, max int64) ([]byte, error) {
	limited := io.LimitReader(r, max+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > max {
		return nil, errors.New("request body too large")
	}
	return buf, nil
}

// readResponseLimited mirrors readLimited for upstream responses. Default
// max applies when 0 is passed (32 MiB).
func readResponseLimited(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		max = 32 << 20
	}
	return readLimited(r, max)
}

// inboundAgentToken extracts the cvis_… token from the inbound request's
// Authorization or x-api-key header (the SDK's natural caller-auth slot
// at the LLM endpoint). Used as a fallback to source the caller token
// for the rewriter when no dedicated middleware ran.
func inboundAgentToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		token := strings.TrimSpace(h[len("Bearer "):])
		if strings.HasPrefix(token, "cvis_") {
			return token
		}
	}
	if h := strings.TrimSpace(r.Header.Get("x-api-key")); strings.HasPrefix(h, "cvis_") {
		return h
	}
	return ""
}
