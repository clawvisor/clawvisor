package gmail

import (
	"encoding/base64"
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
