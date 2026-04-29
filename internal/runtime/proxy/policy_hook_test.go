package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	runtimepolicy "github.com/clawvisor/clawvisor/internal/runtime/policy"
	"github.com/clawvisor/clawvisor/internal/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestRuntimeProxyAllowsMatchedTaskAndConsumesOneOffApprovals(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/runtime.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	userID, agentID := seedRuntimePrincipal(t, st)

	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = true
	cfg.RuntimeProxy.DataDir = t.TempDir()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)

	if err := st.CreateTask(ctx, &store.Task{
		ID:            "task-1",
		UserID:        userID,
		AgentID:       agentID,
		Purpose:       "read runtime API",
		Status:        "active",
		Lifetime:      "session",
		SchemaVersion: 2,
		ExpectedEgress: mustJSON(t, []map[string]any{{
			"host":   upstreamURL.Hostname(),
			"method": "GET",
			"path":   "/",
			"why":    "Read the downstream runtime API status.",
		}}),
		ExpiresInSeconds: 3600,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	session := createRuntimeSession(t, st, "session-1", userID, agentID, false)
	srv := newStartedRuntimeProxy(t, st, cfg)
	defer func() { _ = srv.Shutdown(ctx) }()

	client := proxyHTTPClient(t, srv)
	req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+session.secret)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("expected upstream success, got %d %q", resp.StatusCode, string(body))
	}

	req, _ = http.NewRequest(http.MethodGet, upstream.URL+"/other", nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+session.secret)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("blocked proxy request: %v", err)
	}
	blockedBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected runtime approval block, got %d", resp.StatusCode)
	}
	var blocked map[string]any
	if err := json.Unmarshal(blockedBody, &blocked); err != nil {
		t.Fatalf("unmarshal blocked response: %v", err)
	}
	fingerprint := blocked["request_fingerprint"].(string)
	approvalID := blocked["approval_id"].(string)
	if approvalID == "" || fingerprint == "" {
		t.Fatalf("expected approval_id and request_fingerprint, got %v", blocked)
	}

	if err := st.CreateOneOffApproval(ctx, &store.OneOffApproval{
		SessionID:          session.id,
		RequestFingerprint: fingerprint,
		ApprovalID:         &approvalID,
		ApprovedAt:         time.Now().UTC(),
		ExpiresAt:          time.Now().UTC().Add(5 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateOneOffApproval: %v", err)
	}

	req, _ = http.NewRequest(http.MethodGet, upstream.URL+"/other", nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+session.secret)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("one-off proxy request: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("expected one-off upstream success, got %d %q", resp.StatusCode, string(body))
	}
	events, err := st.ListRuntimeEvents(ctx, userID, store.RuntimeEventFilter{SessionID: session.id, Limit: 20})
	if err != nil {
		t.Fatalf("ListRuntimeEvents: %v", err)
	}
	assertRuntimeEventTypes(t, events, "runtime.egress.allowed", "runtime.egress.review_required", "runtime.egress.one_off_consumed")
}

func TestRuntimeProxyOneOffApprovalsRemainBoundToOriginatingSession(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/runtime-cross-session.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	userID, agentID := seedRuntimePrincipal(t, st)

	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = true
	cfg.RuntimeProxy.DataDir = t.TempDir()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	firstSession := createRuntimeSession(t, st, "session-a", userID, agentID, false)
	secondSession := createRuntimeSession(t, st, "session-b", userID, agentID, false)
	srv := newStartedRuntimeProxy(t, st, cfg)
	defer func() { _ = srv.Shutdown(ctx) }()

	client := proxyHTTPClient(t, srv)
	req, _ := http.NewRequest(http.MethodGet, upstream.URL+"/blocked", nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+firstSession.secret)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("blocked proxy request: %v", err)
	}
	blockedBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected runtime approval block, got %d", resp.StatusCode)
	}
	var blocked map[string]any
	if err := json.Unmarshal(blockedBody, &blocked); err != nil {
		t.Fatalf("unmarshal blocked response: %v", err)
	}
	fingerprint := blocked["request_fingerprint"].(string)
	approvalID := blocked["approval_id"].(string)

	if err := st.CreateOneOffApproval(ctx, &store.OneOffApproval{
		SessionID:          firstSession.id,
		RequestFingerprint: fingerprint,
		ApprovalID:         &approvalID,
		ApprovedAt:         time.Now().UTC(),
		ExpiresAt:          time.Now().UTC().Add(5 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateOneOffApproval: %v", err)
	}

	req, _ = http.NewRequest(http.MethodGet, upstream.URL+"/blocked", nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+secondSession.secret)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("cross-session proxy request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected cross-session request to remain blocked, got %d %q", resp.StatusCode, string(body))
	}
}

