package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"golang.org/x/oauth2"

	"github.com/clawvisor/clawvisor/internal/adapters/google/credential"
)

func b64(s string) string    { return base64.URLEncoding.EncodeToString([]byte(s)) }
func rawB64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testGoogleCredential(t *testing.T, scopes ...string) []byte {
	t.Helper()
	cred, err := credential.FromToken(&oauth2.Token{AccessToken: "access-token"}, scopes, true)
	if err != nil {
		t.Fatalf("credential.FromToken: %v", err)
	}
	return cred
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func TestRequiredScopesUseModifyForDrafts(t *testing.T) {
	adapter := New(nil)
	scopes := adapter.RequiredScopes()

	if !containsString(scopes, gmailModifyScope) {
		t.Fatalf("RequiredScopes() missing gmail.modify: %v", scopes)
	}
	if containsString(scopes, "https://www.googleapis.com/auth/gmail.compose") {
		t.Fatalf("RequiredScopes() should not request gmail.compose: %v", scopes)
	}
}

func TestRequireModifyScopeForDrafts(t *testing.T) {
	adapter := New(nil)

	if err := adapter.requireModifyScope(testGoogleCredential(t, gmailModifyScope), "create_draft", "draft permissions"); err != nil {
		t.Fatalf("requireModifyScope with gmail.modify: %v", err)
	}

	err := adapter.requireModifyScope(testGoogleCredential(t, "https://www.googleapis.com/auth/gmail.compose"), "create_draft", "draft permissions")
	if err == nil {
		t.Fatal("expected error when gmail.modify is missing")
	}
	msg := err.Error()
	if !strings.Contains(msg, "gmail create_draft") || !strings.Contains(msg, "gmail.modify") || !strings.Contains(msg, "draft permissions") {
		t.Fatalf("error = %q, want create_draft gmail.modify draft permission guidance", msg)
	}
}

func TestExtractBodyFromParts_DirectPlainText(t *testing.T) {
	payload := gmailPayload{
		MimeType: "text/plain",
		Body:     gmailBody{Data: b64("Hello, world!")},
	}
	got := extractBodyFromParts(payload)
	if got != "Hello, world!" {
		t.Errorf("got %q, want %q", got, "Hello, world!")
	}
}

func TestExtractBodyFromParts_DirectPlainTextRawURLBase64(t *testing.T) {
	payload := gmailPayload{
		MimeType: "text/plain",
		Body:     gmailBody{Data: rawB64("Hello, raw world!")},
	}
	got := extractBodyFromParts(payload)
	if got != "Hello, raw world!" {
		t.Errorf("got %q, want %q", got, "Hello, raw world!")
	}
}

func TestExtractBodyFromParts_DirectHTML(t *testing.T) {
	payload := gmailPayload{
		MimeType: "text/html",
		Body:     gmailBody{Data: b64("<p>Hello</p>")},
	}
	got := extractBodyFromParts(payload)
	if got != "Hello" {
		t.Errorf("got %q, want %q", got, "Hello")
	}
}

func TestExtractBodyFromParts_MultipartPreferPlain(t *testing.T) {
	payload := gmailPayload{
		MimeType: "multipart/alternative",
		Parts: []gmailPart{
			{MimeType: "text/plain", Body: gmailBody{Data: b64("plain text")}},
			{MimeType: "text/html", Body: gmailBody{Data: b64("<b>html</b>")}},
		},
	}
	got := extractBodyFromParts(payload)
	if got != "plain text" {
		t.Errorf("got %q, want %q", got, "plain text")
	}
}

func TestExtractBodyFromParts_NestedMultipart(t *testing.T) {
	payload := gmailPayload{
		MimeType: "multipart/mixed",
		Parts: []gmailPart{
			{
				MimeType: "multipart/alternative",
				Parts: []gmailPart{
					{MimeType: "text/plain", Body: gmailBody{Data: b64("nested plain")}},
					{MimeType: "text/html", Body: gmailBody{Data: b64("<p>nested html</p>")}},
				},
			},
			{
				MimeType: "application/pdf",
				Filename: "receipt.pdf",
				Body:     gmailBody{AttachmentID: "abc123", Size: 5000},
			},
		},
	}
	got := extractBodyFromParts(payload)
	if got != "nested plain" {
		t.Errorf("got %q, want %q", got, "nested plain")
	}
}

func TestExtractBodyFromParts_HTMLFallback(t *testing.T) {
	payload := gmailPayload{
		MimeType: "multipart/alternative",
		Parts: []gmailPart{
			{MimeType: "text/html", Body: gmailBody{Data: b64("<div>only html</div>")}},
		},
	}
	got := extractBodyFromParts(payload)
	if got != "only html" {
		t.Errorf("got %q, want %q", got, "only html")
	}
}

func TestExtractBodyFromParts_Empty(t *testing.T) {
	payload := gmailPayload{MimeType: "multipart/mixed"}
	got := extractBodyFromParts(payload)
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestExtractAttachments_None(t *testing.T) {
	payload := gmailPayload{
		MimeType: "text/plain",
		Body:     gmailBody{Data: b64("no attachments")},
	}
	got := extractAttachments(payload)
	if len(got) != 0 {
		t.Errorf("expected no attachments, got %d", len(got))
	}
}

func TestExtractAttachments_SingleAttachment(t *testing.T) {
	payload := gmailPayload{
		MimeType: "multipart/mixed",
		Parts: []gmailPart{
			{MimeType: "text/plain", Body: gmailBody{Data: b64("body")}},
			{
				MimeType: "application/pdf",
				Filename: "invoice.pdf",
				Body:     gmailBody{AttachmentID: "att-1", Size: 12345},
			},
		},
	}
	got := extractAttachments(payload)
	if len(got) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(got))
	}
	if got[0].AttachmentID != "att-1" {
		t.Errorf("attachment_id = %q, want %q", got[0].AttachmentID, "att-1")
	}
	if got[0].Filename != "invoice.pdf" {
		t.Errorf("filename = %q, want %q", got[0].Filename, "invoice.pdf")
	}
	if got[0].MimeType != "application/pdf" {
		t.Errorf("mime_type = %q, want %q", got[0].MimeType, "application/pdf")
	}
	if got[0].Size != 12345 {
		t.Errorf("size = %d, want %d", got[0].Size, 12345)
	}
}

