package gmail

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/oauth2"

	"github.com/clawvisor/clawvisor/internal/adapters/google/credential"
	"github.com/clawvisor/clawvisor/pkg/adapters"
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

// batchTestServer mocks Gmail's list + batch endpoints for the list_messages
// tests. It replies to GET .../messages with a list of synthetic ids and to
// POST /batch/gmail/v1 with a multipart/mixed response that produces a stock
// meta payload for each requested id, unless perItemStatus overrides that id
// to a non-200 status. batchStatus, if set, replaces the outer batch POST
// status (used for whole-batch failure tests).
type batchTestServer struct {
	t              *testing.T
	listIDs        []string
	perItemStatus  map[string]int
	batchStatus    int // 0 means 200
	batchPostCount atomic.Int32
	lastBatchSizes []int
}

func (s *batchTestServer) handle(req *http.Request) (*http.Response, error) {
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/messages"):
		entries := make([]string, len(s.listIDs))
		for i, id := range s.listIDs {
			entries[i] = fmt.Sprintf(`{"id":%q}`, id)
		}
		body := fmt.Sprintf(`{"messages":[%s],"resultSizeEstimate":%d}`,
			strings.Join(entries, ","), len(s.listIDs))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/batch/gmail/v1"):
		s.batchPostCount.Add(1)
		return s.serveBatch(req)
	default:
		s.t.Logf("unexpected request: %s %s", req.Method, req.URL.String())
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("not found")),
		}, nil
	}
}

func (s *batchTestServer) serveBatch(req *http.Request) (*http.Response, error) {
	_, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil {
		s.t.Fatalf("batch request bad Content-Type: %v", err)
	}
	reqBody, _ := io.ReadAll(req.Body)
	mr := multipart.NewReader(bytes.NewReader(reqBody), params["boundary"])

	type subReq struct {
		contentID string
		msgID     string
	}
	var subs []subReq
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.t.Fatalf("batch request parse part: %v", err)
		}
		cid := part.Header.Get("Content-ID")
		body, _ := io.ReadAll(part)
		part.Close()
		// Sub-request body is a textual HTTP request — parse the request line.
		line, _, _ := bufio.NewReader(bytes.NewReader(body)).ReadLine()
		// e.g. "GET /gmail/v1/users/me/messages/msg-3?format=metadata&... HTTP/1.1"
		fields := strings.Fields(string(line))
		if len(fields) < 2 {
			s.t.Fatalf("batch sub-request bad request line: %q", line)
		}
		path := fields[1]
		if q := strings.Index(path, "?"); q >= 0 {
			path = path[:q]
		}
		msgID := path[strings.LastIndex(path, "/")+1:]
		subs = append(subs, subReq{contentID: cid, msgID: msgID})
	}
	s.lastBatchSizes = append(s.lastBatchSizes, len(subs))

	if s.batchStatus != 0 && s.batchStatus != http.StatusOK {
		return &http.Response{
			StatusCode: s.batchStatus,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("mock batch failure")),
		}, nil
	}

	var resp bytes.Buffer
	mw := multipart.NewWriter(&resp)
	for _, sub := range subs {
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Type", "application/http")
		// Gmail echoes the client-supplied id with a "response-" prefix.
		hdr.Set("Content-ID", "<response-"+strings.Trim(sub.contentID, "<>")+">")
		w, err := mw.CreatePart(hdr)
		if err != nil {
			s.t.Fatalf("batch response build part: %v", err)
		}
		status := http.StatusOK
		if override, ok := s.perItemStatus[sub.msgID]; ok {
			status = override
		}
		var subBody string
		if status >= 400 {
			subBody = fmt.Sprintf(`{"error":{"code":%d,"message":"mock failure"}}`, status)
		} else {
			subBody = fmt.Sprintf(`{"snippet":"Snippet for %s","labelIds":["INBOX","UNREAD"],"payload":{"headers":[{"name":"From","value":"sender-%s@example.com"},{"name":"Subject","value":"Subject %s"},{"name":"Date","value":"Date-%s"}]}}`,
				sub.msgID, sub.msgID, sub.msgID, sub.msgID)
		}
		fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
			status, http.StatusText(status), len(subBody), subBody)
	}
	if err := mw.Close(); err != nil {
		s.t.Fatalf("batch response close: %v", err)
	}

	header := make(http.Header)
	header.Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       io.NopCloser(&resp),
	}, nil
}

func newBatchTestClient(s *batchTestServer) *http.Client {
	return &http.Client{Transport: roundTripFunc(s.handle)}
}

func extractMessageItems(t *testing.T, res *adapters.Result) []msgListItem {
	t.Helper()
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any for Data, got %T", res.Data)
	}
	raw, ok := data["messages"]
	if !ok {
		t.Fatalf("result data missing messages field: %v", res.Data)
	}
	items, ok := raw.([]msgListItem)
	if !ok {
		t.Fatalf("expected []msgListItem, got %T", raw)
	}
	return items
}