func TestRuntimeProxyObservationModeAllowsAndAudits(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/runtime-observe.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	userID, agentID := seedRuntimePrincipal(t, st)

	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = true
	cfg.RuntimeProxy.DataDir = t.TempDir()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	session := createRuntimeSession(t, st, "session-observe", userID, agentID, true)
	srv := newStartedRuntimeProxy(t, st, cfg)
	defer func() { _ = srv.Shutdown(ctx) }()

	client := proxyHTTPClient(t, srv)
	req, _ := http.NewRequest(http.MethodGet, upstream.URL+"/miss", nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+session.secret)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("expected observation-mode passthrough, got %d %q", resp.StatusCode, string(body))
	}

	entries, _, err := st.ListAuditEntries(ctx, userID, store.AuditFilter{Limit: 20})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected audit entry for observation-mode request")
	}
	if !entries[0].WouldBlock || !entries[0].WouldReview {
		t.Fatalf("expected observation audit flags, got %+v", entries[0])
	}
}

func TestRuntimeProxyAcceptsBasicProxyCredentials(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/runtime-basic.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	userID, agentID := seedRuntimePrincipal(t, st)

	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = true
	cfg.RuntimeProxy.DataDir = t.TempDir()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)

	if err := st.CreateTask(ctx, &store.Task{
		ID:            "task-basic",
		UserID:        userID,
		AgentID:       agentID,
		Purpose:       "read runtime API with authenticated proxy URL",
		Status:        "active",
		Lifetime:      "session",
		SchemaVersion: 2,
		ExpectedEgress: mustJSON(t, []map[string]any{{
			"host":   upstreamURL.Hostname(),
			"method": "GET",
			"path":   "/",
			"why":    "Read the downstream runtime API status.",
		}}),
		ExpiresInSeconds: 3600,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	session := createRuntimeSession(t, st, "session-basic", userID, agentID, false)
	srv := newStartedRuntimeProxy(t, st, cfg)
	defer func() { _ = srv.Shutdown(ctx) }()

	client := proxyHTTPClient(t, srv)
	req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("clawvisor:"+session.secret)))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("expected upstream success, got %d %q", resp.StatusCode, string(body))
	}
}

func TestRuntimeProxyContextJudgeCanBindUnmatchedEgressToExistingTask(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/runtime-judge-allow.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	userID, agentID := seedRuntimePrincipal(t, st)

	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = true
	cfg.RuntimeProxy.DataDir = t.TempDir()

	task := &store.Task{
		ID:            "task-judge-existing",
		UserID:        userID,
		AgentID:       agentID,
		Purpose:       "Investigate runtime issue",
		Status:        "active",
		Lifetime:      "session",
		SchemaVersion: 2,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	session := createRuntimeSession(t, st, "session-judge-allow", userID, agentID, false)
	srv := newStartedRuntimeProxyWithJudge(t, st, cfg, &stubRuntimeContextJudge{
		judgment: runtimepolicy.RuntimeContextJudgment{
			Kind:        runtimepolicy.ClassificationBelongsToExistingTask,
			MatchedTask: task,
			Rationale:   "same investigation workflow",
		},
	})
	defer func() { _ = srv.Shutdown(ctx) }()

	client := proxyHTTPClient(t, srv)
	req, _ := http.NewRequest(http.MethodPost, upstream.URL+"/tickets", nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+session.secret)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("expected upstream success, got %d %q", resp.StatusCode, string(body))
	}

	entries, _, err := st.ListAuditEntries(ctx, userID, store.AuditFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(entries) == 0 || !entries[0].UsedConvJudgeResolution || entries[0].TaskID == nil || *entries[0].TaskID != task.ID {
		t.Fatalf("expected judge-attributed audit entry, got %+v", entries)
	}
}

func TestRuntimeProxyContextJudgeCanPromoteUnmatchedEgressToTaskReview(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/runtime-judge-review.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	userID, agentID := seedRuntimePrincipal(t, st)

	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = true
	cfg.RuntimeProxy.DataDir = t.TempDir()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	session := createRuntimeSession(t, st, "session-judge-review", userID, agentID, false)
	srv := newStartedRuntimeProxyWithJudge(t, st, cfg, &stubRuntimeContextJudge{
		judgment: runtimepolicy.RuntimeContextJudgment{
			Kind:           runtimepolicy.ClassificationNeedsNewTask,
			ResolutionHint: "allow_session",
			Rationale:      "write action should promote into task scope",
		},
	})
	defer func() { _ = srv.Shutdown(ctx) }()

	client := proxyHTTPClient(t, srv)
	req, _ := http.NewRequest(http.MethodPost, upstream.URL+"/tickets", nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+session.secret)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected runtime review, got %d %q", resp.StatusCode, string(body))
	}
	var blocked map[string]any
	if err := json.Unmarshal(body, &blocked); err != nil {
		t.Fatalf("unmarshal blocked body: %v", err)
	}
	approvalID, _ := blocked["approval_id"].(string)
	if approvalID == "" {
		t.Fatalf("expected approval_id, got %v", blocked)
	}
	approval, err := st.GetApprovalRecord(ctx, approvalID)
	if err != nil {
		t.Fatalf("GetApprovalRecord: %v", err)
	}
	if approval.Kind != "task_create" {
		t.Fatalf("expected task_create runtime approval, got %+v", approval)
	}
}

