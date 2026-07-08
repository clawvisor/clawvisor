// Package client is a thin, typed REST client for the Clawvisor server API.
//
// It intentionally keeps one method per server endpoint and returns typed
// errors (*APIError) so the Terraform resource layer can map HTTP status
// codes to framework diagnostics without re-parsing bodies. The provider is
// the only consumer; there is no stability guarantee for these types.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// APITokenPrefix is the required prefix for a Clawvisor API token (spec 05).
// The provider does not validate the full shape — the server is authoritative
// — but a bearer that does not start with this prefix is definitely not an
// API token and is worth flagging early.
const APITokenPrefix = "cvat_"

// Client talks to a single Clawvisor endpoint with one API token.
//
// Scope carries the org/instance path abstraction (PRD §8): governance and
// other org-scoped resources build their paths through Scope.Governance(...)
// so no resource ever concatenates a path by hand.
type Client struct {
	http     *http.Client
	endpoint string
	token    string
	Scope    PathScope
}

// PathScope resolves org-scoped vs instance-scoped REST paths. When OrgID is
// empty the instance-scoped OSS routes are used (`/api/governance/*`); when
// set, the cloud org routes are used (`/api/orgs/{id}/governance/*`).
type PathScope struct {
	OrgID string
}

// Governance returns the governance route for sub (e.g. "model_policy",
// "spend_caps/daily"). This is the single place path scoping is applied.
func (p PathScope) Governance(sub string) string {
	if p.OrgID != "" {
		return fmt.Sprintf("/api/orgs/%s/governance/%s", p.OrgID, sub)
	}
	return "/api/governance/" + sub
}

// New constructs a Client. endpoint is the base URL (no trailing slash
// required); token is the raw `cvat_` API token; orgID is optional (cloud).
func New(endpoint, token, orgID string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		http:     httpClient,
		endpoint: strings.TrimRight(endpoint, "/"),
		token:    token,
		Scope:    PathScope{OrgID: orgID},
	}
}

// APIError is a typed error carrying the server's status code and error body
// fields (`error`/`code`). Resources use it to decide framework behavior:
// 404 → remove from state; 401/403 → auth diagnostic; 409/422 → surface the
// server message verbatim.
type APIError struct {
	StatusCode int
	Code       string // server `code` field, e.g. "INSUFFICIENT_SCOPE"
	Message    string // server `error` field
	Method     string
	Path       string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	if e.Code != "" {
		return fmt.Sprintf("%s %s: %d %s (%s)", e.Method, e.Path, e.StatusCode, msg, e.Code)
	}
	return fmt.Sprintf("%s %s: %d %s", e.Method, e.Path, e.StatusCode, msg)
}

// NotFound reports whether err is an *APIError with a 404 status.
func NotFound(err error) bool {
	ae, ok := err.(*APIError)
	return ok && ae.StatusCode == http.StatusNotFound
}

// do issues one request. If body is non-nil it is JSON-encoded. If out is
// non-nil the (2xx) response body is JSON-decoded into it. Non-2xx responses
// are returned as *APIError.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, reqBody)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseAPIError(method, path, resp.StatusCode, raw)
	}

	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decoding %s %s response: %w", method, path, err)
		}
	}
	return nil
}

func parseAPIError(method, path string, status int, raw []byte) *APIError {
	ae := &APIError{StatusCode: status, Method: method, Path: path}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(raw, &body); err == nil {
		ae.Message = body.Error
		ae.Code = body.Code
	}
	if ae.Message == "" {
		ae.Message = strings.TrimSpace(string(raw))
	}
	return ae
}
