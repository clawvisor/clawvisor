package intent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/config"
)

// brokenServer always returns 500 so the LLM client treats every attempt as a
// transport failure, exercising the verifier's fail-open / fail-closed branch.
func brokenServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newVerifierWithCfg(t *testing.T, failClosed bool) *LLMVerifier {
	t.Helper()
	srv := brokenServer(t)
	cfg := config.LLMConfig{
		Verification: config.VerificationConfig{
			LLMProviderConfig: config.LLMProviderConfig{
				Enabled:        true,
				Provider:       "openai",
				Endpoint:       srv.URL,
				APIKey:         "test-key",
				Model:          "test-model",
				TimeoutSeconds: 1,
			},
			FailClosed:      failClosed,
			CacheTTLSeconds: 60,
		},
	}
	health := llm.NewHealth(cfg)
	return NewLLMVerifier(health, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func sampleVerifyReq() VerifyRequest {
	return VerifyRequest{
		TaskPurpose: "test purpose",
		Service:     "google.gmail",
		Action:      "list",
		Params:      map[string]any{"q": "is:unread"},
		Reason:      "fetch unread email",
		TaskID:      "task-fail-mode",
	}
}

func TestVerify_FailClosed_BlocksOnLLMError(t *testing.T) {
	v := newVerifierWithCfg(t, true)
	verdict, err := v.Verify(context.Background(), sampleVerifyReq())
	if err != nil {
		t.Fatalf("Verify returned error (should swallow into verdict): %v", err)
	}
	if verdict == nil {
		t.Fatalf("expected blocking verdict when FailClosed=true, got nil (fail-open)")
	}
	if verdict.Allow {
		t.Fatalf("expected Allow=false when FailClosed=true, got Allow=true")
	}
	if verdict.ParamScope != "n/a" || verdict.ReasonCoherence != "n/a" {
		t.Fatalf("expected n/a scope/reason on fail-closed, got scope=%q reason=%q",
			verdict.ParamScope, verdict.ReasonCoherence)
	}
}

func TestVerify_FailOpen_DegradesToNilVerdict(t *testing.T) {
	v := newVerifierWithCfg(t, false)
	verdict, err := v.Verify(context.Background(), sampleVerifyReq())
	if err != nil {
		t.Fatalf("Verify returned error (should swallow into nil verdict): %v", err)
	}
	if verdict != nil {
		t.Fatalf("expected nil verdict when FailClosed=false (verifier degrades), got %+v", verdict)
	}
}
