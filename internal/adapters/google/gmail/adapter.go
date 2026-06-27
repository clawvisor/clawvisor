// Package gmail implements the Clawvisor adapter for Gmail.
package gmail

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/clawvisor/clawvisor/internal/adapters/format"
	"github.com/clawvisor/clawvisor/internal/adapters/google/credential"
	"github.com/clawvisor/clawvisor/pkg/adapters"
)

const serviceID = "google.gmail"

const gmailModifyScope = "https://www.googleapis.com/auth/gmail.modify"

// gmailScopes is the full set of scopes Gmail can use. The YAML definition is
// the source of truth for OAuth URL generation and action gating; this list
// is only consumed by the in-process token-source builder below.
var gmailScopes = []string{
	"https://www.googleapis.com/auth/gmail.readonly",
	"https://www.googleapis.com/auth/gmail.send",
	gmailModifyScope,
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
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
	return []string{
		"list_messages", "get_message", "get_thread", "get_attachment",
		"send_message", "create_draft", "archive_message",
	}
}

func (a *GmailAdapter) RequiredScopes() []string { return gmailScopes }

func (a *GmailAdapter) OAuthConfig() *oauth2.Config {
	clientID, clientSecret := a.oauthProvider.OAuthClientCredentials()
	if clientID == "" {
		return nil
	}
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       gmailScopes,
		Endpoint:     google.Endpoint,
	}
}

func (a *GmailAdapter) CredentialFromToken(token *oauth2.Token) ([]byte, error) {
	return credential.FromToken(token, gmailScopes, false)
}

func (a *GmailAdapter) ValidateCredential(credBytes []byte) error {
	return credential.Validate(credBytes)
}

// FetchIdentity returns the Google account email for auto-alias detection.
func (a *GmailAdapter) FetchIdentity(ctx context.Context, credBytes []byte, _ map[string]string) (string, error) {
	client, err := a.httpClient(ctx, credBytes)
	if err != nil {
		return "", err
	}
	return credential.FetchGoogleEmail(ctx, client)
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
	case "get_thread":
		return a.getThread(ctx, client, req.Params)
	case "get_attachment":
		return a.getAttachment(ctx, client, req.Params)
	case "send_message":
		return a.sendMessage(ctx, client, req.Params)
	case "create_draft":
		if err := a.requireModifyScope(req.Credential, "create_draft", "draft permissions"); err != nil {
			return nil, err
		}
		return a.createDraft(ctx, client, req.Params)
	case "archive_message":
		if err := a.requireModifyScope(req.Credential, "archive_message", "label-modification permissions"); err != nil {
			return nil, err
		}
		return a.archiveMessage(ctx, client, req.Params)
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
	ID       string   `json:"id"`
	From     string   `json:"from"`
	Subject  string   `json:"subject"`
	Snippet  string   `json:"snippet"`
	Date     string   `json:"timestamp"`
	IsUnread bool     `json:"is_unread"`
	Labels   []string `json:"labels,omitempty"`
}