func TestExtractAttachments_MultipleAndNested(t *testing.T) {
	payload := gmailPayload{
		MimeType: "multipart/mixed",
		Parts: []gmailPart{
			{
				MimeType: "multipart/alternative",
				Parts: []gmailPart{
					{MimeType: "text/plain", Body: gmailBody{Data: b64("body")}},
				},
			},
			{
				MimeType: "image/png",
				Filename: "photo.png",
				Body:     gmailBody{AttachmentID: "att-1", Size: 1000},
			},
			{
				MimeType: "application/zip",
				Filename: "archive.zip",
				Body:     gmailBody{AttachmentID: "att-2", Size: 50000},
			},
		},
	}
	got := extractAttachments(payload)
	if len(got) != 2 {
		t.Fatalf("expected 2 attachments, got %d", len(got))
	}
	if got[0].Filename != "photo.png" {
		t.Errorf("first attachment = %q, want %q", got[0].Filename, "photo.png")
	}
	if got[1].Filename != "archive.zip" {
		t.Errorf("second attachment = %q, want %q", got[1].Filename, "archive.zip")
	}
}

func TestExtractAttachments_SkipsPartsWithoutAttachmentID(t *testing.T) {
	// Inline images may have a filename but no attachmentId when content is inline
	payload := gmailPayload{
		MimeType: "multipart/mixed",
		Parts: []gmailPart{
			{MimeType: "text/plain", Body: gmailBody{Data: b64("body")}},
			{
				MimeType: "image/png",
				Filename: "inline.png",
				Body:     gmailBody{Data: b64("inline-data")}, // no AttachmentID
			},
			{
				MimeType: "application/pdf",
				Filename: "real.pdf",
				Body:     gmailBody{AttachmentID: "att-1", Size: 9999},
			},
		},
	}
	got := extractAttachments(payload)
	if len(got) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(got))
	}
	if got[0].Filename != "real.pdf" {
		t.Errorf("attachment = %q, want %q", got[0].Filename, "real.pdf")
	}
}

