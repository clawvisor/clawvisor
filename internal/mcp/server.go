package mcp

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/clawvisor/clawvisor/pkg/store"
)

// Server handles MCP JSON-RPC 2.0 requests.
type Server struct {
	sessions *SessionStore
	handlers map[string]http.Handler // existing HTTP handlers, keyed by route
	logger   *slog.Logger
}

// NewServer creates an MCP server.
// handlers maps internal route patterns to their HTTP handlers (e.g. "GET /api/skill/catalog").
func NewServer(st store.Store, sessionTTL time.Duration, handlers map[string]http.Handler, logger *slog.Logger) *Server {
	return &Server{
		sessions: NewSessionStore(st, sessionTTL, logger),
		handlers: handlers,
		logger:   logger,
	}
}

// StartCleanup launches the background session cleanup goroutine.
func (s *Server) StartCleanup(done <-chan struct{}) {
	go s.sessions.RunCleanup(done)
}

// HandleJSONRPC dispatches a parsed JSON-RPC request and returns a response.
// w and r are the original HTTP request/response (used for session headers and tool execution).
func (s *Server) HandleJSONRPC(w http.ResponseWriter, r *http.Request, req *Request) *Response {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(w, r, req)
	case "notifications/initialized":
		// Notification — validate session but no response either way.
		sessionID := r.Header.Get("Mcp-Session-Id")
		if !s.sessions.Valid(r.Context(), sessionID) {
			s.logger.Warn("notifications/initialized with invalid session", "session_id", sessionID)
		}
		return nil
	case "ping", "tools/list", "tools/call", "prompts/list", "prompts/get":
		sessionID := r.Header.Get("Mcp-Session-Id")
		if !s.sessions.Valid(r.Context(), sessionID) {
			return newErrorResponse(req.ID, CodeInvalidRequest, "invalid or expired session")
		}
		switch req.Method {
		case "ping":
			return newResponse(req.ID, map[string]any{})
		case "tools/list":
			return s.handleToolsList(req)
		case "tools/call":
			return s.handleToolsCall(r, req)
		case "prompts/list":
			return s.handlePromptsList(req)
		case "prompts/get":
			return s.handlePromptsGet(req)
		}
		return nil // unreachable
	default:
		return newErrorResponse(req.ID, CodeMethodNotFound, "method not found: "+req.Method)
	}
}

func (s *Server) handleInitialize(w http.ResponseWriter, r *http.Request, req *Request) *Response {
	sessionID, err := s.sessions.Create(r.Context())
	if err != nil {
		return newErrorResponse(req.ID, CodeInternalError, "failed to create session")
	}

	w.Header().Set("Mcp-Session-Id", sessionID)

	return newResponse(req.ID, map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities": map[string]any{
			"tools":   map[string]any{},
			"prompts": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "clawvisor",
			"version": "1.0.0",
		},
		"instructions": workflowPrompt,
	})
}

func (s *Server) handleToolsList(req *Request) *Response {
	return newResponse(req.ID, map[string]any{
		"tools": toolDefs(),
	})
}

func (s *Server) handlePromptsList(req *Request) *Response {
	return newResponse(req.ID, map[string]any{
		"prompts": promptDefs(),
	})
}

func (s *Server) handlePromptsGet(req *Request) *Response {
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return newErrorResponse(req.ID, CodeInvalidParams, "invalid prompt params")
	}

	description, messages, ok := promptContent(params.Name)
	if !ok {
		return newErrorResponse(req.ID, CodeInvalidParams, "unknown prompt: "+params.Name)
	}

	return newResponse(req.ID, map[string]any{
		"description": description,
		"messages":    messages,
	})
}

func (s *Server) handleToolsCall(r *http.Request, req *Request) *Response {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return newErrorResponse(req.ID, CodeInvalidParams, "invalid tool call params")
	}

	result, err := executeTool(r, params.Name, params.Arguments, s.handlers, s.logger)
	if err != nil {
		return newResponse(req.ID, ToolResult{
			Content: []ToolContent{{Type: "text", Text: err.Error()}},
			IsError: true,
		})
	}

	return newResponse(req.ID, result)
}
