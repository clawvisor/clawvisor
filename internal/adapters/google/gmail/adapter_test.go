package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func b64(s string) string { return base64.URLEncoding.EncodeToString([]byte(s)) }
func rawB64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
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

	detail := parseMessageDetail(msg)

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
	if detail.Body != "Hello!" {
		t.Errorf("Body = %q, want %q", detail.Body, "Hello!")
	}
}

func TestBuildMIMEMessage_WithReferences(t *testing.T) {
	msg := buildMIMEMessage(
		"Alice Smith <alice@example.com>",
		"bob@example.com",
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
	msg := buildMIMEMessage("", "bob@example.com", "Hello", "Body", "", "", "")

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