func (a *GmailAdapter) listMessages(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	query, _ := params["query"].(string)
	maxResults := 10
	if v, ok := paramInt(params, "max_results"); ok {
		if v > 0 && v <= 200 {
			maxResults = v
		}
	}

	u := fmt.Sprintf("https://gmail.googleapis.com/gmail/v1/users/me/messages?maxResults=%d", maxResults)
	if query != "" {
		u += "&q=" + encodeParam(query)
	}
	if pt, _ := params["page_token"].(string); pt != "" {
		u += "&pageToken=" + encodeParam(pt)
	}

	var listResp struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
		NextPageToken      string `json:"nextPageToken"`
		ResultSizeEstimate int    `json:"resultSizeEstimate"`
	}
	if err := gmailGET(ctx, client, u, &listResp); err != nil {
		return nil, fmt.Errorf("gmail list_messages: %w", err)
	}

	labels := newLabelResolver(ctx, client)
	items := make([]msgListItem, 0, len(listResp.Messages))
	unread := 0

	ids := make([]string, len(listResp.Messages))
	for i, m := range listResp.Messages {
		ids[i] = m.ID
	}
	metas, metaErrs := fetchMessageMetasBatch(ctx, client, ids)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var firstFetchErr error
	for i, meta := range metas {
		if metaErrs[i] != nil {
			if firstFetchErr == nil {
				firstFetchErr = metaErrs[i]
			}
			continue
		}
		item := msgListItem{
			ID:       ids[i],
			From:     format.SanitizeHeader(meta.from, format.MaxFieldLen),
			Subject:  format.SanitizeText(meta.subject, format.MaxFieldLen),
			Snippet:  format.SanitizeText(meta.snippet, format.MaxSnippetLen),
			Date:     meta.date,
			IsUnread: meta.isUnread,
			Labels:   labels.resolve(meta.labelIDs),
		}
		items = append(items, item)
		if meta.isUnread {
			unread++
		}
	}

	if len(ids) > 0 && len(items) == 0 && firstFetchErr != nil {
		return nil, fmt.Errorf("gmail list_messages: all metadata fetches failed: %w", firstFetchErr)
	}

	total := listResp.ResultSizeEstimate
	summary := format.Summary("%d messages (%d unread)", len(items), unread)
	if total > len(items) {
		summary = format.Summary("%d of ~%d messages (%d unread)", len(items), total, unread)
	}

	result := &adapters.Result{Summary: summary, Data: map[string]any{"messages": items}}
	if listResp.NextPageToken != "" {
		result.Meta = map[string]any{"next_page_token": listResp.NextPageToken}
	}
	return result, nil
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
	Cc          string           `json:"cc,omitempty"`
	ReplyTo     string           `json:"reply_to,omitempty"`
	Subject     string           `json:"subject"`
	Date        string           `json:"timestamp"`
	Body        string           `json:"body"`
	IsUnread    bool             `json:"is_unread"`
	ThreadID    string           `json:"thread_id"`
	MessageID   string           `json:"message_id_header,omitempty"`
	References  string           `json:"references,omitempty"`
	Labels      []string         `json:"labels,omitempty"`
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

	detail := parseMessageDetail(raw, newLabelResolver(ctx, client))

	summary := format.Summary("Email from %s: %q", detail.From, detail.Subject)
	if len(detail.Attachments) > 0 {
		summary = format.Summary("Email from %s: %q (%d attachments)", detail.From, detail.Subject, len(detail.Attachments))
	}
	return &adapters.Result{Summary: summary, Data: detail}, nil
}

// ── get_thread ────────────────────────────────────────────────────────────────

type threadDetail struct {
	ThreadID string      `json:"thread_id"`
	Messages []msgDetail `json:"messages"`
}

func (a *GmailAdapter) getThread(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	threadID, _ := params["thread_id"].(string)
	if threadID == "" {
		return nil, fmt.Errorf("gmail get_thread: thread_id is required")
	}

	url := fmt.Sprintf("https://gmail.googleapis.com/gmail/v1/users/me/threads/%s?format=full", threadID)
	var raw struct {
		ID       string         `json:"id"`
		Messages []gmailMessage `json:"messages"`
	}
	if err := gmailGET(ctx, client, url, &raw); err != nil {
		return nil, fmt.Errorf("gmail get_thread: %w", err)
	}

	labels := newLabelResolver(ctx, client)
	messages := make([]msgDetail, 0, len(raw.Messages))
	for _, msg := range raw.Messages {
		detail := parseMessageDetail(msg, labels)
		messages = append(messages, detail)
	}

	result := threadDetail{
		ThreadID: raw.ID,
		Messages: messages,
	}

	summary := format.Summary("Thread %s: %d messages", threadID, len(messages))
	if len(messages) > 0 {
		summary = format.Summary("Thread %s: %d messages (subject: %q)", threadID, len(messages), messages[0].Subject)
	}
	return &adapters.Result{Summary: summary, Data: result}, nil
}

