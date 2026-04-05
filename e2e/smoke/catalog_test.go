package smoke_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSkillCatalog(t *testing.T) {
	env := setup(t)

	// The catalog endpoint returns text/markdown for agent tokens.
	resp := env.agentDo("GET", "/api/skill/catalog", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/markdown") {
		t.Errorf("expected text/markdown content type, got %q", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	catalog := string(body)

	if !strings.Contains(catalog, "Service Catalog") {
		t.Error("catalog does not contain expected header")
	}
	t.Logf("catalog length: %d bytes", len(catalog))
}

func TestSkillCatalogSingleService(t *testing.T) {
	env := setup(t)

	resp := env.agentDo("GET", "/api/skill/catalog?service=github", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		t.Error("expected non-empty catalog detail for github")
	}
	t.Logf("github catalog detail: %d bytes", len(body))
}

func TestServicesList(t *testing.T) {
	env := setup(t)

	resp := env.userDo("GET", "/api/services", nil)
	m := mustStatus(t, resp, http.StatusOK)

	services, ok := m["services"].([]any)
	if !ok {
		t.Fatal("expected services array in response")
	}
	t.Logf("user has %d service(s) configured", len(services))

	for _, raw := range services {
		svc, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id := strOr(svc, "id", "?")
		alias := strOr(svc, "alias", "")
		status := strOr(svc, "status", "?")
		if alias != "" {
			t.Logf("  %s:%s — %s", id, alias, status)
		} else {
			t.Logf("  %s — %s", id, status)
		}
	}
}
