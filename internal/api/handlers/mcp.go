package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/mcp"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// MCPHandler handles POST /mcp — the MCP JSON-RPC 2.0 endpoint.
// It performs agent auth inline (not via middleware) so it can return
// the WWW-Authenticate header that MCP OAuth discovery requires.
type MCPHandler struct {
	server  *mcp.Server
	st      store.Store
	baseURL string
}

// NewMCPHandler creates an MCP handler.
func NewMCPHandler(server *mcp.Server, st store.Store, baseURL string) *MCPHandler {
	return &MCPHandler{server: server, st: st, baseURL: baseURL}
}

// Handle processes an MCP JSON-RPC request.
func (h *MCPHandler) Handle(w http.ResponseWriter, r *http.Request) {
	// Authenticate agent inline — we need to set WWW-Authenticate on 401.
	token := bearerToken(r)
	if token == "" {
		h.writeUnauthorized(w)
		return
	}

	hash := auth.HashToken(token)
	agent, err := h.st.GetAgentByToken(r.Context(), hash)
	if err != nil {
		h.writeUnauthorized(w)
		return
	}

	// Inject agent into context (same as requireAgent middleware).
	ctx := context.WithValue(r.Context(), middleware.AgentContextKey, agent)
	r = r.WithContext(ctx)

	var req mcp.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(mcp.Response{
			JSONRPC: "2.0",
			Error:   &mcp.ErrorObject{Code: mcp.CodeParseError, Message: "invalid JSON"},
		})
		return
	}

	if req.JSONRPC != "2.0" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(mcp.Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &mcp.ErrorObject{Code: mcp.CodeInvalidRequest, Message: "jsonrpc must be 2.0"},
		})
		return
	}

	resp := h.server.HandleJSONRPC(w, r, &req)

	// Notifications don't get a response.
	if resp == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// HandleSSE handles GET /mcp — the SSE transport entry point.
// Returns 401 with OAuth discovery for unauthenticated requests so that
// mcp-remote (and other MCP clients) fall through to OAuth instead of
// mistakenly connecting to the SPA catch-all.
func (h *MCPHandler) HandleSSE(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" {
		h.writeUnauthorized(w)
		return
	}
	// SSE streaming not implemented — tell client to use POST.
	http.Error(w, "SSE transport not supported, use POST", http.StatusMethodNotAllowed)
}

// HandleDelete handles DELETE /mcp — session termination.
func (h *MCPHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// writeUnauthorized sends a 401 with the WWW-Authenticate header
// that tells MCP clients where to find the OAuth metadata.
func (h *MCPHandler) writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate",
		`Bearer resource_metadata="`+h.baseURL+`/.well-known/oauth-protected-resource"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	json.NewEncoder(w).Encode(map[string]string{
		"error": "unauthorized",
		"code":  "UNAUTHORIZED",
	})
}

// bearerToken extracts a Bearer token from the Authorization header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
