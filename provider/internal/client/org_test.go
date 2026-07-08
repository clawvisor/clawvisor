package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestOrgAdminPaths proves the org bootstrap client methods hit the
// instance-admin /api/admin/* surface (never org-scoped) and carry the right
// method + body, and that the one-time token plaintext is surfaced on create.
func TestOrgAdminPaths(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/admin/orgs":
			var body CreateOrgRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Name == "" || body.Slug == "" {
				t.Errorf("create org body missing fields: %+v", body)
			}
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":"org_1","name":"Acme","slug":"acme","tier":"enterprise"}`))
		case r.Method == "GET" && r.URL.Path == "/api/admin/orgs/org_1":
			_, _ = w.Write([]byte(`{"id":"org_1","name":"Acme","slug":"acme","tier":"enterprise"}`))
		case r.Method == "DELETE" && r.URL.Path == "/api/admin/orgs/org_1":
			w.WriteHeader(204)
		case r.Method == "POST" && r.URL.Path == "/api/admin/orgs/org_1/tokens":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":"tk_1","name":"terraform","role":"admin","token_prefix":"cvot_abc","token":"cvot_SECRET"}`))
		case r.Method == "GET" && r.URL.Path == "/api/admin/orgs/org_1/tokens":
			_, _ = w.Write([]byte(`{"tokens":[{"id":"tk_1","name":"terraform","role":"admin","token_prefix":"cvot_abc"}]}`))
		case r.Method == "DELETE" && r.URL.Path == "/api/admin/orgs/org_1/tokens/tk_1":
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	// Note: NO org_id — the admin surface is instance-scoped.
	c := New(srv.URL, "cvat_"+padToken(), "", srv.Client())
	ctx := context.Background()

	org, err := c.CreateOrg(ctx, CreateOrgRequest{Name: "Acme", Slug: "acme"})
	if err != nil || org.ID != "org_1" || org.Tier != "enterprise" {
		t.Fatalf("CreateOrg: %+v err=%v (path %s %s)", org, err, gotMethod, gotPath)
	}

	if got, err := c.GetOrg(ctx, "org_1"); err != nil || got.Slug != "acme" {
		t.Fatalf("GetOrg: %+v err=%v", got, err)
	}

	tok, err := c.CreateOrgTokenAdmin(ctx, "org_1", CreateOrgTokenRequest{Name: "terraform"})
	if err != nil || tok.Token != "cvot_SECRET" || tok.Role != "admin" {
		t.Fatalf("CreateOrgTokenAdmin: %+v err=%v", tok, err)
	}

	toks, err := c.ListOrgTokens(ctx, "org_1")
	if err != nil || len(toks) != 1 || toks[0].ID != "tk_1" {
		t.Fatalf("ListOrgTokens: %+v err=%v", toks, err)
	}
	// The list response never carries the plaintext.
	if toks[0].Token != "" {
		t.Fatalf("ListOrgTokens leaked plaintext: %q", toks[0].Token)
	}

	if err := c.RevokeOrgToken(ctx, "org_1", "tk_1"); err != nil {
		t.Fatalf("RevokeOrgToken: %v", err)
	}
	if err := c.DeleteOrg(ctx, "org_1"); err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}
}

// TestGetOrgNotFound proves an absent org maps to a 404 *APIError so the
// resource layer removes it from state.
func TestGetOrgNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"org not found","code":"NOT_FOUND"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "cvat_"+padToken(), "", srv.Client())
	if _, err := c.GetOrg(context.Background(), "org_x"); !NotFound(err) {
		t.Fatalf("GetOrg: want 404 NotFound, got %v", err)
	}
}

// TestCreateOrgTokenExpiry proves expires_in_days is sent only when set.
func TestCreateOrgTokenExpiry(t *testing.T) {
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"tk_1","token":"cvot_x","role":"admin"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "cvat_"+padToken(), "", srv.Client())
	days := 30
	if _, err := c.CreateOrgTokenAdmin(context.Background(), "org_1", CreateOrgTokenRequest{Name: "tf", ExpiresInDays: &days}); err != nil {
		t.Fatal(err)
	}
	if raw["expires_in_days"].(float64) != 30 {
		t.Fatalf("expires_in_days not sent: %+v", raw)
	}
}