func TestRuntimeProxyPrefersActiveTaskSessionBinding(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/runtime-task-bias.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	userID, agentID := seedRuntimePrincipal(t, st)

	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = true
	cfg.RuntimeProxy.DataDir = t.TempDir()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)

	for _, taskID := range []string{"task-a", "task-b"} {
		if err := st.CreateTask(ctx, &store.Task{
			ID:            taskID,
			UserID:        userID,
			AgentID:       agentID,
			Purpose:       "overlapping runtime task " + taskID,
			Status:        "active",
			Lifetime:      "session",
			SchemaVersion: 2,
			ExpectedEgress: mustJSON(t, []map[string]any{{
				"host":   upstreamURL.Hostname(),
				"method": "GET",
				"path":   "/",
				"why":    "Overlap for active-session bias test.",
			}}),
			ExpiresInSeconds: 3600,
		}); err != nil {
			t.Fatalf("CreateTask(%s): %v", taskID, err)
		}
	}

	session := createRuntimeSession(t, st, "session-bias", userID, agentID, false)
	now := time.Now().UTC()
	if err := st.UpsertActiveTaskSession(ctx, &store.ActiveTaskSession{
		TaskID:     "task-b",
		SessionID:  session.id,
		UserID:     userID,
		AgentID:    agentID,
		Status:     "active",
		StartedAt:  now,
		LastSeenAt: now,
	}); err != nil {
		t.Fatalf("UpsertActiveTaskSession: %v", err)
	}

	srv := newStartedRuntimeProxy(t, st, cfg)
	defer func() { _ = srv.Shutdown(ctx) }()

	client := proxyHTTPClient(t, srv)
	req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+session.secret)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("expected upstream success, got %d %q", resp.StatusCode, string(body))
	}

	entries, _, err := st.ListAuditEntries(ctx, userID, store.AuditFilter{Limit: 20})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected audit entry")
	}
	if entries[0].MatchedTaskID == nil || *entries[0].MatchedTaskID != "task-b" {
		t.Fatalf("expected matched task to prefer active session binding, got %+v", entries[0].MatchedTaskID)
	}
}

