package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	runtimetiming "github.com/clawvisor/clawvisor/internal/runtime/timing"
	"github.com/clawvisor/clawvisor/internal/store/sqlite"
	intvault "github.com/clawvisor/clawvisor/internal/vault"
	"github.com/clawvisor/clawvisor/pkg/config"
)

var placeholderExtractRE = regexp.MustCompile(`autovault_[A-Za-z0-9._:-]+`)

func TestRuntimeSecretCaptureKnownPrefixAndReusePlaceholder(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-secret-capture.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	userID, agentID := seedRuntimePrincipal(t, st)
	session := createRuntimeSession(t, st, "capture-session", userID, agentID, false)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	cfg := config.Default()
	cfg.LLM.Verification.Enabled = false
	hooks := InboundSecretHooks{Store: st, Vault: v, Config: cfg}
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"use ghp_exampleSecret123456789 now"}]}]}`)

	first, summary, observed, err := srv.scanAndReplaceRuntimeSecrets(ctx, hooks, runtimeSession, "api.anthropic.com", body)
	if err != nil {
		t.Fatalf("scanAndReplaceRuntimeSecrets(first): %v", err)
	}
	if observed != 0 || summary == nil || summary.ReplacementCount != 1 {
		t.Fatalf("unexpected first summary: summary=%+v observed=%d", summary, observed)
	}
	firstPlaceholder := string(placeholderExtractRE.Find(first))
	if firstPlaceholder == "" || strings.Contains(string(first), "ghp_exampleSecret123456789") {
		t.Fatalf("expected rewritten placeholder body, got %s", string(first))
	}

	second, summary, observed, err := srv.scanAndReplaceRuntimeSecrets(ctx, hooks, runtimeSession, "api.anthropic.com", body)
	if err != nil {
		t.Fatalf("scanAndReplaceRuntimeSecrets(second): %v", err)
	}
	if observed != 0 || summary == nil || summary.ReplacementCount != 1 {
		t.Fatalf("unexpected second summary: summary=%+v observed=%d", summary, observed)
	}
	secondPlaceholder := string(placeholderExtractRE.Find(second))
	if secondPlaceholder != firstPlaceholder {
		t.Fatalf("expected placeholder reuse, got first=%q second=%q", firstPlaceholder, secondPlaceholder)
	}
	meta, err := st.GetRuntimePlaceholder(ctx, firstPlaceholder)
	if err != nil {
		t.Fatalf("GetRuntimePlaceholder: %v", err)
	}
	cred, err := v.Get(ctx, userID, meta.ServiceID)
	if err != nil {
		t.Fatalf("vault.Get: %v", err)
	}
	if string(cred) != "ghp_exampleSecret123456789" {
		t.Fatalf("expected captured secret in vault, got %q", string(cred))
	}
}

func TestRuntimeSecretCaptureRecordsScanSubspans(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-secret-spans.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	userID, agentID := seedRuntimePrincipal(t, st)
	session := createRuntimeSession(t, st, "span-session", userID, agentID, false)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	cfg := config.Default()
	cfg.LLM.Verification.Enabled = false
	hooks := InboundSecretHooks{Store: st, Vault: v, Config: cfg}
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"api_key=ZXhhbXBsZV9zZWNyZXRfdG9rZW5fMTIzNDU2Nzg5 and the password is hunter2secret99"}]}]}`)
	recorder := &runtimetiming.Recorder{}

	_, summary, observed, err := srv.scanAndReplaceRuntimeSecrets(runtimetiming.WithRecorder(ctx, recorder), hooks, runtimeSession, "api.openai.com", body)
	if err != nil {
		t.Fatalf("scanAndReplaceRuntimeSecrets: %v", err)
	}
	if observed != 0 || summary == nil || summary.ReplacementCount < 2 {
		t.Fatalf("unexpected summary: summary=%+v observed=%d", summary, observed)
	}

	totals := recorder.Totals()
	for _, name := range []string{
		"inbound_secret.scan.unmarshal",
		"inbound_secret.scan.walk",
		"inbound_secret.scan.marshal",
		"inbound_secret.scan.known_prefix",
		"inbound_secret.scan.context_noise_check",
		"inbound_secret.scan.strip_tags",
		"inbound_secret.scan.detect_candidates",
		"inbound_secret.scan.find_passwords",
	} {
		if _, ok := totals[name]; !ok {
			t.Fatalf("expected timing span %q in %+v", name, totals)
		}
	}
	attrs := recorder.Attrs()
	if got := attrs["inbound_secret.scan.strings_seen"]; got == nil {
		t.Fatalf("expected strings_seen attr in %+v", attrs)
	}
	if got := attrs["inbound_secret.scan.candidates"]; got == nil {
		t.Fatalf("expected candidates attr in %+v", attrs)
	}
	if got := attrs["inbound_secret.scan.passwords"]; got == nil {
		t.Fatalf("expected passwords attr in %+v", attrs)
	}
}

