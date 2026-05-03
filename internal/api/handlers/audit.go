package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/google/uuid"
)

// AuditHandler serves the audit log query API.
type AuditHandler struct {
	st store.Store
}

type auditEntryResponse struct {
	*store.AuditEntry
	SummaryText  string `json:"summary_text"`
	ActivityKind string `json:"activity_kind,omitempty"`
	ActionTarget string `json:"action_target,omitempty"`
	Host         string `json:"host,omitempty"`
	Path         string `json:"path,omitempty"`
	Method       string `json:"method,omitempty"`
	ToolName     string `json:"tool_name,omitempty"`
}

func NewAuditHandler(st store.Store) *AuditHandler {
	return &AuditHandler{st: st}
}

// List returns paginated audit log entries for the authenticated user.
//
// GET /api/audit?service=...&outcome=...&data_origin=...&limit=50&offset=0
// Auth: user JWT
func (h *AuditHandler) List(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	q := r.URL.Query()
	limit := parseIntQuery(q.Get("limit"), 50)
	if limit > maxListLimit {
		limit = maxListLimit
	}
	includeRuntime := parseBoolQueryDefault(q.Get("include_runtime"), true)
	filter := store.AuditFilter{
		Service:        q.Get("service"),
		Outcome:        q.Get("outcome"),
		DataOrigin:     q.Get("data_origin"),
		TaskID:         q.Get("task_id"),
		AgentID:        q.Get("agent_id"),
		IncludeRuntime: &includeRuntime,
		Limit:          limit,
		Offset:         parseIntQuery(q.Get("offset"), 0),
	}

	entries, total, err := h.st.ListAuditEntries(r.Context(), user.ID, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list audit entries")
		return
	}
	if entries == nil {
		entries = []*store.AuditEntry{}
	}
	respEntries := make([]*auditEntryResponse, 0, len(entries))
	for _, entry := range entries {
		respEntries = append(respEntries, normalizeAuditEntry(entry))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":   total,
		"entries": respEntries,
	})
}

// Get returns a single audit log entry by ID.
//
// GET /api/audit/{id}
// Auth: user JWT
func (h *AuditHandler) Get(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}

	id := r.PathValue("id")
	entry, err := h.st.GetAuditEntry(r.Context(), id, user.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "audit entry not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not get audit entry")
		return
	}
	writeJSON(w, http.StatusOK, normalizeAuditEntry(entry))
}

// ListMutes returns user-scoped muted activity host filters.
//
// GET /api/audit/mutes
func (h *AuditHandler) ListMutes(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	mutes, err := h.st.ListActivityMutes(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not list activity mutes")
		return
	}
	if mutes == nil {
		mutes = []*store.ActivityMute{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": mutes,
		"total":   len(mutes),
	})
}