func TestParseMessageDetail_ExtractsAllHeaders(t *testing.T) {
	msg := gmailMessage{
		ID:       "msg-1",
		ThreadId: "thread-1",
		LabelIds: []string{"INBOX", "UNREAD"},
		Payload: gmailPayload{
			MimeType: "text/plain",
			Headers: []gmailHeader{
				{Name: "From", Value: "alice@example.com"},
				{Name: "To", Value: "bob@example.com"},
				{Name: "Cc", Value: "charlie@example.com"},
				{Name: "Reply-To", Value: "alice-reply@example.com"},
				{Name: "Subject", Value: "Test subject"},
				{Name: "Date", Value: "Mon, 7 Apr 2026 12:00:00 +0000"},
				{Name: "Message-ID", Value: "<abc123@mail.gmail.com>"},
				{Name: "References", Value: "<ref1@mail.gmail.com> <ref2@mail.gmail.com>"},
			},
			Body: gmailBody{Data: b64("Hello!")},
		},
	}

	detail := parseMessageDetail(msg, newLabelResolver(context.Background(), nil))

	if detail.ID != "msg-1" {
		t.Errorf("ID = %q, want %q", detail.ID, "msg-1")
	}
	if detail.ThreadID != "thread-1" {
		t.Errorf("ThreadID = %q, want %q", detail.ThreadID, "thread-1")
	}
	if detail.From != "alice@example.com" {
		t.Errorf("From = %q, want %q", detail.From, "alice@example.com")
	}
	if detail.To != "bob@example.com" {
		t.Errorf("To = %q, want %q", detail.To, "bob@example.com")
	}
	if detail.Cc != "charlie@example.com" {
		t.Errorf("Cc = %q, want %q", detail.Cc, "charlie@example.com")
	}
	if detail.ReplyTo != "alice-reply@example.com" {
		t.Errorf("ReplyTo = %q, want %q", detail.ReplyTo, "alice-reply@example.com")
	}
	if detail.Subject != "Test subject" {
		t.Errorf("Subject = %q, want %q", detail.Subject, "Test subject")
	}
	if detail.MessageID != "<abc123@mail.gmail.com>" {
		t.Errorf("MessageID = %q, want %q", detail.MessageID, "<abc123@mail.gmail.com>")
	}
	if detail.References != "<ref1@mail.gmail.com> <ref2@mail.gmail.com>" {
		t.Errorf("References = %q, want %q", detail.References, "<ref1@mail.gmail.com> <ref2@mail.gmail.com>")
	}
	if !detail.IsUnread {
		t.Error("IsUnread should be true")
	}
	if len(detail.Labels) != 2 || detail.Labels[0] != "INBOX" || detail.Labels[1] != "UNREAD" {
		t.Errorf("Labels = %v, want [INBOX UNREAD]", detail.Labels)
	}
	if detail.Body != "Hello!" {
		t.Errorf("Body = %q, want %q", detail.Body, "Hello!")
	}
}

func TestBuildMIMEMessage_WithReferences(t *testing.T) {
	msg := buildMIMEMessage(
		"Alice Smith <alice@example.com>",
		"bob@example.com",
		"", "",
		"Re: Test",
		"Reply body",
		"",
		"<orig@mail.gmail.com>",
		"<ref1@mail.gmail.com> <orig@mail.gmail.com>",
	)

	if !strings.Contains(msg, "From: Alice Smith <alice@example.com>") {
		t.Error("missing From header with display name")
	}
	if !strings.Contains(msg, "In-Reply-To: <orig@mail.gmail.com>") {
		t.Error("missing In-Reply-To header")
	}
	if !strings.Contains(msg, "References: <ref1@mail.gmail.com> <orig@mail.gmail.com>") {
		t.Error("missing References header")
	}
	if !strings.Contains(msg, "To: bob@example.com") {
		t.Error("missing To header")
	}
	if !strings.Contains(msg, "Reply body") {
		t.Error("missing body")
	}
}

func TestBuildMIMEMessage_WithoutReferences(t *testing.T) {
	msg := buildMIMEMessage("", "bob@example.com", "", "", "Hello", "Body", "", "", "")

	if !strings.Contains(msg, "From: me") {
		t.Error("should fall back to From: me when from is empty")
	}
	if strings.Contains(msg, "In-Reply-To:") {
		t.Error("should not have In-Reply-To header")
	}
	if strings.Contains(msg, "References:") {
		t.Error("should not have References header")
	}
}