func TestRuntimeSecretCaptureHeuristicAndPasswordReveal(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-secret-heuristic.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	userID, agentID := seedRuntimePrincipal(t, st)
	session := createRuntimeSession(t, st, "heuristic-session", userID, agentID, false)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	hooks := InboundSecretHooks{Store: st, Vault: v, Config: config.Default()}
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"api_key=ZXhhbXBsZV9zZWNyZXRfdG9rZW5fMTIzNDU2Nzg5 and the password is hunter2secret99"}]}]}`)

	rewritten, summary, observed, err := srv.scanAndReplaceRuntimeSecrets(ctx, hooks, runtimeSession, "api.openai.com", body)
	if err != nil {
		t.Fatalf("scanAndReplaceRuntimeSecrets: %v", err)
	}
	if observed != 0 || summary == nil || summary.ReplacementCount < 2 {
		t.Fatalf("unexpected summary: summary=%+v observed=%d", summary, observed)
	}
	rewrittenBody := string(rewritten)
	if strings.Contains(rewrittenBody, "ZXhhbXBsZV9zZWNyZXRfdG9rZW5fMTIzNDU2Nzg5") || strings.Contains(rewrittenBody, "hunter2secret99") {
		t.Fatalf("expected both heuristic and password replacements, got %s", rewrittenBody)
	}
}

func TestRuntimeSecretCaptureObservesAmbiguousCandidateWithoutReplacing(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-secret-observe.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	userID, agentID := seedRuntimePrincipal(t, st)
	session := createRuntimeSession(t, st, "observe-session", userID, agentID, false)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	hooks := InboundSecretHooks{Store: st, Vault: v, Config: config.Default()}
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"maybe use this id 123e4567-e89b-12d3-a456-426614174000 later"}]}]}`)

	rewritten, summary, observed, err := srv.scanAndReplaceRuntimeSecrets(ctx, hooks, runtimeSession, "api.openai.com", body)
	if err != nil {
		t.Fatalf("scanAndReplaceRuntimeSecrets: %v", err)
	}
	if strings.Contains(string(rewritten), "autovault_") {
		t.Fatalf("expected observe-only body without placeholders, got %s", string(rewritten))
	}
	if observed == 0 || summary == nil || summary.ReplacementCount != 0 {
		t.Fatalf("unexpected observe-only summary: summary=%+v observed=%d", summary, observed)
	}
}

func TestRuntimeSecretCaptureSkipsNoiseSubtreesForHeuristics(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-secret-noise.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	userID, agentID := seedRuntimePrincipal(t, st)
	session := createRuntimeSession(t, st, "noise-session", userID, agentID, false)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	var adjudicatorCalls int64
	adjudicator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&adjudicatorCalls, 1)
		_, _ = w.Write([]byte(`{"credential":false,"service":"","confidence":0.9}`))
	}))
	defer adjudicator.Close()

	cfg := config.Default()
	cfg.LLM.Verification.Enabled = true
	cfg.LLM.Verification.Endpoint = adjudicator.URL
	cfg.LLM.Verification.APIKey = "test"
	cfg.LLM.Verification.Model = "haiku"

	body := []byte(`{
  "model": "claude-haiku-4-5-20251001",
  "system": "You are Claude. SystemNoise_8gyXD1ddhvF8iEFwrt9f3ywd is random.",
  "tools": [{"name":"foo","description":"Uses ToolsBlob_8gyXD1ddhvF8iEFwrt9f3ywd internally."}],
  "metadata": {"user_id":"123e4567-e89b-12d3-a456-426614174000"},
  "messages": [{"role":"user","content":[{"type":"text","text":"hello"}]}]
}`)

	rewritten, summary, observed, err := srv.scanAndReplaceRuntimeSecrets(ctx, InboundSecretHooks{Store: st, Vault: v, Config: cfg}, runtimeSession, "api.anthropic.com", body)
	if err != nil {
		t.Fatalf("scanAndReplaceRuntimeSecrets: %v", err)
	}
	if got := atomic.LoadInt64(&adjudicatorCalls); got != 0 {
		t.Fatalf("expected zero adjudicator calls for top-level noise fields, got %d", got)
	}
	if summary != nil && summary.ReplacementCount != 0 {
		t.Fatalf("expected no replacements from noise fields, got %+v", summary)
	}
	if observed != 0 {
		t.Fatalf("expected no observe-only detections from noise fields, got %d", observed)
	}
	if string(rewritten) != string(body) {
		t.Fatalf("expected noise-only body to remain unchanged, got %s", string(rewritten))
	}
}

