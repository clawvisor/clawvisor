package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPathScopeRouting proves the single path-scoping seam (PRD §8): with an
// org_id set the governance routes are org-scoped; without it they are
// instance-scoped. Resources never concatenate paths themselves.
func TestPathScopeRouting(t *testing.T) {
	cases := []struct {
		name  string
		orgID string
		sub   string
		want  string
	}{
		{"instance model_policy", "", "model_policy", "/api/governance/model_policy"},
		{"instance spend window", "", "spend_caps/daily", "/api/governance/spend_caps/daily"},
		{"org model_policy", "org_abc123", "model_policy", "/api/orgs/org_abc123/governance/model_policy"},
		{"org content id", "org_abc123", "content_policies/cp_1", "/api/orgs/org_abc123/governance/content_policies/cp_1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps := PathScope{OrgID: tc.orgID}
			if got := ps.Governance(tc.sub); got != tc.want {
				t.Fatalf("Governance(%q) with orgID=%q = %q, want %q", tc.sub, tc.orgID, got, tc.want)
			}
		})
	}
}

// TestClientErrorMapping proves non-2xx responses become typed *APIError
// values carrying the server's status, `error`, and `code` fields, so the
// resource layer can map them to framework diagnostics without re-parsing.

// TestPathScopeOrg covers the org-scoped (non-governance) path builder used by
// clawvisor_org_settings.
func TestPathScopeOrg(t *testing.T) {
	ps := PathScope{OrgID: "org_abc123"}
	if got, want := ps.Org("settings"), "/api/orgs/org_abc123/settings"; got != want {
		t.Fatalf("Org(settings) = %q, want %q", got, want)
	}
	if got, want := ps.Org("sso"), "/api/orgs/org_abc123/sso"; got != want {
		t.Fatalf("Org(sso) = %q, want %q", got, want)
	}
}

func TestClientErrorMapping(t *testing.T) {
	cases := []struct {
		status  int
		body    string
		wantMsg string
		want404 bool
	}{
		{http.StatusNotFound, `{"error":"agent not found","code":"NOT_FOUND"}`, "agent not found", true},
		{http.StatusUnauthorized, `{"error":"token revoked","code":"UNAUTHORIZED"}`, "token revoked", false},
		{http.StatusConflict, `{"error":"already exists"}`, "already exists", false},
		{http.StatusUnprocessableEntity, `{"error":"model id must be provider-qualified"}`, "model id must be provider-qualified", false},
		{http.StatusInternalServerError, `not json at all`, "not json at all", false},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		c := New(srv.URL, "cvat_"+padToken(), "", srv.Client())
		err := c.do(context.Background(), "GET", "/api/agents", nil, nil)
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: expected error, got nil", tc.status)
		}
		ae, ok := err.(*APIError)
		if !ok {
			t.Fatalf("status %d: expected *APIError, got %T", tc.status, err)
		}
		if ae.StatusCode != tc.status {
			t.Errorf("status: got %d, want %d", ae.StatusCode, tc.status)
		}
		if ae.Message != tc.wantMsg {
			t.Errorf("message: got %q, want %q", ae.Message, tc.wantMsg)
		}
		if NotFound(err) != tc.want404 {
			t.Errorf("NotFound: got %v, want %v", NotFound(err), tc.want404)
		}
	}
}

// TestClientSetsBearerAuth proves every request carries the API token as a
// Bearer credential (spec 05 header), and never leaks it elsewhere.
func TestClientSetsBearerAuth(t *testing.T) {
	const token = "cvat_" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c := New(srv.URL, token, "", srv.Client())
	if _, err := c.ListAgents(context.Background()); err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if want := "Bearer " + token; gotAuth != want {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, want)
	}
}

// TestFeaturesMissingFieldIsFalse proves an absent capability field decodes as
// false — the fail-fast contract (a server too old to advertise a capability
// is treated as lacking it).
func TestFeaturesMissingFieldIsFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reports api_tokens only; local_governance is absent from the body.
		_, _ = w.Write([]byte(`{"api_tokens":true}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "cvat_"+padToken(), "", srv.Client())
	f, err := c.Features(context.Background())
	if err != nil {
		t.Fatalf("Features: %v", err)
	}
	if !f.Has(CapabilityAPITokens) {
		t.Errorf("expected api_tokens capability present")
	}
	if f.Has(CapabilityLocalGovernance) {
		t.Errorf("expected local_governance to decode as false when absent")
	}
}

func padToken() string { return "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" }