func TestListMessages_BatchSuccess(t *testing.T) {
	ids := []string{"msg-1", "msg-2", "msg-3", "msg-4", "msg-5"}
	srv := &batchTestServer{t: t, listIDs: ids}
	adapter := &GmailAdapter{}

	res, err := adapter.listMessages(context.Background(), newBatchTestClient(srv), map[string]any{
		"max_results": len(ids),
	})
	if err != nil {
		t.Fatalf("listMessages error: %v", err)
	}
	items := extractMessageItems(t, res)

	if got := srv.batchPostCount.Load(); got != 1 {
		t.Errorf("batch POST count = %d, want 1", got)
	}
	if got := srv.lastBatchSizes; len(got) != 1 || got[0] != len(ids) {
		t.Errorf("batch sub-request sizes = %v, want [%d]", got, len(ids))
	}
	if len(items) != len(ids) {
		t.Fatalf("len(items) = %d, want %d", len(items), len(ids))
	}
	for i, item := range items {
		if item.ID != ids[i] {
			t.Errorf("items[%d].ID = %q, want %q", i, item.ID, ids[i])
		}
		if want := fmt.Sprintf("sender-%s@example.com", ids[i]); item.From != want {
			t.Errorf("items[%d].From = %q, want %q", i, item.From, want)
		}
		if want := fmt.Sprintf("Subject %s", ids[i]); item.Subject != want {
			t.Errorf("items[%d].Subject = %q, want %q", i, item.Subject, want)
		}
		if !item.IsUnread {
			t.Errorf("items[%d].IsUnread = false, want true", i)
		}
	}
}

func TestListMessages_BatchPartialFailure(t *testing.T) {
	ids := []string{"msg-1", "msg-2", "msg-3"}
	srv := &batchTestServer{
		t:             t,
		listIDs:       ids,
		perItemStatus: map[string]int{"msg-2": http.StatusNotFound},
	}
	adapter := &GmailAdapter{}

	res, err := adapter.listMessages(context.Background(), newBatchTestClient(srv), map[string]any{
		"max_results": len(ids),
	})
	if err != nil {
		t.Fatalf("listMessages error: %v", err)
	}
	items := extractMessageItems(t, res)

	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(items))
	}
	if items[0].ID != "msg-1" {
		t.Errorf("items[0].ID = %q, want msg-1", items[0].ID)
	}
	if items[1].ID != "msg-3" {
		t.Errorf("items[1].ID = %q, want msg-3", items[1].ID)
	}
}

func TestListMessages_BatchPaging(t *testing.T) {
	const total = 150
	ids := make([]string, total)
	for i := range ids {
		ids[i] = fmt.Sprintf("msg-%03d", i+1)
	}
	srv := &batchTestServer{t: t, listIDs: ids}
	adapter := &GmailAdapter{}

	res, err := adapter.listMessages(context.Background(), newBatchTestClient(srv), map[string]any{
		"max_results": total,
	})
	if err != nil {
		t.Fatalf("listMessages error: %v", err)
	}
	items := extractMessageItems(t, res)

	if got := srv.batchPostCount.Load(); got != 2 {
		t.Errorf("batch POST count = %d, want 2", got)
	}
	if got, want := srv.lastBatchSizes, []int{100, 50}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("batch sub-request sizes = %v, want %v", got, want)
	}
	if len(items) != total {
		t.Fatalf("len(items) = %d, want %d", len(items), total)
	}
	for i, item := range items {
		if item.ID != ids[i] {
			t.Fatalf("items[%d].ID = %q, want %q (ordering broken across batches?)", i, item.ID, ids[i])
		}
	}
}

func TestListMessages_BatchEmptyResult(t *testing.T) {
	srv := &batchTestServer{t: t, listIDs: nil}
	adapter := &GmailAdapter{}

	res, err := adapter.listMessages(context.Background(), newBatchTestClient(srv), map[string]any{
		"max_results": 10,
	})
	if err != nil {
		t.Fatalf("listMessages error: %v", err)
	}
	items := extractMessageItems(t, res)

	if got := srv.batchPostCount.Load(); got != 0 {
		t.Errorf("batch POST count = %d, want 0", got)
	}
	if len(items) != 0 {
		t.Errorf("len(items) = %d, want 0", len(items))
	}
}

// TestListMessages_BatchMissingPart simulates Gmail returning fewer
// sub-responses than we asked for: only msg-1 echoes back; msg-2 is silently
// dropped from the multipart body. The dropped slot must surface as an
// error rather than degrade to an empty msgListItem with no headers.
// errReadCloser returns a chunk of bytes once, then a non-EOF error on the
// next Read. Used to simulate a truncated multipart response from Gmail.
type errReadCloser struct {
	data []byte
	pos  int
	err  error
}

func (r *errReadCloser) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, r.err
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func (r *errReadCloser) Close() error { return nil }