func TestRuntimeSecretCaptureSkipsHarnessMetadataTagsForHeuristics(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-secret-reminder.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	userID, agentID := seedRuntimePrincipal(t, st)
	session := createRuntimeSession(t, st, "reminder-session", userID, agentID, false)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	var adjudicatorCalls int64
	adjudicator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&adjudicatorCalls, 1)
		_, _ = w.Write([]byte(`{"credential":false,"service":"","confidence":0.9}`))
	}))
	defer adjudicator.Close()

	cfg := config.Default()
	cfg.LLM.Verification.Enabled = true
	cfg.LLM.Verification.Endpoint = adjudicator.URL
	cfg.LLM.Verification.APIKey = "test"
	cfg.LLM.Verification.Model = "haiku"

	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>noise: Aa1Bb2Cc3Dd4Ee5Ff6Gg7Hh8Ii9Jj0 and SystemNoise_8gyXD1ddhvF8iEFwrt9f3ywd</system-reminder> hello"}]}]}`)
	rewritten, summary, observed, err := srv.scanAndReplaceRuntimeSecrets(ctx, InboundSecretHooks{Store: st, Vault: v, Config: cfg}, runtimeSession, "api.anthropic.com", body)
	if err != nil {
		t.Fatalf("scanAndReplaceRuntimeSecrets: %v", err)
	}
	if got := atomic.LoadInt64(&adjudicatorCalls); got != 0 {
		t.Fatalf("expected zero adjudicator calls for harness metadata tags, got %d", got)
	}
	if summary != nil && summary.ReplacementCount != 0 {
		t.Fatalf("expected no replacements from system-reminder contents, got %+v", summary)
	}
	if observed != 0 {
		t.Fatalf("expected no observe-only detections from system-reminder contents, got %d", observed)
	}
	if string(rewritten) != string(body) {
		t.Fatalf("expected system-reminder-only body to remain unchanged, got %s", string(rewritten))
	}
}

