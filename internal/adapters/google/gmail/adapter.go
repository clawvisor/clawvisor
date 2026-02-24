// Package gmail implements the Clawvisor adapter for Gmail.
package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/ericlevine/clawvisor/internal/adapters"
	"github.com/ericlevine/clawvisor/internal/adapters/format"
)

const serviceID = "google.gmail"

// storedCredential is the JSON structure saved (encrypted) in the vault under key "google".
type storedCredential struct {
	Type         string    `json:"type"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
	Scopes       []string  `json:"scopes"`
}

// GmailAdapter implements adapters.Adapter for Gmail.
type GmailAdapter struct {
	clientID     string
	clientSecret string
	redirectURL  string
}

func New(clientID, clientSecret, redirectURL string) *GmailAdapter {
	return &GmailAdapter{
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURL:  redirectURL,
	}
}

func (a *GmailAdapter) ServiceID() string { return serviceID }

func (a *GmailAdapter) SupportedActions() []string {
	return []string{"list_messages", "get_message", "send_message"}
}

// allGoogleScopes is the unified scope set requested for all Google services.
// All Google adapters (Gmail, Calendar, Drive, Contacts) request the same scope set
// so a single OAuth consent grants access to all of them.
var allGoogleScopes = []string{
	"https://www.googleapis.com/auth/gmail.readonly",
	"https://www.googleapis.com/auth/gmail.send",
	"https://www.googleapis.com/auth/calendar.readonly",
	"https://www.googleapis.com/auth/calendar.events",
	"https://www.googleapis.com/auth/drive.readonly",
	"https://www.googleapis.com/auth/drive.file",
	"https://www.googleapis.com/auth/contacts.readonly",
}

func (a *GmailAdapter) OAuthConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     a.clientID,
		ClientSecret: a.clientSecret,
		RedirectURL:  a.redirectURL,
		Scopes:       allGoogleScopes,
		Endpoint:     google.Endpoint,
	}
}

func (a *GmailAdapter) CredentialFromToken(token *oauth2.Token) ([]byte, error) {
	cred := storedCredential{
		Type:         "oauth2",
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		Expiry:       token.Expiry,
		Scopes:       allGoogleScopes,
	}
	return json.Marshal(cred)
}

func (a *GmailAdapter) ValidateCredential(credBytes []byte) error {
	var cred storedCredential
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return fmt.Errorf("gmail: invalid credential: %w", err)
	}
	if cred.RefreshToken == "" && cred.AccessToken == "" {
		return fmt.Errorf("gmail: credential missing tokens")
	}
	return nil
}

// Execute runs a Gmail action. Credential is injected by the gateway.
func (a *GmailAdapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	client, err := a.httpClient(ctx, req.Credential)
	if err != nil {
		return nil, err
	}

	switch req.Action {
	case "list_messages":
		return a.listMessages(ctx, client, req.Params)
	case "get_message":
		return a.getMessage(ctx, client, req.Params)
	case "send_message":
		return a.sendMessage(ctx, client, req.Params)
	default:
		return nil, fmt.Errorf("gmail: unsupported action %q", req.Action)
	}
}

// ── HTTP client from stored credential ───────────────────────────────────────

func (a *GmailAdapter) httpClient(ctx context.Context, credBytes []byte) (*http.Client, error) {
	var cred storedCredential
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		return nil, fmt.Errorf("gmail: parsing credential: %w", err)
	}

	token := &oauth2.Token{
		AccessToken:  cred.AccessToken,
		RefreshToken: cred.RefreshToken,
		Expiry:       cred.Expiry,
		TokenType:    "Bearer",
	}

	cfg := a.OAuthConfig()
	// Token source auto-refreshes when the token is expired.
	ts := cfg.TokenSource(ctx, token)
	return oauth2.NewClient(ctx, ts), nil
}

// ── list_messages ─────────────────────────────────────────────────────────────

type msgListItem struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	Subject  string `json:"subject"`
	Snippet  string `json:"snippet"`
	Date     string `json:"timestamp"`
	IsUnread bool   `json:"is_unread"`
}

func (a *GmailAdapter) listMessages(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	query, _ := params["query"].(string)
	maxResults := 10
	if v, ok := paramInt(params, "max_results"); ok {
		if v > 0 && v <= 50 {
			maxResults = v
		}
	}

	url := fmt.Sprintf("https://gmail.googleapis.com/gmail/v1/users/me/messages?maxResults=%d", maxResults)
	if query != "" {
		url += "&q=" + encodeParam(query)
	}

	var listResp struct {
		Messages           []struct{ ID string `json:"id"` } `json:"messages"`
		ResultSizeEstimate int                               `json:"resultSizeEstimate"`
	}
	if err := gmailGET(ctx, client, url, &listResp); err != nil {
		return nil, fmt.Errorf("gmail list_messages: %w", err)
	}

	items := make([]msgListItem, 0, len(listResp.Messages))
	unread := 0
	for _, m := range listResp.Messages {
		meta, err := fetchMessageMeta(ctx, client, m.ID)
		if err != nil {
			continue
		}
		item := msgListItem{
			ID:       m.ID,
			From:     format.SanitizeText(meta.from, format.MaxFieldLen),
			Subject:  format.SanitizeText(meta.subject, format.MaxFieldLen),
			Snippet:  format.SanitizeText(meta.snippet, format.MaxSnippetLen),
			Date:     meta.date,
			IsUnread: meta.isUnread,
		}
		items = append(items, item)
		if meta.isUnread {
			unread++
		}
	}

	total := listResp.ResultSizeEstimate
	summary := format.Summary("%d messages (%d unread)", len(items), unread)
	if total > len(items) {
		summary = format.Summary("%d of ~%d messages (%d unread)", len(items), total, unread)
	}

	return &adapters.Result{Summary: summary, Data: items}, nil
}

// ── get_message ───────────────────────────────────────────────────────────────

type msgDetail struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	To       string `json:"to"`
	Subject  string `json:"subject"`
	Date     string `json:"timestamp"`
	Body     string `json:"body"`
	IsUnread bool   `json:"is_unread"`
	ThreadID string `json:"thread_id"`
}

func (a *GmailAdapter) getMessage(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	msgID, _ := params["message_id"].(string)
	if msgID == "" {
		return nil, fmt.Errorf("gmail get_message: message_id is required")
	}

	url := fmt.Sprintf("https://gmail.googleapis.com/gmail/v1/users/me/messages/%s?format=full", msgID)
	var raw struct {
		ID       string `json:"id"`
		ThreadId string `json:"threadId"`
		LabelIds []string `json:"labelIds"`
		Snippet  string `json:"snippet"`
		Payload  struct {
			Headers []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"headers"`
			Parts []struct {
				MimeType string `json:"mimeType"`
				Body     struct {
					Data string `json:"data"`
				} `json:"body"`
				Parts []struct {
					MimeType string `json:"mimeType"`
					Body     struct {
						Data string `json:"data"`
					} `json:"body"`
				} `json:"parts"`
			} `json:"parts"`
			Body struct {
				Data string `json:"data"`
			} `json:"body"`
			MimeType string `json:"mimeType"`
		} `json:"payload"`
	}

	if err := gmailGET(ctx, client, url, &raw); err != nil {
		return nil, fmt.Errorf("gmail get_message: %w", err)
	}

	// Extract headers
	var from, to, subject, date string
	for _, h := range raw.Payload.Headers {
		switch strings.ToLower(h.Name) {
		case "from":
			from = h.Value
		case "to":
			to = h.Value
		case "subject":
			subject = h.Value
		case "date":
			date = h.Value
		}
	}

	isUnread := false
	for _, l := range raw.LabelIds {
		if l == "UNREAD" {
			isUnread = true
		}
	}

	body := extractBody(raw.Payload)
	if body == "" {
		body = raw.Snippet
	}

	detail := msgDetail{
		ID:       raw.ID,
		From:     format.SanitizeText(from, format.MaxFieldLen),
		To:       format.SanitizeText(to, format.MaxFieldLen),
		Subject:  format.SanitizeText(subject, format.MaxFieldLen),
		Date:     date,
		Body:     format.SanitizeText(body, format.MaxBodyLen),
		IsUnread: isUnread,
		ThreadID: raw.ThreadId,
	}

	summary := format.Summary("Email from %s: %q", detail.From, detail.Subject)
	return &adapters.Result{Summary: summary, Data: detail}, nil
}

