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
	"os"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/internal/adapters/format"
	"github.com/clawvisor/clawvisor/internal/adapters/google/credential"
)

const serviceID = "google.gmail"

// gmailBaseScopes are always requested.
var gmailBaseScopes = []string{
	"https://www.googleapis.com/auth/gmail.readonly",
	"https://www.googleapis.com/auth/gmail.send",
}

// draftsEnabled reports whether the create_draft action is available.
// Set GMAIL_DRAFTS_ENABLED=false to disable in environments where the
// gmail.compose scope has not yet been approved.
func draftsEnabled() bool {
	return os.Getenv("GMAIL_DRAFTS_ENABLED") != "false"
}

// gmailScopes returns the scopes to request, including gmail.compose
// only when drafts are enabled.
func gmailScopes() []string {
	if !draftsEnabled() {
		return gmailBaseScopes
	}
	return append(gmailBaseScopes, "https://www.googleapis.com/auth/gmail.compose")
}

// GmailAdapter implements adapters.Adapter for Gmail.
type GmailAdapter struct {
	oauthProvider adapters.OAuthCredentialProvider
}

func New(provider adapters.OAuthCredentialProvider) *GmailAdapter {
	return &GmailAdapter{oauthProvider: provider}
}

func (a *GmailAdapter) ServiceID() string { return serviceID }

func (a *GmailAdapter) SupportedActions() []string {
	actions := []string{"list_messages", "get_message", "get_attachment", "send_message"}
	if draftsEnabled() {
		actions = append(actions, "create_draft")
	}
	return actions
}

func (a *GmailAdapter) RequiredScopes() []string { return gmailScopes() }

func (a *GmailAdapter) OAuthConfig() *oauth2.Config {
	clientID, clientSecret := a.oauthProvider.OAuthClientCredentials()
	if clientID == "" {
		return nil
	}
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       gmailScopes(),
		Endpoint:     google.Endpoint,
	}
}

func (a *GmailAdapter) CredentialFromToken(token *oauth2.Token) ([]byte, error) {
	return credential.FromToken(token, gmailScopes())
}

func (a *GmailAdapter) ValidateCredential(credBytes []byte) error {
	return credential.Validate(credBytes)
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
	case "get_attachment":
		return a.getAttachment(ctx, client, req.Params)
	case "send_message":
		return a.sendMessage(ctx, client, req.Params)
	case "create_draft":
		if err := a.requireComposeScope(req.Credential); err != nil {
			return nil, err
		}
		return a.createDraft(ctx, client, req.Params)
	default:
		return nil, fmt.Errorf("gmail: unsupported action %q", req.Action)
	}
}

// ── HTTP client from stored credential ───────────────────────────────────────

