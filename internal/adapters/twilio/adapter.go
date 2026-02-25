// Package twilio implements the Clawvisor adapter for Twilio.
package twilio

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/oauth2"

	"github.com/ericlevine/clawvisor/internal/adapters"
	"github.com/ericlevine/clawvisor/internal/adapters/format"
)

const serviceID = "twilio"

// storedCredential holds Twilio Account SID + Auth Token as a composite.
// Token format: "ACCOUNT_SID:AUTH_TOKEN"
type storedCredential struct {
	Type  string `json:"type"`  // "api_key"
	Token string `json:"token"` // "ACxxxxxxxxx:auth_token_here"
}

// TwilioAdapter implements adapters.Adapter for Twilio.
type TwilioAdapter struct{}

func New() *TwilioAdapter { return &TwilioAdapter{} }

func (a *TwilioAdapter) ServiceID() string { return serviceID }

func (a *TwilioAdapter) SupportedActions() []string {
	return []string{
		"send_sms", "send_whatsapp", "list_messages", "get_message",
	}
}

func (a *TwilioAdapter) OAuthConfig() *oauth2.Config                         { return nil }
func (a *TwilioAdapter) RequiredScopes() []string                            { return nil }
func (a *TwilioAdapter) CredentialFromToken(_ *oauth2.Token) ([]byte, error) {
	return nil, fmt.Errorf("twilio: OAuth token exchange not supported — use API key activation")
}

func (a *TwilioAdapter) ValidateCredential(credBytes []byte) error {
	var cred storedCredential
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return fmt.Errorf("twilio: invalid credential: %w", err)
	}
	sid, token, err := splitCredential(cred.Token)
	if err != nil {
		return fmt.Errorf("twilio: %w", err)
	}
	if !strings.HasPrefix(sid, "AC") {
		return fmt.Errorf("twilio: account SID must start with 'AC'")
	}
	if token == "" {
		return fmt.Errorf("twilio: auth token is empty")
	}
	return nil
}

func (a *TwilioAdapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	sid, authToken, err := extractCredential(req.Credential)
	if err != nil {
		return nil, err
	}
	client := twilioClient(sid, authToken)

	switch req.Action {
	case "send_sms":
		return a.sendSMS(ctx, client, sid, req.Params)
	case "send_whatsapp":
		return a.sendWhatsApp(ctx, client, sid, req.Params)
	case "list_messages":
		return a.listMessages(ctx, client, sid, req.Params)
	case "get_message":
		return a.getMessage(ctx, client, sid, req.Params)
	default:
		return nil, fmt.Errorf("twilio: unsupported action %q", req.Action)
	}
}

func extractCredential(credBytes []byte) (sid, authToken string, err error) {
	var cred storedCredential
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return "", "", fmt.Errorf("twilio: parsing credential: %w", err)
	}
	return splitCredential(cred.Token)
}

func splitCredential(composite string) (sid, authToken string, err error) {
	parts := strings.SplitN(composite, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("token must be in format 'ACCOUNT_SID:AUTH_TOKEN'")
	}
	return parts[0], parts[1], nil
}

func twilioClient(sid, authToken string) *http.Client {
	return &http.Client{
		Transport: &basicAuthTransport{sid: sid, token: authToken, base: http.DefaultTransport},
	}
}

type basicAuthTransport struct {
	sid   string
	token string
	base  http.RoundTripper
}

func (t *basicAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.SetBasicAuth(t.sid, t.token)
	return t.base.RoundTrip(clone)
}

// baseURL returns the Twilio API base URL for the given account SID.
func baseURL(sid string) string {
	return fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/", sid)
}

// ── send_sms ──────────────────────────────────────────────────────────────────

func (a *TwilioAdapter) sendSMS(ctx context.Context, client *http.Client, sid string, params map[string]any) (*adapters.Result, error) {
	to, _ := params["to"].(string)
	body, _ := params["body"].(string)
	if to == "" || body == "" {
		return nil, fmt.Errorf("twilio send_sms: to and body are required")
	}

	form := url.Values{}
	form.Set("To", to)
	form.Set("Body", body)
	if from, ok := params["from"].(string); ok && from != "" {
		form.Set("From", from)
	}

	var resp struct {
		SID    string `json:"sid"`
		Status string `json:"status"`
		To     string `json:"to"`
		From   string `json:"from"`
	}
	if err := twilioFormPOST(ctx, client, baseURL(sid)+"Messages.json", form, &resp); err != nil {
		return nil, fmt.Errorf("twilio send_sms: %w", err)
	}

	return &adapters.Result{
		Summary: format.Summary("SMS sent to %s (status: %s)", resp.To, resp.Status),
		Data: map[string]any{
			"sid":    resp.SID,
			"status": resp.Status,
			"to":     resp.To,
			"from":   resp.From,
		},
	}, nil
}

// ── send_whatsapp ─────────────────────────────────────────────────────────────