// ── send_message ──────────────────────────────────────────────────────────────

func (a *GmailAdapter) sendMessage(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	to, _ := params["to"].(string)
	subject, _ := params["subject"].(string)
	body, _ := params["body"].(string)
	inReplyTo, _ := params["in_reply_to"].(string)

	if to == "" {
		return nil, fmt.Errorf("gmail send_message: to is required")
	}
	if subject == "" {
		return nil, fmt.Errorf("gmail send_message: subject is required")
	}

	raw := buildMIMEMessage(to, subject, body, inReplyTo)
	encoded := base64.URLEncoding.EncodeToString([]byte(raw))

	var sendResp struct {
		ID string `json:"id"`
	}
	if err := gmailPOST(ctx, client, "https://gmail.googleapis.com/gmail/v1/users/me/messages/send",
		map[string]string{"raw": encoded}, &sendResp); err != nil {
		return nil, fmt.Errorf("gmail send_message: %w", err)
	}

	result := map[string]string{
		"message_id": sendResp.ID,
		"to":         to,
		"subject":    subject,
	}
	summary := format.Summary("Email sent to %s (subject: %q)", to, subject)
	return &adapters.Result{Summary: summary, Data: result}, nil
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

func gmailGET(ctx context.Context, client *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
		return fmt.Errorf("gmail API %s: %d: %s", url, resp.StatusCode, truncate(string(body), 200))
	}
	return json.Unmarshal(body, out)
}

func gmailPOST(ctx context.Context, client *http.Client, url string, payload any, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("gmail API POST %s: %d: %s", url, resp.StatusCode, truncate(string(body), 200))
	}
	return json.Unmarshal(body, out)
}

