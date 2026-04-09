package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/adaptergen"
	"github.com/clawvisor/clawvisor/internal/api/middleware"
)

// GeneratorFactory creates a Generator scoped to a specific user.
// For local/single-user mode the userID is ignored; for cloud/multi-user
// mode a per-user DBStore is created.
type GeneratorFactory func(userID string) *adaptergen.Generator

// AdapterGenHandler exposes adapter generation, update, and removal endpoints.
type AdapterGenHandler struct {
	factory GeneratorFactory
	logger  *slog.Logger
}

// NewAdapterGenHandler creates a new handler.
func NewAdapterGenHandler(factory GeneratorFactory, logger *slog.Logger) *AdapterGenHandler {
	return &AdapterGenHandler{factory: factory, logger: logger}
}

// generatorForRequest returns a Generator scoped to the authenticated user.
// Works for both dashboard (user JWT) and MCP (agent token) auth flows.
func (h *AdapterGenHandler) generatorForRequest(r *http.Request) *adaptergen.Generator {
	var userID string
	if u := middleware.UserFromContext(r.Context()); u != nil {
		userID = u.ID
	} else if a := middleware.AgentFromContext(r.Context()); a != nil {
		userID = a.UserID
	}
	return h.factory(userID)
}

// createAdapterRequest is the request body for POST /api/adapters/generate.
type createAdapterRequest struct {
	SourceType    string            `json:"source_type"`                // "mcp", "openapi", "docs"
	Source        string            `json:"source,omitempty"`           // raw content (mutually exclusive with source_url)
	SourceURL     string            `json:"source_url,omitempty"`      // URL to fetch content from
	SourceHeaders map[string]string `json:"source_headers,omitempty"`  // headers to send when fetching source_url (e.g. Authorization)
	ServiceID     string            `json:"service_id,omitempty"`
	AuthType      string            `json:"auth_type,omitempty"`
}

// updateAdapterRequest is the request body for PUT /api/adapters/{service_id}/generate.
type updateAdapterRequest struct {
	SourceType    string            `json:"source_type"`
	Source        string            `json:"source,omitempty"`
	SourceURL     string            `json:"source_url,omitempty"`
	SourceHeaders map[string]string `json:"source_headers,omitempty"`
}

const maxFetchBytes = 2 * 1024 * 1024 // 2 MB limit for fetched specs

// ssrfRanges are private/internal CIDR blocks that must not be targeted.
var ssrfRanges = func() []*net.IPNet {
	cidrs := []string{
		"0.0.0.0/8",     // "this" network (includes 0.0.0.0)
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16", // link-local / cloud metadata
		"127.0.0.0/8",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, n, _ := net.ParseCIDR(cidr)
		nets = append(nets, n)
	}
	return nets
}()

// ssrfSafeClient is an HTTP client that blocks requests to private/internal IPs,
// resolving DNS at dial time to prevent rebinding attacks.
var ssrfSafeClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, fmt.Errorf("invalid address %q: %w", address, err)
			}
			ips, err := net.DefaultResolver.LookupHost(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("cannot resolve host %q: %w", host, err)
			}
			for _, ipStr := range ips {
				ip := net.ParseIP(ipStr)
				if ip == nil {
					continue
				}
				for _, n := range ssrfRanges {
					if n.Contains(ip) {
						return nil, fmt.Errorf("host %q resolves to blocked IP %s", host, ipStr)
					}
				}
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ipStr, port))
			}
			return nil, fmt.Errorf("no safe IPs found for host %q", host)
		},
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	},
}

// fetchSourceURL downloads content from a URL with optional headers. Returns the body as a string.
func fetchSourceURL(ctx context.Context, rawURL string, headers map[string]string) (string, error) {
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return "", fmt.Errorf("source_url must be an HTTP or HTTPS URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("invalid source_url: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ssrfSafeClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching source_url: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("source_url returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return "", fmt.Errorf("reading source_url response: %w", err)
	}
	if len(body) > maxFetchBytes {
		return "", fmt.Errorf("source_url response exceeds 2 MB limit")
	}
	return string(body), nil
}

// Create handles POST /api/adapters/generate — generates a new adapter from source material.
func (h *AdapterGenHandler) Create(w http.ResponseWriter, r *http.Request) {
	// Extend the server's WriteTimeout — generation makes two sequential LLM calls.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(3 * time.Minute))

	var req createAdapterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if req.SourceType == "" {
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": "source_type is required"})
		return
	}

	// Resolve source content: inline or fetched from URL.
	source := req.Source
	if source == "" && req.SourceURL != "" {
		fetched, err := fetchSourceURL(r.Context(), req.SourceURL, req.SourceHeaders)
		if err != nil {
			writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		source = fetched
	}
	if source == "" {
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": "source or source_url is required"})
		return
	}

	src := adaptergen.Source{
		Type:      adaptergen.SourceType(req.SourceType),
		Content:   source,
		ServiceID: req.ServiceID,
		AuthType:  req.AuthType,
	}

	result, err := h.generatorForRequest(r).Generate(r.Context(), src)
	if err != nil {
		h.logger.Warn("adapter generation failed", "err", err)
		writeJSONResponse(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}

	writeJSONResponse(w, http.StatusOK, result)
}

// Update handles PUT /api/adapters/{service_id}/generate — regenerates an existing adapter.
func (h *AdapterGenHandler) Update(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(3 * time.Minute))

	serviceID := r.PathValue("service_id")
	if serviceID == "" {
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": "service_id is required"})
		return
	}

	var req updateAdapterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	source := req.Source
	if source == "" && req.SourceURL != "" {
		fetched, err := fetchSourceURL(r.Context(), req.SourceURL, req.SourceHeaders)
		if err != nil {
			writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		source = fetched
	}
	if source == "" {
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": "source or source_url is required"})
		return
	}

	src := adaptergen.Source{
		Type:    adaptergen.SourceType(req.SourceType),
		Content: source,
	}

	result, err := h.generatorForRequest(r).Update(r.Context(), serviceID, src)
	if err != nil {
		h.logger.Warn("adapter update failed", "service_id", serviceID, "err", err)
		writeJSONResponse(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}

	writeJSONResponse(w, http.StatusOK, result)
}

// Remove handles DELETE /api/adapters/{service_id} — removes an adapter.
func (h *AdapterGenHandler) Remove(w http.ResponseWriter, r *http.Request) {
	serviceID := r.PathValue("service_id")
	if serviceID == "" {
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": "service_id is required"})
		return
	}

	if err := h.generatorForRequest(r).Remove(r.Context(), serviceID); err != nil {
		h.logger.Warn("adapter removal failed", "service_id", serviceID, "err", err)
		writeJSONResponse(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}

	writeJSONResponse(w, http.StatusOK, map[string]string{"status": "removed", "service_id": serviceID})
}

// Install handles POST /api/adapters/install — saves and hot-loads a previously generated adapter.
func (h *AdapterGenHandler) Install(w http.ResponseWriter, r *http.Request) {
	var req struct {
		YAML string `json:"yaml"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.YAML == "" {
		writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": "yaml is required"})
		return
	}

	result, err := h.generatorForRequest(r).Install(r.Context(), req.YAML)
	if err != nil {
		h.logger.Warn("adapter install failed", "err", err)
		writeJSONResponse(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}

	writeJSONResponse(w, http.StatusOK, result)
}

// writeJSONResponse is a helper to write JSON responses for adapter gen endpoints.
func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