func (a *TwilioAdapter) sendWhatsApp(ctx context.Context, client *http.Client, sid string, params map[string]any) (*adapters.Result, error) {
	to, _ := params["to"].(string)
	body, _ := params["body"].(string)
	if to == "" || body == "" {
		return nil, fmt.Errorf("twilio send_whatsapp: to and body are required")
	}

	// Prefix with whatsapp: if not already present.
	if !strings.HasPrefix(to, "whatsapp:") {
		to = "whatsapp:" + to
	}

	form := url.Values{}
	form.Set("To", to)
	form.Set("Body", body)
	if from, ok := params["from"].(string); ok && from != "" {
		if !strings.HasPrefix(from, "whatsapp:") {
			from = "whatsapp:" + from
		}
		form.Set("From", from)
	}

	var resp struct {
		SID    string `json:"sid"`
		Status string `json:"status"`
		To     string `json:"to"`
		From   string `json:"from"`
	}
	if err := twilioFormPOST(ctx, client, baseURL(sid)+"Messages.json", form, &resp); err != nil {
		return nil, fmt.Errorf("twilio send_whatsapp: %w", err)
	}

	return &adapters.Result{
		Summary: format.Summary("WhatsApp message sent to %s (status: %s)", resp.To, resp.Status),
		Data: map[string]any{
			"sid":    resp.SID,
			"status": resp.Status,
			"to":     resp.To,
			"from":   resp.From,
		},
	}, nil
}

// ── list_messages ─────────────────────────────────────────────────────────────

func (a *TwilioAdapter) listMessages(ctx context.Context, client *http.Client, sid string, params map[string]any) (*adapters.Result, error) {
	q := url.Values{}
	pageSize := 20
	if v, ok := paramInt(params, "page_size"); ok && v > 0 && v <= 1000 {
		pageSize = v
	}
	q.Set("PageSize", fmt.Sprintf("%d", pageSize))
	if to, ok := params["to"].(string); ok && to != "" {
		q.Set("To", to)
	}
	if from, ok := params["from"].(string); ok && from != "" {
		q.Set("From", from)
	}

	apiURL := baseURL(sid) + "Messages.json"
	if len(q) > 0 {
		apiURL += "?" + q.Encode()
	}

	var resp struct {
		Messages []struct {
			SID       string `json:"sid"`
			To        string `json:"to"`
			From      string `json:"from"`
			Body      string `json:"body"`
			Status    string `json:"status"`
			Direction string `json:"direction"`
			DateSent  string `json:"date_sent"`
		} `json:"messages"`
	}
	if err := twilioGET(ctx, client, apiURL, &resp); err != nil {
		return nil, fmt.Errorf("twilio list_messages: %w", err)
	}

	type messageItem struct {
		SID       string `json:"sid"`
		To        string `json:"to"`
		From      string `json:"from"`
		Body      string `json:"body"`
		Status    string `json:"status"`
		Direction string `json:"direction"`
		DateSent  string `json:"date_sent"`
	}
	items := make([]messageItem, 0, len(resp.Messages))
	for _, m := range resp.Messages {
		items = append(items, messageItem{
			SID:       m.SID,
			To:        m.To,
			From:      m.From,
			Body:      format.SanitizeText(m.Body, format.MaxBodyLen),
			Status:    m.Status,
			Direction: m.Direction,
			DateSent:  m.DateSent,
		})
	}
	return &adapters.Result{
		Summary: format.Summary("%d message(s)", len(items)),
		Data:    items,
	}, nil
}

// ── get_message ───────────────────────────────────────────────────────────────

func (a *TwilioAdapter) getMessage(ctx context.Context, client *http.Client, sid string, params map[string]any) (*adapters.Result, error) {
	msgSID, _ := params["sid"].(string)
	if msgSID == "" {
		return nil, fmt.Errorf("twilio get_message: sid is required")
	}

	apiURL := baseURL(sid) + "Messages/" + msgSID + ".json"

	var resp struct {
		SID       string `json:"sid"`
		To        string `json:"to"`
		From      string `json:"from"`
		Body      string `json:"body"`
		Status    string `json:"status"`
		Direction string `json:"direction"`
		DateSent  string `json:"date_sent"`
		Price     string `json:"price"`
		PriceUnit string `json:"price_unit"`
	}
	if err := twilioGET(ctx, client, apiURL, &resp); err != nil {
		return nil, fmt.Errorf("twilio get_message: %w", err)
	}

	result := map[string]any{
		"sid":        resp.SID,
		"to":         resp.To,
		"from":       resp.From,
		"body":       format.SanitizeText(resp.Body, format.MaxBodyLen),
		"status":     resp.Status,
		"direction":  resp.Direction,
		"date_sent":  resp.DateSent,
		"price":      resp.Price,
		"price_unit": resp.PriceUnit,
	}
	return &adapters.Result{
		Summary: format.Summary("Message %s: %s → %s [%s]", resp.SID, resp.From, resp.To, resp.Status),
		Data:    result,
	}, nil
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

func twilioGET(ctx context.Context, client *http.Client, apiURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return json.Unmarshal(body, out)
}

func twilioFormPOST(ctx context.Context, client *http.Client, apiURL string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	if out != nil && len(body) > 0 {
		return json.Unmarshal(body, out)
	}
	return nil
}

func paramInt(params map[string]any, key string) (int, bool) {
	v, ok := params[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
