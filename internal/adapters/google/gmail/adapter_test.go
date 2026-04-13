package gmail

import (
	"encoding/base64"
	"strings"
	"testing"
)

func b64(s string) string { return base64.URLEncoding.EncodeToString([]byte(s)) }

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
	msg := buildMIMEMessage("", "bob@example.com", "Hello", "Body", "", "")

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