// ── Message parsing helpers ───────────────────────────────────────────────────

type msgMeta struct {
	from, subject, snippet, date string
	isUnread                     bool
}

func fetchMessageMeta(ctx context.Context, client *http.Client, id string) (msgMeta, error) {
	url := fmt.Sprintf("https://gmail.googleapis.com/gmail/v1/users/me/messages/%s?format=metadata&metadataHeaders=From&metadataHeaders=Subject&metadataHeaders=Date", id)
	var raw struct {
		Snippet  string   `json:"snippet"`
		LabelIds []string `json:"labelIds"`
		Payload  struct {
			Headers []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"headers"`
		} `json:"payload"`
	}
	if err := gmailGET(ctx, client, url, &raw); err != nil {
		return msgMeta{}, err
	}

	meta := msgMeta{snippet: raw.Snippet}
	for _, h := range raw.Payload.Headers {
		switch strings.ToLower(h.Name) {
		case "from":
			meta.from = h.Value
		case "subject":
			meta.subject = h.Value
		case "date":
			meta.date = h.Value
		}
	}
	for _, l := range raw.LabelIds {
		if l == "UNREAD" {
			meta.isUnread = true
		}
	}
	return meta, nil
}

// extractBody walks a message payload to find the text/plain part.
func extractBody(payload struct {
	Headers []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
	Parts []struct {
		MimeType string `json:"mimeType"`
		Body     struct {
			Data string `json:"data"`
		} `json:"body"`
		Parts []struct {
			MimeType string `json:"mimeType"`
			Body     struct {
				Data string `json:"data"`
			} `json:"body"`
		} `json:"parts"`
	} `json:"parts"`
	Body struct {
		Data string `json:"data"`
	} `json:"body"`
	MimeType string `json:"mimeType"`
}) string {
	// Direct body
	if payload.MimeType == "text/plain" && payload.Body.Data != "" {
		return decodeBase64(payload.Body.Data)
	}

	// Search parts
	for _, part := range payload.Parts {
		if part.MimeType == "text/plain" && part.Body.Data != "" {
			return decodeBase64(part.Body.Data)
		}
		// Nested multipart
		for _, sub := range part.Parts {
			if sub.MimeType == "text/plain" && sub.Body.Data != "" {
				return decodeBase64(sub.Body.Data)
			}
		}
	}

	// Fall back to any HTML part — strip tags before returning.
	for _, part := range payload.Parts {
		if part.MimeType == "text/html" && part.Body.Data != "" {
			return stripHTML(decodeBase64(part.Body.Data))
		}
	}
	// Also handle direct HTML body (non-multipart).
	if payload.MimeType == "text/html" && payload.Body.Data != "" {
		return stripHTML(decodeBase64(payload.Body.Data))
	}

	return ""
}

// stripHTML removes HTML tags, style/script blocks, and decodes common entities,
// returning plain text suitable for an LLM or human reader.
func stripHTML(s string) string {
	// Remove <style>...</style> and <script>...</script> blocks (case-insensitive).
	for _, tag := range []string{"style", "script"} {
		for {
			open := strings.Index(strings.ToLower(s), "<"+tag)
			if open < 0 {
				break
			}
			close := strings.Index(strings.ToLower(s[open:]), "</"+tag+">")
			if close < 0 {
				s = s[:open]
				break
			}
			s = s[:open] + s[open+close+len("</"+tag+">"):]
		}
	}
	// Strip remaining HTML tags.
	var out strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
			out.WriteRune(' ') // replace tag with space to separate words
		case !inTag:
			out.WriteRune(r)
		}
	}
	// Decode common HTML entities.
	result := out.String()
	result = strings.ReplaceAll(result, "&amp;", "&")
	result = strings.ReplaceAll(result, "&lt;", "<")
	result = strings.ReplaceAll(result, "&gt;", ">")
	result = strings.ReplaceAll(result, "&quot;", `"`)
	result = strings.ReplaceAll(result, "&#39;", "'")
	result = strings.ReplaceAll(result, "&nbsp;", " ")
	// Collapse runs of whitespace/newlines.
	lines := strings.Split(result, "\n")
	var kept []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

func decodeBase64(s string) string {
	// Gmail uses URL-safe base64
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		b, err = base64.StdEncoding.DecodeString(s)
		if err != nil {
			return ""
		}
	}
	return string(b)
}

func buildMIMEMessage(to, subject, body, inReplyTo string) string {
	var sb strings.Builder
	sb.WriteString("From: me\r\n")
	sb.WriteString("To: " + to + "\r\n")
	sb.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n")
	if inReplyTo != "" {
		sb.WriteString("In-Reply-To: " + inReplyTo + "\r\n")
	}
	sb.WriteString("MIME-Version: 1.0\r\n")
	sb.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	sb.WriteString("\r\n")
	sb.WriteString(body)
	return sb.String()
}

func encodeParam(s string) string {
	return strings.ReplaceAll(s, " ", "+")
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