func TestRuntimeSecretCaptureSkipsClaudeMemoryContextForHeuristics(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-secret-claudemd.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	userID, agentID := seedRuntimePrincipal(t, st)
	session := createRuntimeSession(t, st, "claudemd-session", userID, agentID, false)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	var adjudicatorCalls int64
	adjudicator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&adjudicatorCalls, 1)
		_, _ = w.Write([]byte(`{"credential":false,"service":"","confidence":0.9}`))
	}))
	defer adjudicator.Close()

	cfg := config.Default()
	cfg.LLM.Verification.Enabled = true
	cfg.LLM.Verification.Endpoint = adjudicator.URL
	cfg.LLM.Verification.APIKey = "test"
	cfg.LLM.Verification.Model = "haiku"

	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"As you answer the user's questions, you can use the following context:\n# claudeMd\nContents of /tmp/MEMORY.md:\n- [project_onboarding_hitlist.md](project_onboarding_hitlist.md)\n- [project_power_user_gbrain.md](project_power_user_gbrain.md)\n# currentDate\nToday's date is 2026-04-30."}]}]}`)
	rewritten, summary, observed, err := srv.scanAndReplaceRuntimeSecrets(ctx, InboundSecretHooks{Store: st, Vault: v, Config: cfg}, runtimeSession, "api.anthropic.com", body)
	if err != nil {
		t.Fatalf("scanAndReplaceRuntimeSecrets: %v", err)
	}
	if got := atomic.LoadInt64(&adjudicatorCalls); got != 0 {
		t.Fatalf("expected zero adjudicator calls for claude memory context, got %d", got)
	}
	if summary != nil && summary.ReplacementCount != 0 {
		t.Fatalf("expected no replacements from claude memory context, got %+v", summary)
	}
	if observed != 0 {
		t.Fatalf("expected no observe-only detections from claude memory context, got %d", observed)
	}
	if string(rewritten) != string(body) {
		t.Fatalf("expected claude memory context body to remain unchanged, got %s", string(rewritten))
	}
}

func TestRuntimeSecretCaptureSkipsClaudeProtocolIdentifiersForHeuristics(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-secret-protocol-noise.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	userID, agentID := seedRuntimePrincipal(t, st)
	session := createRuntimeSession(t, st, "protocol-noise-session", userID, agentID, false)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	var adjudicatorCalls int64
	adjudicator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&adjudicatorCalls, 1)
		_, _ = w.Write([]byte(`{"credential":false,"service":"","confidence":0.9}`))
	}))
	defer adjudicator.Close()

	cfg := config.Default()
	cfg.LLM.Verification.Enabled = true
	cfg.LLM.Verification.Endpoint = adjudicator.URL
	cfg.LLM.Verification.APIKey = "test"
	cfg.LLM.Verification.Model = "haiku"

	body := []byte(`{
  "context_management": {"edits":[{"type":"clear_thinking_20251015"}]},
  "messages": [
    {"role":"assistant","content":[{"type":"tool_use","id":"toolu_01YXJxtnm9Ab3vXYRNMj8bgs","name":"Glob","input":{"pattern":"**/hello*","path":"/tmp"}}]},
    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01YXJxtnm9Ab3vXYRNMj8bgs","content":"Found 3 files"}]}
  ]
}`)

	rewritten, summary, observed, err := srv.scanAndReplaceRuntimeSecrets(ctx, InboundSecretHooks{Store: st, Vault: v, Config: cfg}, runtimeSession, "api.anthropic.com", body)
	if err != nil {
		t.Fatalf("scanAndReplaceRuntimeSecrets: %v", err)
	}
	if got := atomic.LoadInt64(&adjudicatorCalls); got != 0 {
		t.Fatalf("expected zero adjudicator calls for Claude protocol identifiers, got %d", got)
	}
	if summary != nil && summary.ReplacementCount != 0 {
		t.Fatalf("expected no replacements from protocol identifiers, got %+v", summary)
	}
	if observed != 0 {
		t.Fatalf("expected no observe-only detections from protocol identifiers, got %d", observed)
	}
	if string(rewritten) != string(body) {
		t.Fatalf("expected protocol-noise body to remain unchanged, got %s", string(rewritten))
	}
}

func TestRuntimeSecretCaptureDoesNotRewriteThinkingBlocksOrSignatures(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-secret-thinking.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	userID, agentID := seedRuntimePrincipal(t, st)
	session := createRuntimeSession(t, st, "thinking-session", userID, agentID, false)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"sk-ant-api03-should-not-change","signature":"EvIBautovaultprefixsk-ant-api03-should-not-change"}]},{"role":"user","content":"Can you find the hello world script in /tmp?"}]}`)
	rewritten, summary, observed, err := srv.scanAndReplaceRuntimeSecrets(ctx, InboundSecretHooks{Store: st, Vault: v, Config: config.Default()}, runtimeSession, "api.anthropic.com", body)
	if err != nil {
		t.Fatalf("scanAndReplaceRuntimeSecrets: %v", err)
	}
	if summary != nil || observed != 0 {
		t.Fatalf("expected no secret capture in thinking block, got summary=%+v observed=%d", summary, observed)
	}
	if string(rewritten) != string(body) {
		t.Fatalf("expected thinking/signature block to remain unchanged, got %s", string(rewritten))
	}
}

func TestRuntimeSecretCapturePlaceholdersResolveThroughOutboundSwap(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "runtime-secret-e2e.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	v, err := intvault.NewLocalVault(filepath.Join(t.TempDir(), "vault.key"), db, "sqlite")
	if err != nil {
		t.Fatalf("NewLocalVault: %v", err)
	}
	userID, agentID := seedRuntimePrincipal(t, st)
	session := createRuntimeSession(t, st, "capture-swap-session", userID, agentID, false)
	runtimeSession, err := st.GetRuntimeSession(ctx, session.id)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	srv, err := NewServer(Config{DataDir: t.TempDir(), Addr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"use ghp_bridgeSecret123456789 for github"}]}]}`)
	rewritten, summary, observed, err := srv.scanAndReplaceRuntimeSecrets(ctx, InboundSecretHooks{Store: st, Vault: v, Config: config.Default()}, runtimeSession, "api.anthropic.com", body)
	if err != nil {
		t.Fatalf("scanAndReplaceRuntimeSecrets: %v", err)
	}
	if observed != 0 || summary == nil || summary.ReplacementCount != 1 {
		t.Fatalf("unexpected capture summary: summary=%+v observed=%d", summary, observed)
	}
	placeholder := string(placeholderExtractRE.Find(rewritten))
	if placeholder == "" {
		t.Fatalf("expected placeholder in rewritten body: %s", string(rewritten))
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer ghp_bridgeSecret123456789" {
			t.Fatalf("expected outbound placeholder swap, got %q", got)
		}
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	srv.InstallSessionGuard(&Authenticator{Store: st})
	srv.InstallPlaceholderSwap(PlaceholderHooks{Store: st, Vault: v})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Shutdown(ctx) }()

	client := proxyHTTPClient(t, srv)
	req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+session.secret)
	req.Header.Set("Authorization", "Bearer "+placeholder)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(out) != "ok" {
		t.Fatalf("expected upstream success, got %d %q", resp.StatusCode, string(out))
	}
}
