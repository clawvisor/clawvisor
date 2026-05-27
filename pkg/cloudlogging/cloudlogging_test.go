package cloudlogging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"testing"
)

func TestExtractTrace_CloudHeader(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	// X-Cloud-Trace-Context span IDs are decimal; we must convert to the
	// 16-char hex form logging.googleapis.com/spanId expects.
	req.Header.Set("X-Cloud-Trace-Context", "abc123/456;o=1")

	traceID, spanID := ExtractTrace(req)
	if traceID != "abc123" || spanID != "00000000000001c8" {
		t.Fatalf("got %q/%q", traceID, spanID)
	}
}

func TestExtractTrace_CloudHeaderMaxSpanID(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Cloud-Trace-Context", "abc/18446744073709551615")

	_, spanID := ExtractTrace(req)
	if spanID != "ffffffffffffffff" {
		t.Fatalf("expected ffffffffffffffff, got %q", spanID)
	}
}

func TestExtractTrace_CloudHeaderBadSpanID(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Cloud-Trace-Context", "abc/notanumber")

	traceID, spanID := ExtractTrace(req)
	if traceID != "abc" || spanID != "" {
		t.Fatalf("expected abc/'', got %q/%q", traceID, spanID)
	}
}

func TestExtractTrace_CloudHeaderNoSpan(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Cloud-Trace-Context", "abc123")

	traceID, spanID := ExtractTrace(req)
	if traceID != "abc123" || spanID != "" {
		t.Fatalf("got %q/%q", traceID, spanID)
	}
}

func TestExtractTrace_W3CTraceparent(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")

	traceID, spanID := ExtractTrace(req)
	if traceID != "0af7651916cd43dd8448eb211c80319c" || spanID != "b7ad6b7169203331" {
		t.Fatalf("got %q/%q", traceID, spanID)
	}
}

func TestExtractTrace_None(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	traceID, spanID := ExtractTrace(req)
	if traceID != "" || spanID != "" {
		t.Fatalf("expected empty, got %q/%q", traceID, spanID)
	}
}

func TestHandler_InjectsTraceAndSpan(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, nil)
	h := NewHandler(inner, "my-project")
	logger := slog.New(h)

	ctx := WithTrace(context.Background(), "abc123", "00000000000001c8")
	logger.InfoContext(ctx, "hello")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["logging.googleapis.com/trace"] != "projects/my-project/traces/abc123" {
		t.Fatalf("trace field: %v", got["logging.googleapis.com/trace"])
	}
	if got["logging.googleapis.com/spanId"] != "00000000000001c8" {
		t.Fatalf("span field: %v", got["logging.googleapis.com/spanId"])
	}
}

func TestHandler_NoTraceInContext(t *testing.T) {
	var buf bytes.Buffer
	h := NewHandler(slog.NewJSONHandler(&buf, nil), "my-project")
	logger := slog.New(h)

	logger.Info("hello")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["logging.googleapis.com/trace"]; ok {
		t.Fatalf("did not expect trace field, got: %v", got)
	}
}

func TestHandler_NoProjectIDEmitsBareTraceID(t *testing.T) {
	var buf bytes.Buffer
	h := NewHandler(slog.NewJSONHandler(&buf, nil), "")
	logger := slog.New(h)

	ctx := WithTrace(context.Background(), "abc123", "")
	logger.InfoContext(ctx, "hello")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["logging.googleapis.com/trace"] != "abc123" {
		t.Fatalf("trace field: %v", got["logging.googleapis.com/trace"])
	}
}

func TestHandler_WithAttrsAndGroup(t *testing.T) {
	var buf bytes.Buffer
	h := NewHandler(slog.NewJSONHandler(&buf, nil), "p")
	logger := slog.New(h).With("svc", "test")

	ctx := WithTrace(context.Background(), "t1", "s1")
	logger.InfoContext(ctx, "hello", "k", "v")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["svc"] != "test" || got["k"] != "v" {
		t.Fatalf("missing attrs: %v", got)
	}
	if got["logging.googleapis.com/trace"] != "projects/p/traces/t1" {
		t.Fatalf("trace field: %v", got["logging.googleapis.com/trace"])
	}
}