// CreateMute stores a user-scoped runtime egress mute for the activity feed.
//
// POST /api/audit/mutes
func (h *AuditHandler) CreateMute(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	var req struct {
		Host       string `json:"host"`
		PathPrefix string `json:"path_prefix"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Host = strings.TrimSpace(req.Host)
	req.PathPrefix = strings.TrimSpace(req.PathPrefix)
	if req.Host == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "host is required")
		return
	}
	if req.PathPrefix != "" && !strings.HasPrefix(req.PathPrefix, "/") {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "path_prefix must start with /")
		return
	}
	mute := &store.ActivityMute{
		ID:         uuid.New().String(),
		UserID:     user.ID,
		Host:       req.Host,
		PathPrefix: req.PathPrefix,
	}
	if err := h.st.CreateActivityMute(r.Context(), mute); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, "CONFLICT", "activity mute already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not create activity mute")
		return
	}
	writeJSON(w, http.StatusCreated, mute)
}

// DeleteMute removes a user-scoped activity mute.
//
// DELETE /api/audit/mutes/{id}
func (h *AuditHandler) DeleteMute(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated")
		return
	}
	id := r.PathValue("id")
	if err := h.st.DeleteActivityMute(r.Context(), id, user.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "activity mute not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not delete activity mute")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

const maxListLimit = 200

// parseIntQuery parses a query string integer, returning defaultVal if missing or invalid.
func parseIntQuery(s string, defaultVal int) int {
	if s == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return defaultVal
	}
	return n
}

func parseBoolQueryDefault(s string, defaultVal bool) bool {
	if s == "" {
		return defaultVal
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return defaultVal
	}
}

func normalizeAuditEntry(entry *store.AuditEntry) *auditEntryResponse {
	resp := &auditEntryResponse{AuditEntry: entry}
	if entry == nil {
		return resp
	}
	params := map[string]any{}
	if len(entry.ParamsSafe) > 0 {
		_ = json.Unmarshal(entry.ParamsSafe, &params)
		params = stripSecretsRecursive(params)
		if body, err := json.Marshal(params); err == nil {
			resp.ParamsSafe = body
		}
	}

	switch entry.Service {
	case "runtime.egress":
		method := normalizeVerb(firstNonEmpty(readString(params["method"]), entry.Action))
		host := readString(params["host"])
		path := readString(params["path"])
		resp.ActivityKind = "runtime_egress"
		resp.Host = host
		resp.Path = path
		resp.Method = method
		resp.ActionTarget = strings.TrimSpace(host + path)
		resp.SummaryText = strings.TrimSpace(strings.Join(compactParts(method, host+path), " "))
	case "runtime.tool_use":
		toolName := firstNonEmpty(readString(params["tool_name"]), entry.Action)
		toolInput, _ := params["tool_input"].(map[string]any)
		target := firstNonEmpty(
			readString(toolInput["url"]),
			readString(toolInput["file_path"]),
			readString(toolInput["path"]),
			readString(toolInput["directory"]),
			readString(toolInput["pattern"]),
			readString(toolInput["command"]),
		)
		resp.ActivityKind = "runtime_tool_use"
		resp.ToolName = toolName
		resp.ActionTarget = target
		resp.SummaryText = strings.TrimSpace(strings.Join(compactParts(toolName, target), " "))
	default:
		if strings.HasPrefix(entry.Service, "runtime.") {
			resp.ActivityKind = "runtime"
		} else {
			resp.ActivityKind = "service"
		}
		resp.SummaryText = strings.TrimSpace(strings.Join(compactParts(entry.Service, entry.Action), " "))
	}

	if resp.SummaryText == "" {
		resp.SummaryText = strings.TrimSpace(strings.Join(compactParts(entry.Service, entry.Action), " "))
	}
	return resp
}

func normalizeVerb(value string) string {
	value = strings.TrimSpace(value)
	switch strings.ToUpper(value) {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return strings.ToUpper(value)
	default:
		return ""
	}
}

func readString(value any) string {
	s, _ := value.(string)
	return s
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func stripSecretsRecursive(value any) map[string]any {
	root, _ := stripSecretsNode(value).(map[string]any)
	if root == nil {
		return map[string]any{}
	}
	return root
}

func stripSecretsNode(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			if looksSecretKey(key) {
				continue
			}
			out[key] = stripSecretsNode(child)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = stripSecretsNode(child)
		}
		return out
	default:
		return value
	}
}

// looksSecretKey reports whether a JSON key likely contains a credential.
//
// The matcher splits the key on separators and camelCase boundaries, then
// matches whole tokens against a small allowlist. This avoids the false
// positives the prior substring matcher produced (e.g. it stripped
// "oauth_endpoint" because it contained "auth", and "author" for the same
// reason). "oauth" alone is not treated as a secret marker — it's the
// protocol name; "oauth_token" still matches because the "token" token does.
func looksSecretKey(key string) bool {
	tokens := tokenizeSecretKey(key)
	for _, t := range tokens {
		switch t {
		case "token", "tokens",
			"secret", "secrets",
			"password", "passwd",
			"credential", "credentials",
			"auth", "authorization",
			"bearer",
			"apikey", "accesskey", "privatekey":
			return true
		}
	}
	for i := 0; i < len(tokens)-1; i++ {
		if tokens[i+1] != "key" {
			continue
		}
		switch tokens[i] {
		case "api", "access", "private":
			return true
		}
	}
	return false
}

// tokenizeSecretKey splits a JSON key into lowercase tokens at separator,
// camelCase, and letter↔digit boundaries so that names like "apiKey",
// "oauth2Token", or "api_v2_token" all surface their semantic words for
// allowlist matching. Without the digit-boundary split, "oauth2Token"
// would collapse to a single token "oauth2token" and bypass the "token"
// match.
func tokenizeSecretKey(key string) []string {
	var tokens []string
	var cur []rune
	var prev rune
	flush := func() {
		if len(cur) > 0 {
			tokens = append(tokens, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	for i, r := range key {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case i > 0 && unicode.IsUpper(r) && unicode.IsLower(prev):
			flush()
			cur = append(cur, r)
		case i > 0 && unicode.IsLetter(r) && unicode.IsDigit(prev),
			i > 0 && unicode.IsDigit(r) && unicode.IsLetter(prev):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
		prev = r
	}
	flush()
	return tokens
}

func compactParts(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}