func TestBuildMIMEMessage_WithHTMLAlternative(t *testing.T) {
	msg := buildMIMEMessage(
		"Alice Smith <alice@example.com>",
		"bob@example.com",
		"", "",
		"Re: Test",
		"Reply body\r\n\r\nOn date, person wrote:\r\n> quoted",
		`<div dir="ltr">Reply body</div><br><div class="gmail_quote gmail_quote_container"><blockquote class="gmail_quote">quoted</blockquote></div>`,
		"<orig@mail.gmail.com>",
		"<ref1@mail.gmail.com> <orig@mail.gmail.com>",
	)

	if !strings.Contains(msg, `Content-Type: multipart/alternative; boundary="clawvisor-alt"`) {
		t.Errorf("missing multipart content type: %s", msg)
	}
	if !strings.Contains(msg, "Content-Type: text/plain; charset=\"UTF-8\"") {
		t.Errorf("missing text/plain part: %s", msg)
	}
	if !strings.Contains(msg, "Content-Type: text/html; charset=\"UTF-8\"") {
		t.Errorf("missing text/html part: %s", msg)
	}
	if !strings.Contains(msg, "gmail_quote") {
		t.Errorf("missing gmail quote HTML: %s", msg)
	}
}

func TestSendMessage_WithThreadID_QuotesPreviousMessage(t *testing.T) {
	var sentPayload struct {
		Raw      string `json:"raw"`
		ThreadID string `json:"threadId"`
	}

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var body string
			switch {
			case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/threads/thread-123"):
				body = `{
					"id":"thread-123",
					"messages":[{
						"id":"msg-1",
						"threadId":"thread-123",
						"payload":{
							"headers":[
								{"name":"From","value":"alice@example.com"},
								{"name":"To","value":"bob@example.com"},
								{"name":"Subject","value":"Original subject"},
								{"name":"Date","value":"Mon, 7 Apr 2026 12:00:00 +0000"},
								{"name":"Message-ID","value":"<orig@mail.gmail.com>"},
								{"name":"References","value":"<root@mail.gmail.com>"}
							],
							"mimeType":"text/plain",
							"body":{"data":"` + rawB64("Original line 1\nOriginal line 2") + `"}
						}
					}]
				}`
			case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/settings/sendAs"):
				body = `{"sendAs":[{"sendAsEmail":"sender@example.com","displayName":"Sender","isDefault":true,"isPrimary":true}]}`
			case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/messages/send"):
				data, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("read request body: %v", err)
				}
				if err := json.Unmarshal(data, &sentPayload); err != nil {
					t.Fatalf("unmarshal send payload: %v", err)
				}
				body = `{"id":"sent-1"}`
			default:
				t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}

	adapter := &GmailAdapter{}
	result, err := adapter.sendMessage(context.Background(), client, map[string]any{
		"thread_id": "thread-123",
		"body":      "My reply",
	})
	if err != nil {
		t.Fatalf("sendMessage error: %v", err)
	}
	if result == nil {
		t.Fatal("sendMessage returned nil result")
	}
	if sentPayload.ThreadID != "thread-123" {
		t.Fatalf("threadId = %q, want %q", sentPayload.ThreadID, "thread-123")
	}

	raw, err := base64.RawURLEncoding.DecodeString(sentPayload.Raw)
	if err != nil {
		t.Fatalf("decode raw MIME: %v", err)
	}
	msg := string(raw)
	if !strings.Contains(msg, `Content-Type: multipart/alternative; boundary="clawvisor-alt"`) {
		t.Errorf("missing multipart content type in MIME: %s", msg)
	}
	if !strings.Contains(msg, "Subject: Re: Original subject") {
		t.Errorf("missing derived reply subject in MIME: %s", msg)
	}
	if !strings.Contains(msg, "In-Reply-To: <orig@mail.gmail.com>") {
		t.Errorf("missing In-Reply-To header in MIME: %s", msg)
	}
	if !strings.Contains(msg, "References: <root@mail.gmail.com> <orig@mail.gmail.com>") {
		t.Errorf("missing References header in MIME: %s", msg)
	}
	if !strings.Contains(msg, "My reply") || !strings.Contains(msg, "On Mon, 7 Apr 2026 12:00:00 +0000, alice@example.com wrote:") || !strings.Contains(msg, "> Original line 1") || !strings.Contains(msg, "> Original line 2") {
		t.Errorf("missing quoted original body in MIME: %s", msg)
	}
	if !strings.Contains(msg, "gmail_quote") {
		t.Errorf("missing gmail quote HTML in MIME: %s", msg)
	}
}