func (a *GmailAdapter) httpClient(ctx context.Context, credBytes []byte) (*http.Client, error) {
	cred, err := credential.Parse(credBytes)
	if err != nil {
		return nil, fmt.Errorf("gmail: %w", err)
	}
	ts := a.OAuthConfig().TokenSource(ctx, cred.ToOAuth2Token())
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

type attachmentMeta struct {
	AttachmentID string `json:"attachment_id"`
	Filename     string `json:"filename"`
	MimeType     string `json:"mime_type"`
	Size         int    `json:"size"`
}

type msgDetail struct {
	ID          string           `json:"id"`
	From        string           `json:"from"`
	To          string           `json:"to"`
	Subject     string           `json:"subject"`
	Date        string           `json:"timestamp"`
	Body        string           `json:"body"`
	IsUnread    bool             `json:"is_unread"`
	ThreadID    string           `json:"thread_id"`
	Attachments []attachmentMeta `json:"attachments,omitempty"`
}

func (a *GmailAdapter) getMessage(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	msgID, _ := params["message_id"].(string)
	if msgID == "" {
		return nil, fmt.Errorf("gmail get_message: message_id is required")
	}

	url := fmt.Sprintf("https://gmail.googleapis.com/gmail/v1/users/me/messages/%s?format=full", msgID)
	var raw gmailMessage
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

	body := extractBodyFromParts(raw.Payload)
	if body == "" {
		body = raw.Snippet
	}

	attachments := extractAttachments(raw.Payload)

	detail := msgDetail{
		ID:          raw.ID,
		From:        format.SanitizeText(from, format.MaxFieldLen),
		To:          format.SanitizeText(to, format.MaxFieldLen),
		Subject:     format.SanitizeText(subject, format.MaxFieldLen),
		Date:        date,
		Body:        format.SanitizeText(body, format.MaxBodyLen),
		IsUnread:    isUnread,
		ThreadID:    raw.ThreadId,
		Attachments: attachments,
	}

	summary := format.Summary("Email from %s: %q", detail.From, detail.Subject)
	if len(attachments) > 0 {
		summary = format.Summary("Email from %s: %q (%d attachments)", detail.From, detail.Subject, len(attachments))
	}
	return &adapters.Result{Summary: summary, Data: detail}, nil
}

// ── get_attachment ────────────────────────────────────────────────────────────

func (a *GmailAdapter) getAttachment(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	msgID, _ := params["message_id"].(string)
	attachmentID, _ := params["attachment_id"].(string)
	if msgID == "" {
		return nil, fmt.Errorf("gmail get_attachment: message_id is required")
	}
	if attachmentID == "" {
		return nil, fmt.Errorf("gmail get_attachment: attachment_id is required")
	}

	url := fmt.Sprintf("https://gmail.googleapis.com/gmail/v1/users/me/messages/%s/attachments/%s", msgID, attachmentID)
	var raw struct {
		Size int    `json:"size"`
		Data string `json:"data"`
	}
	if err := gmailGET(ctx, client, url, &raw); err != nil {
		return nil, fmt.Errorf("gmail get_attachment: %w", err)
	}

	result := map[string]any{
		"message_id":    msgID,
		"attachment_id": attachmentID,
		"size":          raw.Size,
		"data":          raw.Data,
	}
	summary := format.Summary("Attachment fetched (%d bytes)", raw.Size)
	return &adapters.Result{Summary: summary, Data: result}, nil
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

// requireComposeScope checks whether the stored credential includes the
// gmail.compose scope. Legacy tokens that only have gmail.send will fail
// with a descriptive error prompting the user to reconnect.
func (a *GmailAdapter) requireComposeScope(credBytes []byte) error {
	cred, err := credential.Parse(credBytes)
	if err != nil {
		return fmt.Errorf("gmail create_draft: %w", err)
	}
	if !credential.HasAllScopes(cred.Scopes, []string{"https://www.googleapis.com/auth/gmail.compose"}) {
		return fmt.Errorf("gmail create_draft: the gmail.compose scope is required — please reconnect your Google account to grant draft permissions")
	}
	return nil
}

// ── create_draft ──────────────────────────────────────────────────────────────

func (a *GmailAdapter) createDraft(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	to, _ := params["to"].(string)
	subject, _ := params["subject"].(string)
	body, _ := params["body"].(string)
	inReplyTo, _ := params["in_reply_to"].(string)

	if to == "" {
		return nil, fmt.Errorf("gmail create_draft: to is required")
	}
	if subject == "" {
		return nil, fmt.Errorf("gmail create_draft: subject is required")
	}

	raw := buildMIMEMessage(to, subject, body, inReplyTo)
	encoded := base64.URLEncoding.EncodeToString([]byte(raw))

	payload := map[string]any{
		"message": map[string]string{"raw": encoded},
	}

	var draftResp struct {
		ID      string `json:"id"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
	}
	if err := gmailPOST(ctx, client, "https://gmail.googleapis.com/gmail/v1/users/me/drafts",
		payload, &draftResp); err != nil {
		return nil, fmt.Errorf("gmail create_draft: %w", err)
	}

	result := map[string]string{
		"draft_id":   draftResp.ID,
		"message_id": draftResp.Message.ID,
		"to":         to,
		"subject":    subject,
	}
	summary := format.Summary("Draft created for %s (subject: %q)", to, subject)
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

// ── Gmail API message types ───────────────────────────────────────────────────

type gmailMessage struct {
	ID       string       `json:"id"`
	ThreadId string       `json:"threadId"`
	LabelIds []string     `json:"labelIds"`
	Snippet  string       `json:"snippet"`
	Payload  gmailPayload `json:"payload"`
}

type gmailPayload struct {
	MimeType string        `json:"mimeType"`
	Headers  []gmailHeader `json:"headers"`
	Body     gmailBody     `json:"body"`
	Parts    []gmailPart   `json:"parts"`
}

type gmailPart struct {
	MimeType string        `json:"mimeType"`
	Filename string        `json:"filename"`
	Headers  []gmailHeader `json:"headers"`
	Body     gmailBody     `json:"body"`
	Parts    []gmailPart   `json:"parts"`
}

type gmailHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type gmailBody struct {
	AttachmentID string `json:"attachmentId"`
	Size         int    `json:"size"`
	Data         string `json:"data"`
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
			Headers []gmailHeader `json:"headers"`
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

// extractBodyFromParts walks a message payload to find the best text content.
// Many newsletters (Substack, etc.) include only a short teaser in the
// text/plain part while the full article lives in text/html, so we extract
// both and return whichever is longer.
func extractBodyFromParts(payload gmailPayload) string {
	var plain, htmlBody string

	// Direct body (non-multipart message)
	if payload.Body.Data != "" {
		decoded := decodeBase64(payload.Body.Data)
		if payload.MimeType == "text/plain" {
			plain = decoded
		} else if payload.MimeType == "text/html" {
			htmlBody = stripHTML(decoded)
		}
	}

	// Search MIME parts
	if plain == "" {
		plain = findTextInParts(payload.Parts, "text/plain")
	}
	if htmlBody == "" {
		if raw := findTextInParts(payload.Parts, "text/html"); raw != "" {
			htmlBody = stripHTML(raw)
		}
	}

	// Return whichever is longer — newsletters often have full content
	// only in the HTML part while text/plain is just a preview.
	if len(htmlBody) > len(plain) {
		return htmlBody
	}
	if plain != "" {
		return plain
	}
	return htmlBody
}

// findTextInParts recursively searches MIME parts for content of the given type.
func findTextInParts(parts []gmailPart, mimeType string) string {
	for _, part := range parts {
		if part.MimeType == mimeType && part.Body.Data != "" {
			return decodeBase64(part.Body.Data)
		}
		if result := findTextInParts(part.Parts, mimeType); result != "" {
			return result
		}
	}
	return ""
}

// extractAttachments collects attachment metadata from MIME parts.
func extractAttachments(payload gmailPayload) []attachmentMeta {
	var attachments []attachmentMeta
	collectAttachments(payload.Parts, &attachments)
	return attachments
}

func collectAttachments(parts []gmailPart, out *[]attachmentMeta) {
	for _, part := range parts {
		if part.Filename != "" && part.Body.AttachmentID != "" {
			*out = append(*out, attachmentMeta{
				AttachmentID: part.Body.AttachmentID,
				Filename:     part.Filename,
				MimeType:     part.MimeType,
				Size:         part.Body.Size,
			})
		}
		collectAttachments(part.Parts, out)
	}
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

// ── Approval rendering & editing ─────────────────────────────────────────────

func paramStr(params map[string]any, key string) string {
	v, ok := params[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// RenderForApproval implements adapters.ApprovalRenderer.
func (a *GmailAdapter) RenderForApproval(action string, params map[string]any) ([]adapters.RenderedField, error) {
	switch action {
	case "send_message", "create_draft":
		fields := []adapters.RenderedField{
			{Key: "to", Label: "To", Value: paramStr(params, "to"), Editable: true, Format: "text"},
			{Key: "subject", Label: "Subject", Value: paramStr(params, "subject"), Editable: true, Format: "text"},
			{Key: "body", Label: "Body", Value: paramStr(params, "body"), Editable: true, Multiline: true, Format: "text"},
		}
		if inReplyTo := paramStr(params, "in_reply_to"); inReplyTo != "" {
			fields = append(fields, adapters.RenderedField{
				Key: "in_reply_to", Label: "In Reply To", Value: inReplyTo, Editable: false, Format: "text",
			})
		}
		return fields, nil
	default:
		return nil, nil
	}
}

// ApplyEdits implements adapters.ApprovalEditor.
func (a *GmailAdapter) ApplyEdits(action string, params map[string]any, edits map[string]string) (map[string]any, error) {
	switch action {
	case "send_message", "create_draft":
		allowed := map[string]bool{"to": true, "subject": true, "body": true}
		for k, v := range edits {
			if !allowed[k] {
				return nil, fmt.Errorf("field %q is not editable", k)
			}
			params[k] = v
		}
		return params, nil
	default:
		return nil, fmt.Errorf("action %q does not support editing", action)
	}
}