// parseMessageDetail extracts a msgDetail from a raw gmailMessage. If
// labels is non-nil, opaque label IDs are resolved to display names.
func parseMessageDetail(raw gmailMessage, labels *labelResolver) msgDetail {
	var from, to, cc, replyTo, subject, date, messageID, references string
	for _, h := range raw.Payload.Headers {
		switch strings.ToLower(h.Name) {
		case "from":
			from = h.Value
		case "to":
			to = h.Value
		case "cc":
			cc = h.Value
		case "reply-to":
			replyTo = h.Value
		case "subject":
			subject = h.Value
		case "date":
			date = h.Value
		case "message-id":
			messageID = h.Value
		case "references":
			references = h.Value
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

	return msgDetail{
		ID:          raw.ID,
		From:        format.SanitizeHeader(from, format.MaxFieldLen),
		To:          format.SanitizeHeader(to, format.MaxFieldLen),
		Cc:          format.SanitizeHeader(cc, format.MaxFieldLen),
		ReplyTo:     format.SanitizeHeader(replyTo, format.MaxFieldLen),
		Subject:     format.SanitizeText(subject, format.MaxFieldLen),
		Date:        date,
		Body:        format.SanitizeText(body, format.MaxBodyLen),
		IsUnread:    isUnread,
		ThreadID:    raw.ThreadId,
		MessageID:   messageID,
		References:  references,
		Labels:      labels.resolve(raw.LabelIds),
		Attachments: attachments,
	}
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
	cc, _ := params["cc"].(string)
	bcc, _ := params["bcc"].(string)
	subject, _ := params["subject"].(string)
	body, _ := params["body"].(string)
	threadID, _ := params["thread_id"].(string)
	legacyInReplyTo, _ := params["in_reply_to"].(string)

	// Resolve thread context for replies. Prefer thread_id (full context);
	// fall back to in_reply_to (legacy — search for the referenced message
	// to discover its thread).
	var inReplyTo, references, quotedBody, htmlBody string

	// If we have in_reply_to but no thread_id, search for the message to
	// discover its thread_id so we get full threading + quoting.
	if threadID == "" && legacyInReplyTo != "" {
		if tid := resolveThreadFromMessageID(ctx, client, legacyInReplyTo); tid != "" {
			threadID = tid
		} else {
			// Can't find the thread — use the Message-ID as a bare In-Reply-To header.
			inReplyTo = legacyInReplyTo
		}
	}

	if threadID != "" {
		threadURL := fmt.Sprintf("https://gmail.googleapis.com/gmail/v1/users/me/threads/%s?format=full", threadID)
		var threadResp struct {
			ID       string         `json:"id"`
			Messages []gmailMessage `json:"messages"`
		}
		if err := gmailGET(ctx, client, threadURL, &threadResp); err != nil {
			return nil, fmt.Errorf("gmail send_message: fetch thread: %w", err)
		}
		if len(threadResp.Messages) == 0 {
			return nil, fmt.Errorf("gmail send_message: thread %s has no messages", threadID)
		}

		lastMsg := threadResp.Messages[len(threadResp.Messages)-1]
		lastDetail := parseMessageDetail(lastMsg, nil)

		// Derive recipient from thread if not explicitly provided.
		if to == "" {
			to = lastDetail.ReplyTo
		}
		if to == "" {
			to = lastDetail.From
		}

		// Derive subject from thread if not explicitly provided.
		if subject == "" {
			subject = lastDetail.Subject
			if !strings.HasPrefix(strings.ToLower(subject), "re:") {
				subject = "Re: " + subject
			}
		}

		// Build threading headers.
		inReplyTo = lastDetail.MessageID
		references = lastDetail.References
		if lastDetail.MessageID != "" {
			if references != "" {
				references += " " + lastDetail.MessageID
			} else {
				references = lastDetail.MessageID
			}
		}

		// Quote the previous message.
		if lastDetail.Body != "" {
			var qb strings.Builder
			qb.WriteString("\r\n\r\nOn ")
			qb.WriteString(lastDetail.Date)
			qb.WriteString(", ")
			qb.WriteString(lastDetail.From)
			qb.WriteString(" wrote:\r\n")
			for _, line := range strings.Split(lastDetail.Body, "\n") {
				qb.WriteString("> ")
				qb.WriteString(strings.TrimRight(line, "\r"))
				qb.WriteString("\r\n")
			}
			quotedBody = qb.String()
			htmlBody = buildReplyHTML(body, lastDetail.Date, lastDetail.From, lastDetail.Body)
		}
	}

	if to == "" {
		return nil, fmt.Errorf("gmail send_message: to is required")
	}
	if subject == "" {
		return nil, fmt.Errorf("gmail send_message: subject is required")
	}

	// Fetch the sender's address for a proper From header.
	from, _ := fetchSenderAddress(ctx, client)

	fullBody := body + quotedBody
	raw := buildMIMEMessage(from, to, cc, bcc, subject, fullBody, htmlBody, inReplyTo, references)
	encoded := base64.RawURLEncoding.EncodeToString([]byte(raw))

	payload := map[string]string{"raw": encoded}
	if threadID != "" {
		payload["threadId"] = threadID
	}

	var sendResp struct {
		ID string `json:"id"`
	}
	if err := gmailPOST(ctx, client, "https://gmail.googleapis.com/gmail/v1/users/me/messages/send",
		payload, &sendResp); err != nil {
		return nil, fmt.Errorf("gmail send_message: %w", err)
	}

	result := map[string]string{
		"message_id": sendResp.ID,
		"to":         to,
		"subject":    subject,
	}
	if threadID != "" {
		result["thread_id"] = threadID
	}
	summary := format.Summary("Email sent to %s (subject: %q)", to, subject)
	return &adapters.Result{Summary: summary, Data: result}, nil
}

// requireModifyScope checks whether the stored credential includes the
// gmail.modify scope. Legacy tokens lacking it will fail with a descriptive
// error prompting the user to reconnect.
func (a *GmailAdapter) requireModifyScope(credBytes []byte, action, permissionDescription string) error {
	cred, err := credential.Parse(credBytes)
	if err != nil {
		return fmt.Errorf("gmail %s: %w", action, err)
	}
	if !credential.HasAllScopes(cred.Scopes, []string{gmailModifyScope}) {
		return fmt.Errorf("gmail %s: the gmail.modify scope is required — please reconnect your Google account to grant %s", action, permissionDescription)
	}
	return nil
}

// ── create_draft ──────────────────────────────────────────────────────────────

func (a *GmailAdapter) createDraft(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	to, _ := params["to"].(string)
	cc, _ := params["cc"].(string)
	bcc, _ := params["bcc"].(string)
	subject, _ := params["subject"].(string)
	body, _ := params["body"].(string)
	threadID, _ := params["thread_id"].(string)
	legacyInReplyTo, _ := params["in_reply_to"].(string)

	// Resolve thread context for draft replies — same logic as sendMessage.
	var inReplyTo, references, quotedBody, htmlBody string

	if threadID == "" && legacyInReplyTo != "" {
		if tid := resolveThreadFromMessageID(ctx, client, legacyInReplyTo); tid != "" {
			threadID = tid
		} else {
			inReplyTo = legacyInReplyTo
		}
	}

	if threadID != "" {
		threadURL := fmt.Sprintf("https://gmail.googleapis.com/gmail/v1/users/me/threads/%s?format=full", threadID)
		var threadResp struct {
			ID       string         `json:"id"`
			Messages []gmailMessage `json:"messages"`
		}
		if err := gmailGET(ctx, client, threadURL, &threadResp); err != nil {
			return nil, fmt.Errorf("gmail create_draft: fetch thread: %w", err)
		}
		if len(threadResp.Messages) == 0 {
			return nil, fmt.Errorf("gmail create_draft: thread %s has no messages", threadID)
		}

		lastMsg := threadResp.Messages[len(threadResp.Messages)-1]
		lastDetail := parseMessageDetail(lastMsg, nil)

		if to == "" {
			to = lastDetail.ReplyTo
		}
		if to == "" {
			to = lastDetail.From
		}

		if subject == "" {
			subject = lastDetail.Subject
			if !strings.HasPrefix(strings.ToLower(subject), "re:") {
				subject = "Re: " + subject
			}
		}

		inReplyTo = lastDetail.MessageID
		references = lastDetail.References
		if lastDetail.MessageID != "" {
			if references != "" {
				references += " " + lastDetail.MessageID
			} else {
				references = lastDetail.MessageID
			}
		}

		if lastDetail.Body != "" {
			var qb strings.Builder
			qb.WriteString("\r\n\r\nOn ")
			qb.WriteString(lastDetail.Date)
			qb.WriteString(", ")
			qb.WriteString(lastDetail.From)
			qb.WriteString(" wrote:\r\n")
			for _, line := range strings.Split(lastDetail.Body, "\n") {
				qb.WriteString("> ")
				qb.WriteString(strings.TrimRight(line, "\r"))
				qb.WriteString("\r\n")
			}
			quotedBody = qb.String()
			htmlBody = buildReplyHTML(body, lastDetail.Date, lastDetail.From, lastDetail.Body)
		}
	}

	if to == "" {
		return nil, fmt.Errorf("gmail create_draft: to is required")
	}
	if subject == "" {
		return nil, fmt.Errorf("gmail create_draft: subject is required")
	}

	from, _ := fetchSenderAddress(ctx, client)

	fullBody := body + quotedBody
	raw := buildMIMEMessage(from, to, cc, bcc, subject, fullBody, htmlBody, inReplyTo, references)
	encoded := base64.RawURLEncoding.EncodeToString([]byte(raw))

	msg := map[string]string{"raw": encoded}
	if threadID != "" {
		msg["threadId"] = threadID
	}
	payload := map[string]any{
		"message": msg,
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
	if threadID != "" {
		result["thread_id"] = threadID
	}
	summary := format.Summary("Draft created for %s (subject: %q)", to, subject)
	return &adapters.Result{Summary: summary, Data: result}, nil
}

// ── archive_message ───────────────────────────────────────────────────────────

func (a *GmailAdapter) archiveMessage(ctx context.Context, client *http.Client, params map[string]any) (*adapters.Result, error) {
	msgID, _ := params["message_id"].(string)
	if msgID == "" {
		return nil, fmt.Errorf("gmail archive_message: message_id is required")
	}

	url := fmt.Sprintf("https://gmail.googleapis.com/gmail/v1/users/me/messages/%s/modify", msgID)
	payload := map[string][]string{"removeLabelIds": {"INBOX"}}

	var modifyResp struct {
		ID       string   `json:"id"`
		ThreadId string   `json:"threadId"`
		LabelIds []string `json:"labelIds"`
	}
	if err := gmailPOST(ctx, client, url, payload, &modifyResp); err != nil {
		return nil, fmt.Errorf("gmail archive_message: %w", err)
	}

	result := map[string]any{
		"message_id": modifyResp.ID,
		"thread_id":  modifyResp.ThreadId,
		"labels":     modifyResp.LabelIds,
	}
	summary := format.Summary("Archived message %s", msgID)
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
		return fmt.Errorf("gmail API %s: %d: %s", url, resp.StatusCode, format.Truncate(string(body), 200))
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
		return fmt.Errorf("gmail API POST %s: %d: %s", url, resp.StatusCode, format.Truncate(string(body), 200))
	}
	return json.Unmarshal(body, out)
}

// resolveThreadFromMessageID searches for a message by its RFC 2822 Message-ID
// header (e.g. "<abc@mail.gmail.com>") and returns the Gmail thread ID.
// Returns "" if the message can't be found.
func resolveThreadFromMessageID(ctx context.Context, client *http.Client, messageID string) string {
	// If the value doesn't contain angle brackets it's likely a Gmail API
	// message ID (e.g. "19abc123def") rather than an RFC 2822 Message-ID
	// (e.g. "<CABx...@mail.gmail.com>"). Agents commonly pass the API ID
	// since that's the `id` field returned by get_message/list_messages.
	if !strings.Contains(messageID, "<") {
		u := fmt.Sprintf("https://gmail.googleapis.com/gmail/v1/users/me/messages/%s?format=minimal", messageID)
		var msg struct {
			ThreadId string `json:"threadId"`
		}
		if err := gmailGET(ctx, client, u, &msg); err == nil && msg.ThreadId != "" {
			return msg.ThreadId
		}
	}

	// Fall back to rfc822msgid: search for proper RFC 2822 Message-IDs.
	query := "rfc822msgid:" + messageID
	url := fmt.Sprintf("https://gmail.googleapis.com/gmail/v1/users/me/messages?maxResults=1&q=%s", encodeParam(query))
	var resp struct {
		Messages []struct {
			ID       string `json:"id"`
			ThreadId string `json:"threadId"`
		} `json:"messages"`
	}
	if err := gmailGET(ctx, client, url, &resp); err != nil {
		return ""
	}
	if len(resp.Messages) == 0 {
		return ""
	}
	return resp.Messages[0].ThreadId
}

// fetchSenderAddress resolves the authenticated user's display name and email
// for the From header (e.g. "Eric Levine <eric@clawvisor.com>").
//
// Strategy:
//  1. Try Gmail sendAs settings (has display name + email together).
//  2. If sendAs returns a bare email (no display name), supplement with Google
//     userinfo which reliably has the account name.
//  3. Returns "" on complete failure so callers can fall back to "me".
func fetchSenderAddress(ctx context.Context, client *http.Client) (string, error) {
	var email, displayName string

	// Try sendAs first — it has the canonical send-from address.
	var sendAsResp struct {
		SendAs []struct {
			SendAsEmail string `json:"sendAsEmail"`
			DisplayName string `json:"displayName"`
			IsDefault   bool   `json:"isDefault"`
			IsPrimary   bool   `json:"isPrimary"`
		} `json:"sendAs"`
	}
	if err := gmailGET(ctx, client, "https://gmail.googleapis.com/gmail/v1/users/me/settings/sendAs", &sendAsResp); err == nil {
		for _, sa := range sendAsResp.SendAs {
			if sa.IsDefault {
				email = sa.SendAsEmail
				displayName = sa.DisplayName
				break
			}
		}
		if email == "" && len(sendAsResp.SendAs) > 0 {
			email = sendAsResp.SendAs[0].SendAsEmail
			displayName = sendAsResp.SendAs[0].DisplayName
		}
	}

	// If we still don't have a display name, try Google userinfo.
	if displayName == "" {
		if name, addr, err := fetchGoogleProfile(ctx, client); err == nil {
			displayName = name
			if email == "" {
				email = addr
			}
		}
	}

	if email == "" {
		return "", fmt.Errorf("could not determine sender address")
	}
	if displayName != "" {
		return fmt.Sprintf("%s <%s>", displayName, email), nil
	}
	return email, nil
}

// fetchGoogleProfile returns the user's name and email from the Google
// userinfo endpoint (requires the userinfo.email scope we already request).
func fetchGoogleProfile(ctx context.Context, client *http.Client) (name, email string, err error) {
	var info struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := gmailGET(ctx, client, "https://www.googleapis.com/oauth2/v2/userinfo", &info); err != nil {
		return "", "", err
	}
	return info.Name, info.Email, nil
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

// labelResolver returns display names for a slice of Gmail label IDs. System
// labels (INBOX, UNREAD, IMPORTANT, STARRED, …) are already human-readable;
// only user-created labels arrive as opaque IDs like "Label_4596042054184368034".
// The labels.list API call is deferred until a message actually contains an
// opaque ID, and is then memoized for the rest of the call.
type labelResolver struct {
	ctx    context.Context
	client *http.Client
	loaded bool
	m      map[string]string
}

func newLabelResolver(ctx context.Context, client *http.Client) *labelResolver {
	return &labelResolver{ctx: ctx, client: client}
}

func (r *labelResolver) resolve(ids []string) []string {
	if len(ids) == 0 || r == nil {
		return nil
	}
	needsLookup := false
	for _, id := range ids {
		if strings.HasPrefix(id, "Label_") {
			needsLookup = true
			break
		}
	}
	if needsLookup && !r.loaded {
		r.m = fetchLabelMap(r.ctx, r.client)
		r.loaded = true
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		if name, ok := r.m[id]; ok {
			out[i] = name
		} else {
			out[i] = id
		}
	}
	return out
}

// fetchLabelMap returns a map of Gmail label ID → display name. Returns nil
// on error so callers can degrade gracefully (opaque IDs will fall through).
func fetchLabelMap(ctx context.Context, client *http.Client) map[string]string {
	var raw struct {
		Labels []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := gmailGET(ctx, client, "https://gmail.googleapis.com/gmail/v1/users/me/labels", &raw); err != nil {
		return nil
	}
	m := make(map[string]string, len(raw.Labels))
	for _, l := range raw.Labels {
		m[l.ID] = l.Name
	}
	return m
}

// ── Message parsing helpers ───────────────────────────────────────────────────

type msgMeta struct {
	from, subject, snippet, date string
	isUnread                     bool
	labelIDs                     []string
}

// gmailBatchMaxRequests is Gmail's documented per-batch cap.
const gmailBatchMaxRequests = 100

// fetchMessageMetasBatch fetches per-message metadata for the given ids using
// Gmail's multipart/mixed batch endpoint, returning aligned slices of metas
// and errors. A non-nil errs[i] means the i'th id could not be resolved;
// callers tolerate this the same way they did the old per-call fan-out.
//
// Batches are sized to gmailBatchMaxRequests and processed sequentially.
// A request-level failure (network / 5xx on the outer POST) fills the same
// error across every slot in that batch.
func fetchMessageMetasBatch(ctx context.Context, client *http.Client, ids []string) ([]msgMeta, []error) {
	metas := make([]msgMeta, len(ids))
	errs := make([]error, len(ids))
	for start := 0; start < len(ids); start += gmailBatchMaxRequests {
		end := start + gmailBatchMaxRequests
		if end > len(ids) {
			end = len(ids)
		}
		chunkMetas, chunkErrs := fetchMessageMetasBatchOne(ctx, client, ids[start:end])
		copy(metas[start:end], chunkMetas)
		copy(errs[start:end], chunkErrs)
	}
	return metas, errs
}

// fetchMessageMetasBatchOne issues a single multipart/mixed POST to
// /batch/gmail/v1 for up to gmailBatchMaxRequests ids and parses the response.
func fetchMessageMetasBatchOne(ctx context.Context, client *http.Client, ids []string) ([]msgMeta, []error) {
	metas := make([]msgMeta, len(ids))
	errs := make([]error, len(ids))

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for i, id := range ids {
		subHeader := textproto.MIMEHeader{}
		subHeader.Set("Content-Type", "application/http")
		subHeader.Set("Content-ID", fmt.Sprintf("<item-%d>", i))
		part, err := mw.CreatePart(subHeader)
		if err != nil {
			fillErrs(errs, fmt.Errorf("gmail batch build: %w", err))
			return metas, errs
		}
		sub := fmt.Sprintf(
			"GET /gmail/v1/users/me/messages/%s?format=metadata&metadataHeaders=From&metadataHeaders=Subject&metadataHeaders=Date HTTP/1.1\r\n\r\n",
			id,
		)
		if _, err := io.WriteString(part, sub); err != nil {
			fillErrs(errs, fmt.Errorf("gmail batch build: %w", err))
			return metas, errs
		}
	}
	if err := mw.Close(); err != nil {
		fillErrs(errs, fmt.Errorf("gmail batch build: %w", err))
		return metas, errs
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://www.googleapis.com/batch/gmail/v1", &body)
	if err != nil {
		fillErrs(errs, err)
		return metas, errs
	}
	req.Header.Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())

	resp, err := client.Do(req)
	if err != nil {
		fillErrs(errs, err)
		return metas, errs
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		fillErrs(errs, fmt.Errorf("gmail batch: %d: %s", resp.StatusCode, format.Truncate(string(respBody), 200)))
		return metas, errs
	}

	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		fillErrs(errs, fmt.Errorf("gmail batch parse content-type: %w", err))
		return metas, errs
	}
	boundary, ok := params["boundary"]
	if !ok {
		fillErrs(errs, fmt.Errorf("gmail batch: missing boundary in response"))
		return metas, errs
	}

	// Pre-fill errs so any id whose response is missing or has an
	// unparseable Content-ID surfaces as an explicit error rather than
	// silently degrading to a zero-valued msgMeta that listMessages would
	// then emit as an empty/incorrect message. Successful sub-responses
	// clear their slot below.
	for i := range errs {
		errs[i] = fmt.Errorf("gmail batch: no sub-response received for id %q", ids[i])
	}

	mr := multipart.NewReader(bytes.NewReader(respBody), boundary)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			fillErrs(errs, fmt.Errorf("gmail batch read part: %w", err))
			return metas, errs
		}
		idx, idxOK := parseBatchContentIndex(part.Header.Get("Content-ID"))
		partBody, readErr := io.ReadAll(part)
		part.Close()
		if !idxOK || idx < 0 || idx >= len(ids) {
			continue
		}
		if readErr != nil {
			errs[idx] = fmt.Errorf("gmail batch read part body: %w", readErr)
			continue
		}
		meta, err := parseBatchSubResponse(partBody)
		if err != nil {
			errs[idx] = err
			continue
		}
		metas[idx] = meta
		errs[idx] = nil
	}
	return metas, errs
}

// parseBatchContentIndex recovers the request index from a batch part's
// Content-ID header. Gmail echoes the client-supplied id prefixed with
// "response-", so both "<item-3>" and "<response-item-3>" parse to 3.
func parseBatchContentIndex(contentID string) (int, bool) {
	s := strings.TrimPrefix(strings.TrimSuffix(strings.TrimPrefix(contentID, "<"), ">"), "response-")
	s = strings.TrimPrefix(s, "item-")
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseBatchSubResponse decodes one application/http sub-response body into
// an msgMeta, mirroring the prior per-id fetch's status check and JSON shape.
func parseBatchSubResponse(body []byte) (msgMeta, error) {
	sub, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(body)), nil)
	if err != nil {
		return msgMeta{}, fmt.Errorf("gmail batch parse sub-response: %w", err)
	}
	defer sub.Body.Close()
	subBody, _ := io.ReadAll(sub.Body)
	if sub.StatusCode >= 400 {
		return msgMeta{}, fmt.Errorf("gmail batch sub: %d: %s", sub.StatusCode, format.Truncate(string(subBody), 200))
	}
	var raw struct {
		Snippet  string   `json:"snippet"`
		LabelIds []string `json:"labelIds"`
		Payload  struct {
			Headers []gmailHeader `json:"headers"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(subBody, &raw); err != nil {
		return msgMeta{}, fmt.Errorf("gmail batch sub decode: %w", err)
	}
	meta := msgMeta{snippet: raw.Snippet, labelIDs: raw.LabelIds}
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

func fillErrs(errs []error, err error) {
	for i := range errs {
		errs[i] = err
	}
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
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		b, err = base64.URLEncoding.DecodeString(s)
	}
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(s)
	}
	if err != nil {
		b, err = base64.StdEncoding.DecodeString(s)
	}
	if err != nil {
		return ""
	}
	return string(b)
}

func buildMIMEMessage(from, to, cc, bcc, subject, body, htmlBody, inReplyTo, references string) string {
	var sb strings.Builder
	if from != "" {
		sb.WriteString("From: " + from + "\r\n")
	} else {
		sb.WriteString("From: me\r\n")
	}
	sb.WriteString("To: " + to + "\r\n")
	if cc != "" {
		sb.WriteString("Cc: " + cc + "\r\n")
	}
	if bcc != "" {
		sb.WriteString("Bcc: " + bcc + "\r\n")
	}
	sb.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n")
	if inReplyTo != "" {
		sb.WriteString("In-Reply-To: " + inReplyTo + "\r\n")
	}
	if references != "" {
		sb.WriteString("References: " + references + "\r\n")
	}
	sb.WriteString("MIME-Version: 1.0\r\n")
	if htmlBody != "" {
		boundary := "clawvisor-alt"
		sb.WriteString(`Content-Type: multipart/alternative; boundary="` + boundary + `"` + "\r\n")
		sb.WriteString("\r\n")
		sb.WriteString("--" + boundary + "\r\n")
		sb.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
		sb.WriteString("Content-Transfer-Encoding: quoted-printable\r\n")
		sb.WriteString("\r\n")
		sb.WriteString(quotePrintable(body))
		sb.WriteString("\r\n--" + boundary + "\r\n")
		sb.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
		sb.WriteString("Content-Transfer-Encoding: quoted-printable\r\n")
		sb.WriteString("\r\n")
		sb.WriteString(quotePrintable(htmlBody))
		sb.WriteString("\r\n--" + boundary + "--\r\n")
		return sb.String()
	}
	sb.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	sb.WriteString("\r\n")
	sb.WriteString(body)
	return sb.String()
}

func buildReplyHTML(body, date, from, quotedText string) string {
	var sb strings.Builder
	sb.WriteString(`<div dir="ltr">`)
	sb.WriteString(textToHTML(body))
	sb.WriteString(`</div><br><div class="gmail_quote gmail_quote_container"><div dir="ltr" class="gmail_attr">On `)
	sb.WriteString(html.EscapeString(date))
	sb.WriteString(`, `)
	sb.WriteString(html.EscapeString(from))
	sb.WriteString(` wrote:<br></div><blockquote class="gmail_quote" style="margin:0px 0px 0px 0.8ex;border-left:1px solid rgb(204,204,204);padding-left:1ex">`)
	sb.WriteString(textToHTML(quotedText))
	sb.WriteString(`</blockquote></div>`)
	return sb.String()
}

func textToHTML(s string) string {
	return strings.ReplaceAll(html.EscapeString(s), "\n", "<br>\n")
}

func quotePrintable(s string) string {
	var buf bytes.Buffer
	w := quotedprintable.NewWriter(&buf)
	_, _ = io.WriteString(w, strings.ReplaceAll(s, "\n", "\r\n"))
	_ = w.Close()
	return buf.String()
}

func encodeParam(s string) string {
	return url.QueryEscape(s)
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