// TestListMessages_BatchBodyReadError simulates a wire-level truncation
// of the batch response body. listMessages must surface a wrapped read
// error (and therefore return "all metadata fetches failed") rather than
// silently parsing whatever prefix arrived and treating short reads as
// successes.
func TestListMessages_BatchBodyReadError(t *testing.T) {
	ids := []string{"msg-1", "msg-2"}
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/messages") {
			body := fmt.Sprintf(`{"messages":[%s],"resultSizeEstimate":%d}`,
				`{"id":"msg-1"},{"id":"msg-2"}`, len(ids))
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}
		if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/batch/gmail/v1") {
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("nf"))}, nil
		}
		// Return a truncated multipart prefix: a couple of bytes then
		// a non-EOF error. The Content-Type advertises multipart so the
		// status-error branch is skipped and the read-error branch must fire.
		header := make(http.Header)
		header.Set("Content-Type", "multipart/mixed; boundary=boundary42")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     header,
			Body:       &errReadCloser{data: []byte("--boundary42\r\n"), err: fmt.Errorf("connection reset")},
		}, nil
	})
	adapter := &GmailAdapter{}

	_, err := adapter.listMessages(context.Background(), &http.Client{Transport: transport}, map[string]any{
		"max_results": len(ids),
	})
	if err == nil {
		t.Fatalf("expected error when batch body read fails, got nil")
	}
	if !strings.Contains(err.Error(), "all metadata fetches failed") {
		t.Errorf("expected 'all metadata fetches failed' wrap, got: %v", err)
	}
	if !strings.Contains(err.Error(), "gmail batch read body") {
		t.Errorf("expected inner error to mention 'gmail batch read body', got: %v", err)
	}
}

func TestListMessages_BatchMissingPart(t *testing.T) {
	ids := []string{"msg-1", "msg-2"}
	// Custom transport: respond to /messages normally; respond to
	// /batch/gmail/v1 with a multipart that only contains the first part.
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/messages") {
			body := fmt.Sprintf(`{"messages":[%s],"resultSizeEstimate":%d}`,
				`{"id":"msg-1"},{"id":"msg-2"}`, len(ids))
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}
		if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/batch/gmail/v1") {
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("nf"))}, nil
		}
		var resp bytes.Buffer
		mw := multipart.NewWriter(&resp)
		// Only echo back the FIRST sub-response — drop the second entirely.
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Type", "application/http")
		hdr.Set("Content-ID", "<response-item-0>")
		w, _ := mw.CreatePart(hdr)
		subBody := `{"snippet":"Snippet for msg-1","labelIds":["INBOX"],"payload":{"headers":[{"name":"From","value":"a@b.com"},{"name":"Subject","value":"S"},{"name":"Date","value":"D"}]}}`
		fmt.Fprintf(w, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(subBody), subBody)
		mw.Close()
		header := make(http.Header)
		header.Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(&resp)}, nil
	})
	adapter := &GmailAdapter{}

	res, err := adapter.listMessages(context.Background(), &http.Client{Transport: transport}, map[string]any{
		"max_results": len(ids),
	})
	if err != nil {
		t.Fatalf("listMessages error: %v", err)
	}
	items := extractMessageItems(t, res)

	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1 (msg-2's missing response slot must not emit a ghost item)", len(items))
	}
	if items[0].ID != "msg-1" {
		t.Errorf("items[0].ID = %q, want msg-1", items[0].ID)
	}
}

// TestListMessages_BatchAllFailed mirrors the AllMetadataFetchesFailed
// semantic for the batch path: when every sub-response is an error, the
// caller sees a wrapped error rather than an empty success.
func TestListMessages_BatchAllFailed(t *testing.T) {
	ids := []string{"msg-1", "msg-2"}
	srv := &batchTestServer{
		t:       t,
		listIDs: ids,
		perItemStatus: map[string]int{
			"msg-1": http.StatusInternalServerError,
			"msg-2": http.StatusInternalServerError,
		},
	}
	adapter := &GmailAdapter{}

	_, err := adapter.listMessages(context.Background(), newBatchTestClient(srv), map[string]any{
		"max_results": len(ids),
	})
	if err == nil {
		t.Fatalf("expected error when all metadata fetches fail, got nil")
	}
	if !strings.Contains(err.Error(), "all metadata fetches failed") {
		t.Errorf("error = %v, want it to mention 'all metadata fetches failed'", err)
	}
}

// TestListMessages_BatchContextCancel pre-cancels the context and asserts
// listMessages returns an error wrapping context.Canceled. This replaces
// the prior per-message-fetch ContextCancel test: with batched metadata
// the cancellation surface is one outer POST, so the test exercises the
// post-batch ctx.Err() guard in listMessages.
func TestListMessages_BatchContextCancel(t *testing.T) {
	srv := &batchTestServer{t: t, listIDs: []string{"msg-1", "msg-2"}}
	adapter := &GmailAdapter{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := adapter.listMessages(ctx, newBatchTestClient(srv), map[string]any{
		"max_results": 2,
	})
	if err == nil {
		t.Fatalf("expected context cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected error to wrap context.Canceled, got %v", err)
	}
}


