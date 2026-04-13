package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
	"github.com/clawvisor/clawvisor/pkg/version"
)

// HealthHandler handles /health and /ready.
type HealthHandler struct {
	st       store.Store
	v        vault.Vault
	authMode string // "magic_link" or "password"
}

func NewHealthHandler(st store.Store, v vault.Vault, authMode string) *HealthHandler {
	return &HealthHandler{st: st, v: v, authMode: authMode}
}

// Health always returns 200 — used by load balancers.
func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Ready checks DB and vault connectivity.
func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	resp := map[string]string{
		"status": "ok",
		"db":     "ok",
		"vault":  "ok",
	}
	code := http.StatusOK

	if err := h.st.Ping(r.Context()); err != nil {
		resp["status"] = "degraded"
		resp["db"] = "error: " + err.Error()
		code = http.StatusServiceUnavailable
	}

	// Vault readiness: attempt to list services for a sentinel user ID.
	// This just exercises the vault connection without requiring real data.
	if _, err := h.v.List(r.Context(), "_health"); err != nil {
		// List returning ErrNotFound or empty is fine — we just want connectivity.
		if err.Error() != vault.ErrNotFound.Error() {
			resp["status"] = "degraded"
			resp["vault"] = "error: " + err.Error()
			code = http.StatusServiceUnavailable
		}
	}

	writeJSON(w, code, resp)
}

// ConfigPublic returns public configuration (no auth required).
func (h *HealthHandler) ConfigPublic(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_mode": h.authMode,
	})
}

// Version returns the current and latest available version (no auth required).
func (h *HealthHandler) Version(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, version.Check())
}

// SkillVersion returns the current skill version and publish date (no auth required).
func (h *HealthHandler) SkillVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"skill_version":      version.Version,
		"skill_published_at": version.SkillPublishedAt,
	})
}

// writeJSON is a shared JSON response helper used across all handlers.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError writes a standard error response.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{
		"error": message,
		"code":  code,
	})
}

// apiErrorDetail is an enriched error response with actionable debugging info.
// Fields beyond error/code are omitted when empty.
type apiErrorDetail struct {
	Error         string         `json:"error"`
	Code          string         `json:"code"`
	Hint          string         `json:"hint,omitempty"`
	MissingFields []string       `json:"missing_fields,omitempty"`
	Example       map[string]any `json:"example,omitempty"`
	Available     []string       `json:"available,omitempty"`
}

// writeDetailedError writes an enriched error response with optional actionable fields.
func writeDetailedError(w http.ResponseWriter, status int, d apiErrorDetail) {
	writeJSON(w, status, d)
}

// maxRequestBodySize is the default limit for JSON request bodies (1 MB).
const maxRequestBodySize = 1 << 20

// decodeJSON decodes the request body into v.
// It enforces a 1 MB body size limit. On failure it writes a detailed error and returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeDetailedError(w, http.StatusBadRequest, diagnoseJSONError(err))
		return false
	}
	return true
}

// diagnoseJSONError inspects a JSON decode error and returns an actionable error detail.
func diagnoseJSONError(err error) apiErrorDetail {
	d := apiErrorDetail{
		Error: "invalid JSON body",
		Code:  "INVALID_REQUEST",
	}

	switch {
	case errors.Is(err, io.EOF):
		d.Error = "request body is empty"
		d.Hint = "Send a JSON object in the request body. Ensure Content-Type is application/json."
		return d

	case errors.Is(err, io.ErrUnexpectedEOF):
		d.Error = "request body contains incomplete JSON"
		d.Hint = "The JSON is truncated — check for missing closing braces or brackets."
		return d
	}

	msg := err.Error()

	// http.MaxBytesError
	if strings.Contains(msg, "http: request body too large") {
		d.Error = "request body exceeds the 1 MB size limit"
		d.Hint = "Reduce the size of params or split into multiple requests."
		return d
	}

	// json.SyntaxError
	var synErr *json.SyntaxError
	if errors.As(err, &synErr) {
		d.Error = "JSON syntax error at byte offset " + strings.TrimPrefix(msg, "invalid character ")
		d.Hint = diagnoseJSONSyntaxHint(msg)
		return d
	}

	// json.UnmarshalTypeError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		if typeErr.Field == "" {
			// Top-level type mismatch — the caller sent a string/array/number instead of an object.
			d.Error = "expected a JSON object, got " + typeErr.Value
			d.Hint = "The request body must be a JSON object ({...}), not a " + typeErr.Value + ". Wrap your data in an object with the required fields."
		} else {
			d.Error = "wrong type for field \"" + typeErr.Field + "\""
			d.Hint = "Expected " + friendlyTypeName(typeErr.Type.String()) + " but got " + typeErr.Value + ". Check that field values match the expected types."
		}
		return d
	}

	// Fallback: include the raw error for anything else.
	d.Error = "invalid JSON body: " + msg
	d.Hint = "Ensure the body is valid JSON. Common issues: trailing commas, single quotes instead of double quotes, unquoted keys."
	return d
}

// friendlyTypeName converts Go type names into JSON-friendly descriptions.
func friendlyTypeName(goType string) string {
	switch {
	case goType == "string":
		return "a string"
	case goType == "int", goType == "int64", goType == "float64":
		return "a number"
	case goType == "bool":
		return "a boolean"
	case strings.HasPrefix(goType, "map["):
		return "an object"
	case strings.HasPrefix(goType, "[]"):
		return "an array"
	case strings.Contains(goType, "."):
		// Struct types like "store.TaskAction" — strip the package prefix
		// and convert to a friendlier form.
		parts := strings.SplitN(goType, ".", 2)
		return "an object (" + parts[len(parts)-1] + ")"
	default:
		return goType
	}
}

// diagnoseJSONSyntaxHint returns a human-friendly hint for common JSON syntax problems.
func diagnoseJSONSyntaxHint(msg string) string {
	switch {
	case strings.Contains(msg, "looking for beginning of value"):
		return "Check for trailing commas, missing values, or a non-JSON body. Ensure Content-Type is application/json."
	case strings.Contains(msg, "looking for beginning of object key string"):
		return "Object keys must be double-quoted strings. Check for trailing commas after the last field or single-quoted keys."
	case strings.Contains(msg, "after object key"):
		return "Expected ':' after object key. Check that keys and values are separated by colons."
	case strings.Contains(msg, "after object key:value pair"):
		return "Expected ',' or '}' after a key-value pair. Check for missing commas between fields."
	case strings.Contains(msg, "after array element"):
		return "Expected ',' or ']' after an array element. Check for missing commas between elements."
	default:
		return "Check the JSON syntax near the reported position. Common issues: trailing commas, single quotes, unquoted keys."
	}
}