func TestRuntimeProxyDeterministicMatchBeatsLeaseBias(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/runtime-lease-bias.db")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := sqlite.NewStore(db)
	userID, agentID := seedRuntimePrincipal(t, st)

	cfg := config.Default()
	cfg.RuntimeProxy.Enabled = true
	cfg.RuntimeProxy.DataDir = t.TempDir()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)

	if err := st.CreateTask(ctx, &store.Task{
		ID:            "task-lease",
		UserID:        userID,
		AgentID:       agentID,
		Purpose:       "broad runtime task",
		Status:        "active",
		Lifetime:      "session",
		SchemaVersion: 2,
		ExpectedEgress: mustJSON(t, []map[string]any{{
			"host":       upstreamURL.Hostname(),
			"method":     "GET",
			"path_regex": "^/v1/.*$",
			"why":        "Broad runtime API access.",
		}}),
		ExpiresInSeconds: 3600,
	}); err != nil {
		t.Fatalf("CreateTask(task-lease): %v", err)
	}
	if err := st.CreateTask(ctx, &store.Task{
		ID:            "task-specific",
		UserID:        userID,
		AgentID:       agentID,
		Purpose:       "specific runtime task",
		Status:        "active",
		Lifetime:      "session",
		SchemaVersion: 2,
		ExpectedEgress: mustJSON(t, []map[string]any{{
			"host":   upstreamURL.Hostname(),
			"method": "GET",
			"path":   "/v1/search",
			"why":    "Specific runtime API search access.",
		}}),
		ExpiresInSeconds: 3600,
	}); err != nil {
		t.Fatalf("CreateTask(task-specific): %v", err)
	}

	session := createRuntimeSession(t, st, "session-lease-bias", userID, agentID, false)
	if err := st.CreateToolExecutionLease(ctx, &store.ToolExecutionLease{
		LeaseID:   "lease-1",
		SessionID: session.id,
		TaskID:    "task-lease",
		ToolUseID: "toolu_lease",
		ToolName:  "fetch_messages",
		Status:    "open",
		OpenedAt:  time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(5 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateToolExecutionLease: %v", err)
	}

	srv := newStartedRuntimeProxy(t, st, cfg)
	defer func() { _ = srv.Shutdown(ctx) }()

	client := proxyHTTPClient(t, srv)
	req, _ := http.NewRequest(http.MethodGet, upstream.URL+"/v1/search", nil)
	req.Header.Set("Proxy-Authorization", "Bearer "+session.secret)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("expected upstream success, got %d %q", resp.StatusCode, string(body))
	}

	entries, _, err := st.ListAuditEntries(ctx, userID, store.AuditFilter{Limit: 20})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected audit entry")
	}
	if entries[0].MatchedTaskID == nil || *entries[0].MatchedTaskID != "task-specific" {
		t.Fatalf("expected deterministic match to prefer task-specific, got %+v", entries[0].MatchedTaskID)
	}
	if entries[0].LeaseTaskID == nil || *entries[0].LeaseTaskID != "task-lease" {
		t.Fatalf("expected lease attribution to record task-lease, got %+v", entries[0].LeaseTaskID)
	}
	if entries[0].UsedLeaseBias {
		t.Fatalf("expected deterministic match to beat lease bias, got %+v", entries[0])
	}
}

type runtimeTestSession struct {
	id     string
	secret string
}

func createRuntimeSession(t *testing.T, st store.Store, sessionID, userID, agentID string, observation bool) runtimeTestSession {
	t.Helper()
	secret := "test-secret-" + sessionID
	sess := &store.RuntimeSession{
		ID:                    sessionID,
		UserID:                userID,
		AgentID:               agentID,
		Mode:                  "proxy",
		ProxyBearerSecretHash: HashProxyBearerSecret(secret),
		ObservationMode:       observation,
		ExpiresAt:             time.Now().UTC().Add(30 * time.Minute),
	}
	if err := st.CreateRuntimeSession(context.Background(), sess); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	return runtimeTestSession{id: sessionID, secret: secret}
}

func seedRuntimePrincipal(t *testing.T, st store.Store) (string, string) {
	t.Helper()
	ctx := context.Background()
	email := "runtime@test.example"
	if _, err := st.CreateUser(ctx, email, "hash"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	user, err := st.GetUserByEmail(ctx, email)
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, "runtime-agent", "hash")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return user.ID, agent.ID
}

func newStartedRuntimeProxy(t *testing.T, st store.Store, cfg *config.Config) *Server {
	t.Helper()
	srv, err := NewServer(Config{
		DataDir: cfg.RuntimeProxy.DataDir,
		Addr:    "127.0.0.1:0",
	}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.InstallSessionGuard(&Authenticator{Store: st})
	srv.InstallRequestContextCarrier()
	srv.InstallEgressPolicy(PolicyHooks{Store: st, Config: cfg})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return srv
}

func newStartedRuntimeProxyWithJudge(t *testing.T, st store.Store, cfg *config.Config, judge runtimepolicy.RuntimeContextJudge) *Server {
	t.Helper()
	srv, err := NewServer(Config{
		DataDir: cfg.RuntimeProxy.DataDir,
		Addr:    "127.0.0.1:0",
	}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.InstallSessionGuard(&Authenticator{Store: st})
	srv.InstallRequestContextCarrier()
	srv.InstallEgressPolicy(PolicyHooks{Store: st, Config: cfg, ContextJudge: judge})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return srv
}

func proxyHTTPClient(t *testing.T, srv *Server) *http.Client {
	t.Helper()
	proxyURL, err := url.Parse("http://" + srv.Addr())
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return json.RawMessage(b)
}

type stubRuntimeContextJudge struct {
	judgment runtimepolicy.RuntimeContextJudgment
	err      error
}

func (s *stubRuntimeContextJudge) Judge(context.Context, runtimepolicy.RuntimeContextJudgeRequest) (runtimepolicy.RuntimeContextJudgment, error) {
	return s.judgment, s.err
}
