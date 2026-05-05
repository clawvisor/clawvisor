package scenario

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/clawvisor/clawvisor/internal/e2e/harness"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// Apply installs the scenario fixture into the harness: brings up upstream
// mocks (loading their JSON bodies from disk), seeds a principal, creates
// runtime policy rules, and seeds vault entries.
//
// Returns the seeded principal so the orchestrator can pass user/agent IDs
// to the LLM roles.
func Apply(ctx context.Context, h *harness.Server, sc *Scenario) (*harness.Principal, error) {
	if h == nil || sc == nil {
		return nil, fmt.Errorf("scenario.Apply: harness and scenario are required")
	}
	p, err := h.SeedPrincipal(ctx, sc.AgentName)
	if err != nil {
		return nil, fmt.Errorf("seed principal: %w", err)
	}

	// Auto-allow the harness's in-process Clawvisor API. Scenarios that
	// drive POST /api/tasks, /api/gateway/request, etc. would otherwise
	// fall into runtime review for every call. The API host (already
	// registered as an upstream in harness.Start) is treated as a
	// trusted self-call here.
	if err := h.Store.CreateRuntimePolicyRule(ctx, &store.RuntimePolicyRule{
		ID:      fmt.Sprintf("scn-%s-allow-api", sc.ID),
		UserID:  p.User.ID,
		AgentID: &p.Agent.ID,
		Kind:    "egress",
		Action:  "allow",
		Host:    harness.APIHost,
		Source:  "scenario",
		Reason:  "harness clawvisor API self-call",
		Enabled: true,
	}); err != nil {
		return nil, fmt.Errorf("create harness API allow rule: %w", err)
	}

	for _, up := range sc.Fixture.Upstreams {
		body := []byte(`{}`)
		if up.FixturePath != "" {
			path := up.FixturePath
			if !filepath.IsAbs(path) && sc.Path != "" {
				path = filepath.Join(filepath.Dir(sc.Path), path)
			}
			body, err = os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("load upstream fixture %s: %w", path, err)
			}
		}
		host := up.Host
		text := string(body)
		h.Upstreams.AddJSON(host, http.StatusOK, text)
	}

	for i, rule := range sc.Fixture.Rules {
		if err := h.Store.CreateRuntimePolicyRule(ctx, &store.RuntimePolicyRule{
			ID:      fmt.Sprintf("scn-%s-rule-%d", sc.ID, i),
			UserID:  p.User.ID,
			AgentID: &p.Agent.ID,
			Kind:    rule.Kind,
			Action:  rule.Action,
			Service: rule.Service,
			Host:    rule.Host,
			Method:  rule.Method,
			Path:    rule.Path,
			Reason:  rule.Reason,
			Source:  "scenario",
			Enabled: true,
		}); err != nil {
			return nil, fmt.Errorf("create rule %s: %w", rule.Name, err)
		}
	}

	for _, vs := range sc.Fixture.Vault {
		value := []byte(`{"placeholder":"harness-synthetic"}`)
		if vs.ValueEnv != "" {
			if v := os.Getenv(vs.ValueEnv); v != "" {
				value = []byte(v)
			}
		}
		if err := h.Vault.Set(ctx, p.User.ID, vs.Service, value); err != nil {
			return nil, fmt.Errorf("seed vault %s: %w", vs.Service, err)
		}
	}

	return p, nil
}