func TestArchiveMessage_RemovesInboxLabel(t *testing.T) {
	var sentPayload struct {
		RemoveLabelIDs []string `json:"removeLabelIds"`
	}

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/messages/msg-1/modify") {
				t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
			}
			data, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			if err := json.Unmarshal(data, &sentPayload); err != nil {
				t.Fatalf("unmarshal modify payload: %v", err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"id":"msg-1","threadId":"thread-1","labelIds":["UNREAD"]}`)),
			}, nil
		}),
	}

	adapter := &GmailAdapter{}
	result, err := adapter.archiveMessage(context.Background(), client, map[string]any{
		"message_id": "msg-1",
	})
	if err != nil {
		t.Fatalf("archiveMessage error: %v", err)
	}
	if result == nil {
		t.Fatal("archiveMessage returned nil result")
	}
	if len(sentPayload.RemoveLabelIDs) != 1 || sentPayload.RemoveLabelIDs[0] != "INBOX" {
		t.Fatalf("removeLabelIds = %v, want [INBOX]", sentPayload.RemoveLabelIDs)
	}
}

func TestArchiveMessage_RequiresMessageID(t *testing.T) {
	adapter := &GmailAdapter{}
	if _, err := adapter.archiveMessage(context.Background(), &http.Client{}, map[string]any{}); err == nil {
		t.Fatal("expected error when message_id missing")
	}
}

func TestSendMessage_WithInReplyTo_ResolvesThreadAndQuotesPreviousMessage(t *testing.T) {
	var sentPayload struct {
		Raw      string `json:"raw"`
		ThreadID string `json:"threadId"`
	}
	const messageID = "<CAGGMS=RZG7QFRkNRZ99ofzMn+rEX_WyWV0foyLzSf0n6m=fT8w@mail.gmail.com>"

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var body string
			switch {
			case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/messages"):
				if got := req.URL.Query().Get("q"); got != "rfc822msgid:"+messageID {
					t.Fatalf("search query = %q, want %q", got, "rfc822msgid:"+messageID)
				}
				body = `{"messages":[{"id":"msg-1","threadId":"thread-123"}]}`
			case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/threads/thread-123"):
				body = `{
					"id":"thread-123",
					"messages":[{
						"id":"msg-1",
						"threadId":"thread-123",
						"payload":{
							"headers":[
								{"name":"From","value":"alice@example.com"},
								{"name":"Subject","value":"Original subject"},
								{"name":"Date","value":"Mon, 7 Apr 2026 12:00:00 +0000"},
								{"name":"Message-ID","value":"` + messageID + `"},
								{"name":"References","value":"<root@mail.gmail.com>"}
							],
							"mimeType":"text/plain",
							"body":{"data":"` + rawB64("Original line 1\nOriginal line 2") + `"}
						}
					}]
				}`
			case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/settings/sendAs"):
				body = `{"sendAs":[{"sendAsEmail":"sender@example.com","displayName":"Sender","isDefault":true,"isPrimary":true}]}`
			case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/messages/send"):
				data, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("read request body: %v", err)
				}
				if err := json.Unmarshal(data, &sentPayload); err != nil {
					t.Fatalf("unmarshal send payload: %v", err)
				}
				body = `{"id":"sent-1"}`
			default:
				t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}

	adapter := &GmailAdapter{}
	result, err := adapter.sendMessage(context.Background(), client, map[string]any{
		"to":          "eric@levine.tech",
		"subject":     "Re: Sender name test #4",
		"body":        "Reply #5 -- another threading check.",
		"in_reply_to": messageID,
	})
	if err != nil {
		t.Fatalf("sendMessage error: %v", err)
	}
	if result == nil {
		t.Fatal("sendMessage returned nil result")
	}
	if sentPayload.ThreadID != "thread-123" {
		t.Fatalf("threadId = %q, want %q", sentPayload.ThreadID, "thread-123")
	}

	raw, err := base64.RawURLEncoding.DecodeString(sentPayload.Raw)
	if err != nil {
		t.Fatalf("decode raw MIME: %v", err)
	}
	msg := string(raw)
	if !strings.Contains(msg, `Content-Type: multipart/alternative; boundary="clawvisor-alt"`) {
		t.Errorf("missing multipart content type in MIME: %s", msg)
	}
	if !strings.Contains(msg, "In-Reply-To: "+messageID) {
		t.Errorf("missing resolved In-Reply-To header in MIME: %s", msg)
	}
	if !strings.Contains(msg, "References: <root@mail.gmail.com> "+messageID) {
		t.Errorf("missing References header in MIME: %s", msg)
	}
	if !strings.Contains(msg, "Reply #5 -- another threading check.") || !strings.Contains(msg, "On Mon, 7 Apr 2026 12:00:00 +0000, alice@example.com wrote:") || !strings.Contains(msg, "> Original line 1") || !strings.Contains(msg, "> Original line 2") {
		t.Errorf("missing quoted original body in MIME: %s", msg)
	}
	if !strings.Contains(msg, "gmail_quote") {
		t.Errorf("missing gmail quote HTML in MIME: %s", msg)
	}
}

func TestListMessages_Concurrency(t *testing.T) {
	const totalMsgs = 200

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var body string
			switch {
			case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/messages") && !strings.Contains(req.URL.Path, "/msg-"):
				var msgsList []string
				for i := 1; i <= totalMsgs; i++ {
					msgsList = append(msgsList, fmt.Sprintf(`{"id": "msg-%d"}`, i))
				}
				body = fmt.Sprintf(`{
					"messages": [%s],
					"resultSizeEstimate": %d
				}`, strings.Join(msgsList, ","), totalMsgs)
			case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/messages/msg-"):
				// Extract msg ID from URL path (e.g. "/messages/msg-1")
				parts := strings.Split(req.URL.Path, "/")
				msgID := parts[len(parts)-1]
				body = fmt.Sprintf(`{
					"snippet": "Snippet for %s",
					"labelIds": ["INBOX", "UNREAD"],
					"payload": {
						"headers": [
							{"name": "From", "value": "sender-%s@example.com"},
							{"name": "Subject", "value": "Subject %s"},
							{"name": "Date", "value": "Date-%s"}
						]
					}
				}`, msgID, msgID, msgID, msgID)
			default:
				t.Logf("Unexpected request in mock: %s %s", req.Method, req.URL.String())
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Body:       io.NopCloser(strings.NewReader("not found")),
				}, nil
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}

	adapter := &GmailAdapter{}
	res, err := adapter.listMessages(context.Background(), client, map[string]any{
		"max_results": totalMsgs,
	})
	if err != nil {
		t.Fatalf("listMessages error: %v", err)
	}
	if res == nil {
		t.Fatal("listMessages returned nil result")
	}

	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any for Data, got %T", res.Data)
	}
	messagesRaw, ok := data["messages"]
	if !ok {
		t.Fatalf("result data missing messages field: %v", res.Data)
	}
	messages, ok := messagesRaw.([]msgListItem)
	if !ok {
		t.Fatalf("expected []msgListItem, got %T", messagesRaw)
	}

	t.Logf("Fetched messages count: %d", len(messages))
	if len(messages) != totalMsgs {
		t.Errorf("len(messages) = %d, want %d", len(messages), totalMsgs)
	}

	// Verify exact order and metadata extraction
	for i, msg := range messages {
		expectedID := fmt.Sprintf("msg-%d", i+1)
		if msg.ID != expectedID {
			t.Errorf("messages[%d].ID = %q, want %q", i, msg.ID, expectedID)
		}
		expectedFrom := fmt.Sprintf("sender-msg-%d@example.com", i+1)
		if msg.From != expectedFrom {
			t.Errorf("messages[%d].From = %q, want %q", i, msg.From, expectedFrom)
		}
		expectedSubject := fmt.Sprintf("Subject msg-%d", i+1)
		if msg.Subject != expectedSubject {
			t.Errorf("messages[%d].Subject = %q, want %q", i, msg.Subject, expectedSubject)
		}
		expectedSnippet := fmt.Sprintf("Snippet for msg-%d", i+1)
		if msg.Snippet != expectedSnippet {
			t.Errorf("messages[%d].Snippet = %q, want %q", i, msg.Snippet, expectedSnippet)
		}
		if !msg.IsUnread {
			t.Errorf("messages[%d].IsUnread = false, want true", i)
		}
	}
}

func TestListMessages_Concurrency_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const totalMsgs = 20
	cancelDone := make(chan struct{})
	var fetchCount int
	var mu sync.Mutex

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var body string
			switch {
			case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/messages") && !strings.Contains(req.URL.Path, "/msg-"):
				var msgsList []string
				for i := 1; i <= totalMsgs; i++ {
					msgsList = append(msgsList, fmt.Sprintf(`{"id": "msg-%d"}`, i))
				}
				body = fmt.Sprintf(`{
					"messages": [%s],
					"resultSizeEstimate": %d
				}`, strings.Join(msgsList, ","), totalMsgs)
			case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/messages/msg-"):
				parts := strings.Split(req.URL.Path, "/")
				msgID := parts[len(parts)-1]

				if msgID == "msg-2" {
					cancel()
					mu.Lock()
					select {
					case <-cancelDone:
					default:
						close(cancelDone)
					}
					mu.Unlock()
				} else {
					<-cancelDone
				}

				mu.Lock()
				fetchCount++
				mu.Unlock()

				body = fmt.Sprintf(`{
					"snippet": "Snippet for %s",
					"labelIds": ["INBOX"],
					"payload": {
						"headers": [
							{"name": "From", "value": "sender-%s@example.com"},
							{"name": "Subject", "value": "Subject %s"},
							{"name": "Date", "value": "Date-%s"}
						]
					}
				}`, msgID, msgID, msgID, msgID)
			default:
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Body:       io.NopCloser(strings.NewReader("not found")),
				}, nil
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}

	adapter := &GmailAdapter{}
	_, err := adapter.listMessages(ctx, client, map[string]any{
		"max_results": totalMsgs,
	})
	if err == nil {
		t.Fatalf("expected context cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected error to wrap context.Canceled, got %v", err)
	}
}

func TestListMessages_Concurrency_AllMetadataFetchesFailed(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var body string
			var status = http.StatusOK
			switch {
			case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/messages") && !strings.Contains(req.URL.Path, "/msg-"):
				body = `{
					"messages": [
						{"id": "msg-1"},
						{"id": "msg-2"}
					],
					"resultSizeEstimate": 2
				}`
			case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/messages/msg-"):
				status = http.StatusInternalServerError
				body = "internal server error"
			default:
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Body:       io.NopCloser(strings.NewReader("not found")),
				}, nil
			}

			return &http.Response{
				StatusCode: status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}

	adapter := &GmailAdapter{}
	_, err := adapter.listMessages(context.Background(), client, map[string]any{
		"max_results": 2,
	})
	if err == nil {
		t.Fatalf("expected error when all metadata fetches fail, got nil")
	}
	if !strings.Contains(err.Error(), "all metadata fetches failed") {
		t.Errorf("expected error to mention 'all metadata fetches failed', got: %v", err)
	}
}

func TestListMessages_Concurrency_PartialErrors(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var body string
			var status = http.StatusOK
			switch {
			case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/messages") && !strings.Contains(req.URL.Path, "/msg-"):
				body = `{
					"messages": [
						{"id": "msg-1"},
						{"id": "msg-2"},
						{"id": "msg-3"},
						{"id": "msg-4"}
					],
					"resultSizeEstimate": 4
				}`
			case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/messages/msg-"):
				parts := strings.Split(req.URL.Path, "/")
				msgID := parts[len(parts)-1]
				if msgID == "msg-2" || msgID == "msg-4" {
					status = http.StatusInternalServerError
					body = "internal server error"
				} else {
					body = fmt.Sprintf(`{
						"snippet": "Snippet for %s",
						"labelIds": ["INBOX"],
						"payload": {
							"headers": [
								{"name": "From", "value": "sender-%s@example.com"},
								{"name": "Subject", "value": "Subject %s"},
								{"name": "Date", "value": "Date-%s"}
							]
						}
					}`, msgID, msgID, msgID, msgID)
				}
			default:
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Body:       io.NopCloser(strings.NewReader("not found")),
				}, nil
			}

			return &http.Response{
				StatusCode: status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}

	adapter := &GmailAdapter{}
	res, err := adapter.listMessages(context.Background(), client, map[string]any{
		"max_results": 4,
	})
	if err != nil {
		t.Fatalf("listMessages error: %v", err)
	}

	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", res.Data)
	}
	messagesRaw, ok := data["messages"]
	if !ok {
		t.Fatalf("missing messages field")
	}
	messages := messagesRaw.([]msgListItem)

	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}

	if messages[0].ID != "msg-1" {
		t.Errorf("first message ID = %q, want msg-1", messages[0].ID)
	}
	if messages[1].ID != "msg-3" {
		t.Errorf("second message ID = %q, want msg-3", messages[1].ID)
	}
}


