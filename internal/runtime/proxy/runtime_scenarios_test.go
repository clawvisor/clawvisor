package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/clawvisor/clawvisor/internal/store/sqlite"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestRuntimeProxyGlobalEgressPolicyObserveEnforceScenarios(t *testing.T) {
	tests := []struct {
		name               string
		action             string
		observe            bool
		wantStatus         int
		wantEventType      string
		wantDecision       string
		wantOutcome        string
		wantApprovalRecord bool
	}{
		{
			name:          "allow enforce",
			action:        "allow",
			observe:       false,
			wantStatus:    http.StatusOK,
			wantEventType: "runtime.policy.allow_matched",
			wantDecision:  "allow",
			wantOutcome:   "approved",
		},
		{
			name:          "allow observe",
			action:        "allow",
			observe:       true,
			wantStatus:    http.StatusOK,
			wantEventType: "runtime.policy.allow_matched",
			wantDecision:  "allow",
			wantOutcome:   "observed",
		},
		{
			name:          "deny enforce",
			action:        "deny",
			observe:       false,
			wantStatus:    http.StatusForbidden,
			wantEventType: "runtime.policy.deny_matched",
			wantDecision:  "deny",
			wantOutcome:   "blocked",
		},
		{
			name:          "deny observe",
			action:        "deny",
			observe:       true,
			wantStatus:    http.StatusOK,
			wantEventType: "runtime.observe.would_deny",
			wantDecision:  "allow",
			wantOutcome:   "observed",
		},
		{
			name:               "review enforce",
			action:             "review",
			observe:            false,
			wantStatus:         http.StatusForbidden,
			wantEventType:      "runtime.policy.review_matched",
			wantDecision:       "review",
			wantOutcome:        "pending",
			wantApprovalRecord: true,
		},
		{
			name:          "review observe",
			action:        "review",
			observe:       true,
			wantStatus:    http.StatusOK,
			wantEventType: "runtime.observe.would_review",
			wantDecision:  "allow",
			wantOutcome:   "observed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := sqlite.New(ctx, t.TempDir()+"/runtime-global-policy.db")
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

			rule := &store.RuntimePolicyRule{
				ID:      "rule-" + tt.action,
				UserID:  userID,
				Kind:    "egress",
				Action:  tt.action,
				Host:    upstreamURL.Hostname(),
				Method:  http.MethodGet,
				Path:    "/",
				Reason:  "named scenario policy rule",
				Source:  "user",
				Enabled: true,
			}
			if err := st.CreateRuntimePolicyRule(ctx, rule); err != nil {
				t.Fatalf("CreateRuntimePolicyRule: %v", err)
			}

			session := createRuntimeSession(t, st, "session-"+tt.action, userID, agentID, tt.observe)
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
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("expected status %d, got %d body=%s", tt.wantStatus, resp.StatusCode, string(body))
			}

			events, err := st.ListRuntimeEvents(ctx, userID, store.RuntimeEventFilter{SessionID: session.id, Limit: 20})
			if err != nil {
				t.Fatalf("ListRuntimeEvents: %v", err)
			}
			event := findRuntimeEventByType(events, tt.wantEventType)
			if event == nil {
				t.Fatalf("expected event type %q in %+v", tt.wantEventType, events)
			}
			if got := stringOrEmpty(event.Decision); got != tt.wantDecision {
				t.Fatalf("event decision=%q, want %q", got, tt.wantDecision)
			}
			if got := stringOrEmpty(event.Outcome); got != tt.wantOutcome {
				t.Fatalf("event outcome=%q, want %q", got, tt.wantOutcome)
			}
			var meta map[string]any
			if len(event.MetadataJSON) > 0 {
				if err := json.Unmarshal(event.MetadataJSON, &meta); err != nil {
					t.Fatalf("unmarshal event metadata: %v", err)
				}
				if meta["host"] != upstreamURL.Hostname() {
					t.Fatalf("expected event metadata host %q, got %+v", upstreamURL.Hostname(), meta)
				}
			}

			records, err := st.ListPendingApprovalRecords(ctx, userID)
			if err != nil {
				t.Fatalf("ListPendingApprovalRecords: %v", err)
			}
			if tt.wantApprovalRecord && len(records) != 1 {
				t.Fatalf("expected one pending approval record, got %+v", records)
			}
			if tt.wantApprovalRecord {
				rec := records[0]
				if rec.Kind != "request_once" {
					t.Fatalf("approval kind = %q, want request_once", rec.Kind)
				}
				if rec.ResolutionTransport != "consume_one_off_retry" {
					t.Fatalf("approval resolution transport = %q, want consume_one_off_retry", rec.ResolutionTransport)
				}
				var payload RuntimeApprovalPayload
				if err := json.Unmarshal(rec.PayloadJSON, &payload); err != nil {
					t.Fatalf("unmarshal approval payload: %v", err)
				}
				if payload.SessionID != session.id {
					t.Fatalf("approval payload session_id = %q, want %q", payload.SessionID, session.id)
				}
				if payload.AgentID != agentID {
					t.Fatalf("approval payload agent_id = %q, want %q", payload.AgentID, agentID)
				}
				if payload.Method != http.MethodGet {
					t.Fatalf("approval payload method = %q, want %q", payload.Method, http.MethodGet)
				}
				if payload.Host != upstreamURL.Hostname() {
					t.Fatalf("approval payload host = %q, want %q", payload.Host, upstreamURL.Hostname())
				}
				if payload.Path != "/" {
					t.Fatalf("approval payload path = %q, want /", payload.Path)
				}
				if payload.Classification != "request_once" {
					t.Fatalf("approval payload classification = %q, want request_once", payload.Classification)
				}
			}
			if !tt.wantApprovalRecord && len(records) != 0 {
				t.Fatalf("expected no pending approval records, got %+v", records)
			}
		})
	}
}

func TestRuntimeProxyDisabledRulesDoNotShortCircuitReview(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New(ctx, t.TempDir()+"/runtime-disabled-rule.db")
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

	if err := st.CreateRuntimePolicyRule(ctx, &store.RuntimePolicyRule{
		ID:      "rule-disabled-deny",
		UserID:  userID,
		Kind:    "egress",
		Action:  "deny",
		Host:    upstreamURL.Hostname(),
		Method:  http.MethodGet,
		Path:    "/",
		Reason:  "disabled deny rule should not match",
		Source:  "user",
		Enabled: false,
	}); err != nil {
		t.Fatalf("CreateRuntimePolicyRule: %v", err)
	}

	session := createRuntimeSession(t, st, "session-disabled-rule", userID, agentID, false)
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
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected unmatched request to require review, got %d body=%s", resp.StatusCode, string(body))
	}

	events, err := st.ListRuntimeEvents(ctx, userID, store.RuntimeEventFilter{SessionID: session.id, Limit: 20})
	if err != nil {
		t.Fatalf("ListRuntimeEvents: %v", err)
	}
	if findRuntimeEventByType(events, "runtime.policy.deny_matched") != nil {
		t.Fatalf("disabled deny rule should not emit deny_matched: %+v", events)
	}
	if findRuntimeEventByType(events, "runtime.egress.review_required") == nil {
		t.Fatalf("expected normal review-required event, got %+v", events)
	}
}

func findRuntimeEventByType(events []*store.RuntimeEvent, want string) *store.RuntimeEvent {
	for _, event := range events {
		if event.EventType == want {
			return event
		}
	}
	return nil
}
