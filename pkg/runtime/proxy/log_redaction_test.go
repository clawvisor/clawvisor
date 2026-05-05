package proxy

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func newRedactingTextLogger(buf *bytes.Buffer) *slog.Logger {
	base := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(NewRedactingHandler(base))
}

func TestRedactingHandler_AllowlistedKeysPassThrough(t *testing.T) {
	var buf bytes.Buffer
	logger := newRedactingTextLogger(&buf)
	logger.Info("ok", "session_id", "s1", "user_id", "u1", "method", "GET", "host", "api.example.com")

	out := buf.String()
	for _, want := range []string{`session_id=s1`, `user_id=u1`, `method=GET`, `host=api.example.com`} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in log; got %s", want, out)
		}
	}
}

func TestRedactingHandler_DenylistedKeysRedacted(t *testing.T) {
	var buf bytes.Buffer
	logger := newRedactingTextLogger(&buf)
	logger.Warn("denied",
		"authorization", "Bearer sk-real-secret",
		"cookie", "session=abc",
		"api_key", "sk-real",
		"body", `{"prompt":"hello"}`,
		"password", "hunter2",
	)
	out := buf.String()
	for _, leaked := range []string{"sk-real-secret", "Bearer", "session=abc", "hello", "hunter2", "sk-real"} {
		if strings.Contains(out, leaked) {
			t.Errorf("LEAK: %q appears in log output: %s", leaked, out)
		}
	}
	if !strings.Contains(out, "<redacted>") {
		t.Errorf("expected <redacted> placeholder; got %s", out)
	}
}

func TestRedactingHandler_UnknownKeyRedacted(t *testing.T) {
	var buf bytes.Buffer
	logger := newRedactingTextLogger(&buf)
	logger.Info("event", "session_id", "s1", "weird_new_field", "sensitive-value")

	out := buf.String()
	if strings.Contains(out, "sensitive-value") {
		t.Errorf("LEAK: unknown key value %q in log: %s", "sensitive-value", out)
	}
	if !strings.Contains(out, "session_id=s1") {
		t.Errorf("expected session_id=s1 in log: %s", out)
	}
	if !strings.Contains(out, "weird_new_field=<redacted>") {
		t.Errorf("expected redacted placeholder for unknown key: %s", out)
	}
}

func TestRedactingHandler_WithAttrs(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := NewRedactingHandler(base)
	logger := slog.New(h.WithAttrs([]slog.Attr{
		slog.String("session_id", "s1"),
		slog.String("authorization", "Bearer leak"),
	}))
	logger.Info("base")

	out := buf.String()
	if strings.Contains(out, "Bearer leak") {
		t.Errorf("LEAK: authorization in WithAttrs survived: %s", out)
	}
	if !strings.Contains(out, "session_id=s1") {
		t.Errorf("expected session_id=s1 in log: %s", out)
	}
}

func TestRedactingHandler_NilWrapped(t *testing.T) {
	if got := NewRedactingHandler(nil); got != nil {
		t.Errorf("NewRedactingHandler(nil) = %v, want nil", got)
	}
	if got := NewRedactingHandlerWithAllowlist(nil, AllowedLogKeys); got != nil {
		t.Errorf("NewRedactingHandlerWithAllowlist(nil, …) = %v, want nil", got)
	}
}

func TestRedactingHandler_RespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	logger := slog.New(NewRedactingHandler(base))
	logger.Info("info-msg", "session_id", "s1")
	logger.Warn("warn-msg", "session_id", "s1")

	out := buf.String()
	if strings.Contains(out, "info-msg") {
		t.Errorf("Info should be filtered by base handler; got %s", out)
	}
	if !strings.Contains(out, "warn-msg") {
		t.Errorf("Warn should pass through; got %s", out)
	}
	_ = context.Background
}
