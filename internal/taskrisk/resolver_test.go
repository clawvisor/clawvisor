package taskrisk

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/config"
)

// brokenAssessorServer returns 500 on every call so Assess()'s LLM
// invocation exhausts its single retry and falls through to the
// fail-soft "unknown" verdict. The test isn't asserting LLM behavior —
// it just needs Assess() to actually traverse the resolver branch.
func brokenAssessorServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newAssessorWithBrokenLLM(t *testing.T) *LLMAssessor {
	t.Helper()
	srv := brokenAssessorServer(t)
	cfg := config.LLMConfig{
		Verification: config.VerificationConfig{
			LLMProviderConfig: config.LLMProviderConfig{Enabled: true},
		},
		TaskRisk: config.TaskRiskConfig{
			LLMProviderConfig: config.LLMProviderConfig{
				Enabled:        true,
				Provider:       "openai",
				Endpoint:       srv.URL,
				APIKey:         "test-key",
				Model:          "test-model",
				TimeoutSeconds: 1,
			},
		},
	}
	health := llm.NewHealth(cfg)
	return NewLLMAssessor(health, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// resolverCallRecord captures invocations so the test can verify the
// real Assess() code path consulted the resolver, instead of
// duplicating the resolver-call snippet in the test body (the previous
// tautological shape).
type resolverCallRecord struct {
	mu       sync.Mutex
	orgs     []string
	override string
}

func (r *resolverCallRecord) fn(_ context.Context, orgID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.orgs = append(r.orgs, orgID)
	return r.override
}

func (r *resolverCallRecord) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.orgs))
	copy(out, r.orgs)
	return out
}

// TestAssess_InvokesResolverForOrgScopedRequest verifies the resolver
// is actually consulted by Assess() when OrgID is set and
// PromptOverride is empty. The LLM call is stubbed to fail so the
// test exercises only the resolver branch.
func TestAssess_InvokesResolverForOrgScopedRequest(t *testing.T) {
	a := newAssessorWithBrokenLLM(t)
	rec := &resolverCallRecord{override: "RESOLVED-PROMPT"}
	a.SetPromptResolver(rec.fn)

	_, err := a.Assess(context.Background(), AssessRequest{
		Purpose: "test", OrgID: "org-a",
	})
	if err != nil {
		t.Fatalf("Assess returned unexpected error: %v", err)
	}
	calls := rec.calls()
	if len(calls) != 1 || calls[0] != "org-a" {
		t.Fatalf("resolver calls = %v, want [org-a]", calls)
	}
}

// TestAssess_CallerProvidedOverrideWinsOverResolver verifies that a
// caller-provided PromptOverride shortcircuits the resolver — the
// resolver must not be consulted at all in that branch.
func TestAssess_CallerProvidedOverrideWinsOverResolver(t *testing.T) {
	a := newAssessorWithBrokenLLM(t)
	var called atomic.Bool
	a.SetPromptResolver(func(context.Context, string) string {
		called.Store(true)
		return "RESOLVED-PROMPT"
	})

	_, err := a.Assess(context.Background(), AssessRequest{
		Purpose:        "test",
		OrgID:          "org-a",
		PromptOverride: "CALLER-PROVIDED",
	})
	if err != nil {
		t.Fatalf("Assess returned unexpected error: %v", err)
	}
	if called.Load() {
		t.Error("resolver should not be called when caller pre-populated PromptOverride")
	}
}

// TestAssess_EmptyOrgIDSkipsResolver verifies the resolver isn't
// consulted for non-org-scoped requests (open-source build, admin
// sessions).
func TestAssess_EmptyOrgIDSkipsResolver(t *testing.T) {
	a := newAssessorWithBrokenLLM(t)
	var called atomic.Bool
	a.SetPromptResolver(func(context.Context, string) string {
		called.Store(true)
		return "RESOLVED-PROMPT"
	})

	_, err := a.Assess(context.Background(), AssessRequest{Purpose: "test", OrgID: ""})
	if err != nil {
		t.Fatalf("Assess returned unexpected error: %v", err)
	}
	if called.Load() {
		t.Error("resolver should not be called when OrgID is empty")
	}
}
