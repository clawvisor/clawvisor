package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client wraps HTTP calls to the Clawvisor API server.
type Client struct {
	baseURL      string
	httpClient   *http.Client
	accessToken  string
	refreshToken string
}

// New creates a client with the given server URL and refresh token.
func New(baseURL, refreshToken string) *Client {
	return &Client{
		baseURL:      strings.TrimRight(baseURL, "/"),
		refreshToken: refreshToken,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (c *Client) SetAccessToken(t string)  { c.accessToken = t }
func (c *Client) SetRefreshToken(t string) { c.refreshToken = t }
func (c *Client) BaseURL() string           { return c.baseURL }

// ── Auth ────────────────────────────────────────────────────────────────────

// Login authenticates with email and password.
func (c *Client) Login(email, password string) (*AuthResponse, error) {
	body := map[string]string{"email": email, "password": password}
	var resp AuthResponse
	if err := c.post("/api/auth/login", body, &resp); err != nil {
		return nil, err
	}
	c.accessToken = resp.AccessToken
	c.refreshToken = resp.RefreshToken
	return &resp, nil
}

// Refresh exchanges the refresh token for new tokens.
func (c *Client) Refresh() error {
	if c.refreshToken == "" {
		return fmt.Errorf("no refresh token")
	}
	body := map[string]string{"refresh_token": c.refreshToken}
	var resp AuthResponse
	if err := c.doJSON("POST", c.baseURL+"/api/auth/refresh", body, &resp); err != nil {
		return fmt.Errorf("refresh: %w", err)
	}
	c.accessToken = resp.AccessToken
	c.refreshToken = resp.RefreshToken
	return nil
}

// EnsureAuth ensures we have a valid access token, refreshing if needed.
func (c *Client) EnsureAuth() error {
	if c.accessToken != "" {
		return nil
	}
	return c.Refresh()
}

// GetPublicConfig fetches public server configuration (no auth required).
func (c *Client) GetPublicConfig() (*PublicConfig, error) {
	var resp PublicConfig
	req, err := http.NewRequest("GET", c.baseURL+"/api/config/public", nil)
	if err != nil {
		return nil, err
	}
	hresp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer hresp.Body.Close()
	if hresp.StatusCode != 200 {
		return nil, fmt.Errorf("config/public returned %d", hresp.StatusCode)
	}
	if err := json.NewDecoder(hresp.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &resp, nil
}

// LoginMagic exchanges a magic token for auth tokens.
func (c *Client) LoginMagic(token string) (*AuthResponse, error) {
	body := map[string]string{"token": token}
	var resp AuthResponse
	if err := c.doJSON("POST", c.baseURL+"/api/auth/magic", body, &resp); err != nil {
		return nil, err
	}
	c.accessToken = resp.AccessToken
	c.refreshToken = resp.RefreshToken
	return &resp, nil
}

// ── Overview ────────────────────────────────────────────────────────────────

func (c *Client) GetOverview() (*OverviewResponse, error) {
	var resp OverviewResponse
	if err := c.get("/api/overview", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ── Queue ───────────────────────────────────────────────────────────────────

func (c *Client) GetQueue() (*QueueResponse, error) {
	var resp QueueResponse
	if err := c.get("/api/queue", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ── Approvals ───────────────────────────────────────────────────────────────

func (c *Client) GetApprovals() (*ApprovalsResponse, error) {
	var resp ApprovalsResponse
	if err := c.get("/api/approvals", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) ApproveRequest(requestID string) (*ApprovalActionResponse, error) {
	var resp ApprovalActionResponse
	if err := c.post("/api/approvals/"+requestID+"/approve", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) DenyRequest(requestID string) (*ApprovalActionResponse, error) {
	var resp ApprovalActionResponse
	if err := c.post("/api/approvals/"+requestID+"/deny", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ── Tasks ───────────────────────────────────────────────────────────────────

type TaskFilter struct {
	ActiveOnly bool
	Limit      int
	Offset     int
}

func (c *Client) GetTasks(f TaskFilter) (*TasksResponse, error) {
	params := url.Values{}
	if f.ActiveOnly {
		params.Set("active_only", "true")
	}
	if f.Limit > 0 {
		params.Set("limit", fmt.Sprintf("%d", f.Limit))
	}
	if f.Offset > 0 {
		params.Set("offset", fmt.Sprintf("%d", f.Offset))
	}
	var resp TasksResponse
	if err := c.get("/api/tasks", params, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) ApproveTask(taskID string) (*TaskActionResponse, error) {
	var resp TaskActionResponse
	if err := c.post("/api/tasks/"+taskID+"/approve", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) DenyTask(taskID string) (*TaskActionResponse, error) {
	var resp TaskActionResponse
	if err := c.post("/api/tasks/"+taskID+"/deny", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) RevokeTask(taskID string) (*TaskActionResponse, error) {
	var resp TaskActionResponse
	if err := c.post("/api/tasks/"+taskID+"/revoke", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) ApproveExpansion(taskID string) (*TaskActionResponse, error) {
	var resp TaskActionResponse
	if err := c.post("/api/tasks/"+taskID+"/expand/approve", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) DenyExpansion(taskID string) (*TaskActionResponse, error) {
	var resp TaskActionResponse
	if err := c.post("/api/tasks/"+taskID+"/expand/deny", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ── Audit ───────────────────────────────────────────────────────────────────

type AuditFilter struct {
	Service    string
	Outcome    string
	DataOrigin string
	TaskID     string
	Limit      int
	Offset     int
}

func (c *Client) GetAudit(f AuditFilter) (*AuditResponse, error) {
	params := url.Values{}
	if f.Service != "" {
		params.Set("service", f.Service)
	}
	if f.Outcome != "" {
		params.Set("outcome", f.Outcome)
	}
	if f.DataOrigin != "" {
		params.Set("data_origin", f.DataOrigin)
	}
	if f.TaskID != "" {
		params.Set("task_id", f.TaskID)
	}
	if f.Limit > 0 {
		params.Set("limit", fmt.Sprintf("%d", f.Limit))
	}
	if f.Offset > 0 {
		params.Set("offset", fmt.Sprintf("%d", f.Offset))
	}
	var resp AuditResponse
	if err := c.get("/api/audit", params, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) GetAuditEntry(id string) (*AuditEntry, error) {
	var resp AuditEntry
	if err := c.get("/api/audit/"+id, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ── Services ────────────────────────────────────────────────────────────────

func (c *Client) GetServices() (*ServicesResponse, error) {
	var resp ServicesResponse
	if err := c.get("/api/services", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ── Service Activation ──────────────────────────────────────────────────────

// GetOAuthURL returns an OAuth authorization URL for the given service.
// If cliCallback is non-empty it is passed as cli_callback so the OAuth HTML
// page pings the TUI's local server on completion.
func (c *Client) GetOAuthURL(serviceID, alias, cliCallback string) (*OAuthURLResponse, error) {
	params := url.Values{}
	params.Set("service", serviceID)
	if alias != "" {
		params.Set("alias", alias)
	}
	if cliCallback != "" {
		params.Set("cli_callback", cliCallback)
	}
	var resp OAuthURLResponse
	if err := c.get("/api/oauth/url", params, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ActivateWithKey activates a non-OAuth service using an API key / token.
func (c *Client) ActivateWithKey(serviceID, token, alias string, config map[string]string) error {
	body := map[string]any{"token": token}
	if alias != "" {
		body["alias"] = alias
	}
	if len(config) > 0 {
		body["config"] = config
	}
	var resp map[string]string
	return c.post("/api/services/"+serviceID+"/activate-key", body, &resp)
}

// ActivateService activates a credential-free service (e.g. iMessage).
func (c *Client) ActivateService(serviceID string) error {
	var resp map[string]string
	return c.post("/api/services/"+serviceID+"/activate", nil, &resp)
}

// DeactivateService removes credentials for a service (default alias).
func (c *Client) DeactivateService(serviceID, alias string) error {
	body := map[string]string{}
	if alias != "" {
		body["alias"] = alias
	}
	var resp map[string]string
	return c.post("/api/services/"+serviceID+"/deactivate", body, &resp)
}

// ── Device Flow ─────────────────────────────────────────────────────────────

// DeviceFlowStart initiates a device authorization flow for the given service.
func (c *Client) DeviceFlowStart(serviceID, alias string) (*DeviceFlowStartResponse, error) {
	body := map[string]string{}
	if alias != "" {
		body["alias"] = alias
	}
	var resp DeviceFlowStartResponse
	if err := c.post("/api/services/"+serviceID+"/device-flow/start", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeviceFlowPoll polls for device flow completion.
func (c *Client) DeviceFlowPoll(serviceID, flowID string) (*DeviceFlowPollResponse, error) {
	body := map[string]string{"flow_id": flowID}
	var resp DeviceFlowPollResponse
	if err := c.post("/api/services/"+serviceID+"/device-flow/poll", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ── PKCE Flow ───────────────────────────────────────────────────────────────

// PKCEFlowStart initiates a PKCE authorization code flow for the given service.
func (c *Client) PKCEFlowStart(serviceID, alias, cliCallback string) (*PKCEFlowStartResponse, error) {
	body := map[string]string{}
	if alias != "" {
		body["alias"] = alias
	}
	if cliCallback != "" {
		body["cli_callback"] = cliCallback
	}
	var resp PKCEFlowStartResponse
	if err := c.post("/api/services/"+serviceID+"/pkce-flow/start", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ── Restrictions ────────────────────────────────────────────────────────────

func (c *Client) GetRestrictions() ([]Restriction, error) {
	var resp []Restriction
	if err := c.get("/api/restrictions", nil, &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *Client) CreateRestriction(service, action, reason string) (*Restriction, error) {
	body := map[string]string{
		"service": service,
		"action":  action,
		"reason":  reason,
	}
	var resp Restriction
	if err := c.post("/api/restrictions", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) DeleteRestriction(id string) error {
	return c.delete("/api/restrictions/" + id)
}

// ── System OAuth Config ─────────────────────────────────────────────────

// GoogleOAuthConfigured checks whether Google OAuth app credentials are stored.
func (c *Client) GoogleOAuthConfigured() (bool, error) {
	var resp struct {
		Configured bool `json:"configured"`
	}
	if err := c.get("/api/system/google-oauth", nil, &resp); err != nil {
		return false, err
	}
	return resp.Configured, nil
}

// SetGoogleOAuthConfig stores Google OAuth app credentials in the system vault.
func (c *Client) SetGoogleOAuthConfig(clientID, clientSecret string) error {
	body := map[string]string{
		"client_id":     clientID,
		"client_secret": clientSecret,
	}
	var resp struct {
		OK bool `json:"ok"`
	}
	return c.post("/api/system/google-oauth", body, &resp)
}

// ── Agents ──────────────────────────────────────────────────────────────────

func (c *Client) GetAgents() ([]Agent, error) {
	var resp []Agent
	if err := c.get("/api/agents", nil, &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *Client) CreateAgent(name string) (*Agent, error) {
	return c.CreateAgentWithOpts(name, false)
}

// CreateAgentWithOpts creates an agent, optionally generating a callback secret.
func (c *Client) CreateAgentWithOpts(name string, withCallbackSecret bool) (*Agent, error) {
	body := map[string]any{"name": name}
	if withCallbackSecret {
		body["with_callback_secret"] = true
	}
	var resp Agent
	if err := c.post("/api/agents", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) DeleteAgent(id string) error {
	return c.delete("/api/agents/" + id)
}

// ── Health ──────────────────────────────────────────────────────────────────

func (c *Client) Health() error {
	req, err := http.NewRequest("GET", c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("health check returned %d", resp.StatusCode)
	}
	return nil
}

// GetVersion fetches version info from the server (no auth required).
func (c *Client) GetVersion() (*VersionInfo, error) {
	var resp VersionInfo
	req, err := http.NewRequest("GET", c.baseURL+"/api/version", nil)
	if err != nil {
		return nil, err
	}
	hresp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer hresp.Body.Close()
	if hresp.StatusCode != 200 {
		return nil, fmt.Errorf("version returned %d", hresp.StatusCode)
	}
	if err := json.NewDecoder(hresp.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &resp, nil
}

// ── HTTP helpers ────────────────────────────────────────────────────────────

func (c *Client) get(path string, params url.Values, dst interface{}) error {
	u := c.baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	return c.doJSONWithRetry("GET", u, nil, dst)
}

func (c *Client) post(path string, body interface{}, dst interface{}) error {
	return c.doJSONWithRetry("POST", c.baseURL+path, body, dst)
}

func (c *Client) delete(path string) error {
	return c.doJSONWithRetry("DELETE", c.baseURL+path, nil, nil)
}

// doJSONWithRetry makes a request and retries once on 401 after refreshing.
func (c *Client) doJSONWithRetry(method, fullURL string, body interface{}, dst interface{}) error {
	err := c.doJSON(method, fullURL, body, dst)
	if err == nil {
		return nil
	}
	// If 401 and not an auth endpoint, try refresh + retry.
	if isUnauthorized(err) && !strings.Contains(fullURL, "/api/auth/") {
		if refreshErr := c.Refresh(); refreshErr != nil {
			return fmt.Errorf("session expired: %w", refreshErr)
		}
		return c.doJSON(method, fullURL, body, dst)
	}
	return err
}

func (c *Client) doJSON(method, fullURL string, body interface{}, dst interface{}) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, fullURL, bodyReader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode == 401 {
		return &APIError{StatusCode: 401, Message: "unauthorized"}
	}
	if resp.StatusCode == 204 {
		return nil
	}
	if resp.StatusCode >= 400 {
		var apiErr struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(respBody, &apiErr) == nil && apiErr.Error != "" {
			return &APIError{StatusCode: resp.StatusCode, Message: apiErr.Error}
		}
		return &APIError{StatusCode: resp.StatusCode, Message: string(respBody)}
	}

	if dst != nil {
		if err := json.Unmarshal(respBody, dst); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

// ── Devices ─────────────────────────────────────────────────────────────────

// GetPairingCode fetches a new 6-digit pairing code from GET /api/pairing/code.
func (c *Client) GetPairingCode() (*PairingCodeResponse, error) {
	var resp PairingCodeResponse
	if err := c.get("/api/pairing/code", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// StartPairing initiates a device pairing session and returns the token/code.
func (c *Client) StartPairing() (*StartPairingResponse, error) {
	var resp StartPairingResponse
	if err := c.post("/api/devices/pair", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListDevices returns all paired devices for the current user.
func (c *Client) ListDevices() ([]PairedDevice, error) {
	var resp []PairedDevice
	if err := c.get("/api/devices", nil, &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// APIError represents an HTTP error from the API.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API %d: %s", e.StatusCode, e.Message)
}

func isUnauthorized(err error) bool {
	if apiErr, ok := err.(*APIError); ok {
		return apiErr.StatusCode == 401
	}
	return false
}
